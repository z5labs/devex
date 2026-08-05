package main

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dagger/go/internal/dagger"
)

// Spdx renders an SPDX 2.3 JSON document describing the module graph
// compiled into binary.
//
// **Why 2.3 and not 3.0.** The version is chosen for what consumers
// ingest rather than left to whatever the library defaults to. SPDX 2.3
// is the revision behind ISO/IEC 5962's successor line that GitHub's
// dependency graph, Dependency-Track, Grype, Trivy and the CISA/NTIA
// minimum-elements tooling all read today; 3.0 changes the serialization
// wholesale and support for it is still thin. A document nothing can
// parse is not an SBOM.
//
// **The subject is the binary, not the tree.** The component list is read
// out of the compiled artifact with debug/buildinfo, so it names the
// modules that were actually linked in — not everything go.mod happens to
// require. source is an *input* and not the subject: a Go binary embeds
// module paths, versions and hashes but no licence text, so the licences
// have to be resolved from the module cache the source pins.
//
// **Licences are declared and concluded separately.** Licence
// identification is a classifier, and a classifier reports coverage
// rather than a verdict. The classifier's best match is always recorded
// as the declared licence; it is only promoted to the concluded licence
// when the match covers essentially the whole file. Anything less
// concludes NOASSERTION, so a low-confidence match cannot be mistaken
// downstream for an established one.
//
// +cache="session"
func (g *Go) Spdx(
	ctx context.Context,
	// The compiled Go binary the document describes.
	binary *dagger.File,
	// The source tree the binary was built from. Used to resolve the
	// licence of each linked module; never used to enumerate components.
	source *dagger.Directory,
) (*dagger.File, error) {
	graph, err := g.resolveGraph(ctx, binary, source)
	if err != nil {
		return nil, err
	}
	doc, err := graph.spdxDocument()
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile(graph.BinaryName+".spdx.json", doc)
}

// CycloneDx renders a CycloneDX 1.6 JSON document describing the module
// graph compiled into binary.
//
// **Why 1.6.** 1.6 is the current release and the one Dependency-Track,
// Grype and Trivy consume; it is also the first to model a component's
// licence acknowledgement, which is what lets a low-confidence classifier
// match be published as "declared" rather than silently asserted.
//
// The component set, the versions and the licences are identical to what
// Spdx emits for the same inputs: both render from one resolution of the
// graph, so the two documents cannot disagree about what shipped. See
// Spdx for how the graph is resolved and how licence confidence is
// handled.
//
// +cache="session"
func (g *Go) CycloneDx(
	ctx context.Context,
	// The compiled Go binary the document describes.
	binary *dagger.File,
	// The source tree the binary was built from. Used to resolve the
	// licence of each linked module; never used to enumerate components.
	source *dagger.Directory,
) (*dagger.File, error) {
	graph, err := g.resolveGraph(ctx, binary, source)
	if err != nil {
		return nil, err
	}
	doc, err := graph.cycloneDxDocument()
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile(graph.BinaryName+".cdx.json", doc)
}

// component is one module linked into the binary.
type component struct {
	// Path is the module path as the binary records it. For a replaced
	// module this is the replacement's path, because that is the code
	// that shipped.
	Path string
	// Version is the module version, or "(devel)" for a main module
	// built outside a tagged commit.
	Version string
	// Sum is the module's h1: checksum where the binary carries one.
	Sum string
	// ReplacedFrom names the module this one was substituted for, empty
	// when nothing was replaced. The document records the substitution
	// rather than hiding it: a consumer matching advisories against the
	// original path has to know it is not what was built.
	ReplacedFrom string
	// License is what the classifier made of the module's licence file.
	License license
}

// license is a classifier result, kept in the shape SPDX models it: what
// the artifact says about itself, and what this module is prepared to
// conclude from that.
type license struct {
	// Declared is the classifier's best match, or NOASSERTION when no
	// licence file was found or nothing matched.
	Declared string
	// Concluded is Declared when the match covered essentially the whole
	// file, and NOASSERTION otherwise.
	Concluded string
	// Coverage is the fraction of the licence file the best match
	// accounted for, 0 when there was no match.
	Coverage float64
}

// noAssertion is SPDX's spelling of "this document does not say", and is
// used for CycloneDX's absent licence too so the two documents express
// the same uncertainty.
const noAssertion = "NOASSERTION"

// licenseCoverageThreshold is the fraction of a licence file a match has
// to cover before this module will conclude it. Below it the match is
// still published as the declared licence — dropping it would throw away
// the only signal there is — but nothing downstream may treat it as
// established.
//
// 0.90 rather than a round 1.0 because real licence files carry a
// copyright line, an appended notice or a reformatted preamble that no
// classifier attributes to the licence text itself.
const licenseCoverageThreshold = 0.90

// graph is one resolution of what the binary contains. Both documents
// render from a value of this type and neither resolves anything of its
// own, which is what makes the two consistent by construction rather
// than by review.
type graph struct {
	// BinaryName is the artifact's file name, used to name the document
	// and the subject package.
	BinaryName string
	// BinarySha256 is the hex digest of the artifact's bytes. It is what
	// ties the document to a specific build.
	BinarySha256 string
	// MainPath and MainVersion identify the main module.
	MainPath    string
	MainVersion string
	// GoVersion is the toolchain that produced the binary.
	GoVersion string
	// Components are the linked modules, ordered by path then version.
	Components []component
	// Created is the document timestamp, resolved once so both documents
	// can be produced from one graph without disagreeing about it.
	Created time.Time
}

// resolveGraph reads the module graph out of the compiled binary and
// joins it against the licences resolved from source.
//
// The graph comes from debug/buildinfo rather than from `go version -m`
// output: the command prints a format meant for people, and a parser for
// it is a second implementation of something the standard library
// already does against the same section of the same file.
func (g *Go) resolveGraph(ctx context.Context, binary *dagger.File, source *dagger.Directory) (*graph, error) {
	if binary == nil {
		return nil, fmt.Errorf("binary is required")
	}
	if source == nil {
		return nil, fmt.Errorf("source is required to resolve module licences")
	}

	work, err := os.MkdirTemp("", "go-sbom-*")
	if err != nil {
		return nil, fmt.Errorf("create work dir: %v", err)
	}
	defer os.RemoveAll(work)

	name, err := binary.Name(ctx)
	if err != nil {
		return nil, fmt.Errorf("read binary name: %v", err)
	}
	if name == "" {
		name = "binary"
	}
	path := filepath.Join(work, "artifact")
	if _, err := binary.Export(ctx, path); err != nil {
		return nil, fmt.Errorf("export binary: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is this function's own temp dir
	if err != nil {
		return nil, fmt.Errorf("read binary: %v", err)
	}
	sum := sha256.Sum256(data)

	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read build info from %s: %v", name, err)
	}

	licenses, err := g.resolveLicenses(ctx, source, work)
	if err != nil {
		return nil, err
	}

	out := &graph{
		BinaryName:   name,
		BinarySha256: hex.EncodeToString(sum[:]),
		MainPath:     info.Main.Path,
		MainVersion:  info.Main.Version,
		GoVersion:    info.GoVersion,
		Created:      time.Now().UTC(),
	}
	if out.MainPath == "" {
		out.MainPath = info.Path
	}
	for _, dep := range info.Deps {
		mod := dep
		replacedFrom := ""
		if mod.Replace != nil {
			replacedFrom = mod.Path
			mod = mod.Replace
		}
		out.Components = append(out.Components, component{
			Path:         mod.Path,
			Version:      mod.Version,
			Sum:          mod.Sum,
			ReplacedFrom: replacedFrom,
			License:      licenses.lookup(mod.Path, mod.Version),
		})
	}
	sort.Slice(out.Components, func(i, j int) bool {
		if out.Components[i].Path != out.Components[j].Path {
			return out.Components[i].Path < out.Components[j].Path
		}
		return out.Components[i].Version < out.Components[j].Version
	})
	return out, nil
}

// purl renders a component as a Package URL. Go's purl type spells the
// module path as namespace + name, and the whole path is lowercased
// because the spec says a golang purl is case-insensitive and canonically
// lowercase.
func (c component) purl() string {
	return "pkg:golang/" + strings.ToLower(c.Path) + "@" + c.Version
}

// spdxIdentifier is the document-unique element id for a component. The
// digest of the module coordinate rather than the coordinate itself: an
// SPDXRef may only carry letters, digits, "." and "-", and a module path
// carries "/" and "_" routinely.
func (c component) spdxIdentifier() string {
	sum := sha256.Sum256([]byte(c.Path + "@" + c.Version))
	return "SPDXRef-Package-" + hex.EncodeToString(sum[:8])
}

// mustJSON marshals v indented, which is what both writers emit. The
// documents are read by people as often as by tools and a diff of two
// one-line JSON files is unusable.
func mustJSON(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode document: %v", err)
	}
	return append(out, '\n'), nil
}
