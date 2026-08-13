package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
	spdxjson "github.com/spdx/tools-golang/json"
	"github.com/spdx/tools-golang/spdx/v2/common"
	"github.com/spdx/tools-golang/spdx/v2/v2_3"

	"dagger/z-5-labs/internal/dagger"
)

// The subject of a published document is the image, not the binary.
//
// # Why this is not what devex#330 built
//
// #330 built the documents from the compiled binary's module graph, and that
// was complete: the image was one static binary on scratch and an
// entrypoint, so the binary *was* the image. The moment anything else can
// enter the image — a caller's file or directory (devex#392), another app's
// payload (devex#401) — a document about the binary describes some of what
// is inside and no consumer can tell which. That failure is not a lie and it
// is not detectable, which is what makes it worth closing structurally
// rather than by convention.
//
// # The shape, and the constraint that fixed it
//
// One criterion of #330 survives and decides everything here: no shared
// `sbom` module, and no generic scanner above the module or inside it. So an
// image-wide document cannot be recovered by scanning the built image — it
// has to be *assembled* from per-contribution documents. That is not a
// workaround. A scratch image has no package manager metadata to read, so a
// scanner would find almost nothing, and the contributor is the only party
// that knows what it contributed.
//
// The division of labour therefore becomes three concerns rather than two:
//
//   - the ecosystem chain **produces** a document about what it built
//     (`dag.Go().Spdx`, and its siblings in java and zig);
//   - this module **assembles** those documents into one document per
//     format per platform, at publish time, when the digest is known;
//   - the `oci` module **attaches** bytes to a digest, still without ever
//     learning that any of them is an SBOM.
//
// # The contribution document is SPDX 2.3 JSON, and there is one of it
//
// This is the type devex#392's helpers take — `WithFile(path, file,
// document)` — and it is one file rather than a pair. Two reasons, and the
// second is the load-bearing one:
//
//   - A caller shipping a CA bundle should have to describe it once. Asking
//     for two formats of the same statement doubles the burden on the party
//     least equipped to carry it, which is how you get worthless documents
//     supplied to satisfy a signature.
//   - Both published formats then render from **one** resolution of the
//     image, exactly as `daggerverse/go` renders both of its from one
//     resolution of a module graph. Merging N documents into two formats
//     independently would be a new place for the two to disagree, and
//     nothing downstream could adjudicate. Here they cannot disagree,
//     because there is nothing to disagree about: imageBom is resolved once
//     and each renderer is a pure function of it.
//
// SPDX rather than CycloneDX because SPDX has NOASSERTION — a first-class
// way to say "this document does not say" — and content with no ecosystem
// is mostly a statement about what is not known. CycloneDX has no spelling
// for it, so a document for a CA bundle would have to either omit the
// licence or invent one.
//
// # What a consumer fetches: the image document replaces the contributions
//
// A published digest carries exactly two documents per platform, the SPDX
// and the CycloneDX for the whole image. The per-contribution documents are
// *inputs* to assembly and are not attached.
//
// The alternative — attach both the assembled document and every
// contribution's — was rejected. devex#398 settled discovery as referrers
// alone, and a consumer selects a referrer by artifact type; several
// documents of one artifact type on one digest give them no way to tell
// which is the complete one, and picking wrong is silent. Attribution is not
// lost by replacing: it moves inside the document, where the image CONTAINS
// each contribution's subject and each subject DEPENDS_ON what it was made
// of, which is a relationship a consumer can follow rather than a filename
// convention they have to know.

// noAssertion is SPDX's spelling of "this document does not say". CycloneDX
// has no equivalent, so a component carrying it simply gets no licence
// entry — the two documents express the same uncertainty rather than one of
// them inventing a value.
const noAssertion = "NOASSERTION"

// The document metadata this module stamps. The SPDX revision is 2.3 and
// the CycloneDX revision is 1.6 for the reasons daggerverse/go records on
// its own generators: those are the revisions consumers ingest today, and
// documents from the two modules have to stay comparable.
const (
	spdxVersion             = "SPDX-2.3"
	spdxDataLicense         = "CC0-1.0"
	cycloneDxSpecVersion    = cdx.SpecVersion1_6
	documentNamespaceBase   = "https://z5labs.github.io/devex/spdxdocs/"
	imageDocumentCreator    = "daggerverse-z5labs-image-sbom"
	creatorOrganization     = "z5labs"
	spdxImageID             = "SPDXRef-Image"
	spdxDescribesFromDocDoc = "DOCUMENT"
)

// bomFile is one file inside a described package.
//
// Files are carried rather than summarized because the promise this whole
// file exists to keep is that every byte in the image is accounted for. A
// directory contributed as one package with no file list would satisfy the
// letter of that and not the substance — the same failure one level down.
type bomFile struct {
	// Path is the file's path as the contributing document named it.
	Path string
	// Sha1 is the SHA-1 SPDX requires of an analyzed file. It is not a
	// security property here and is not used as one; SPDX 2.3 mandates it
	// and computes its package verification code from it.
	Sha1 string
	// Sha256 is the digest anything else should use.
	Sha256 string
}

// bomPackage is one described thing: a contribution's subject, or a
// component beneath one.
type bomPackage struct {
	Name             string
	Version          string
	Purl             string
	Sha256           string
	Supplier         string
	Purpose          string
	LicenseDeclared  string
	LicenseConcluded string
	LicenseComment   string
	SourceInfo       string
	Files            []bomFile
}

// key identifies a package for de-duplication across contributions. Two
// contributions that link the same module name the same component, and an
// image document listing it twice would inflate every count a consumer
// takes off it.
//
// A purl is the identity where there is one, because it is the coordinate
// an advisory database matches on. Content with no ecosystem has no purl,
// so it falls back to what it does have: a name, a version and the digest
// of its bytes.
func (p bomPackage) key() string {
	if p.Purl != "" {
		return "purl:" + p.Purl
	}
	return "id:" + p.Name + "@" + p.Version + "#" + p.Sha256
}

// elementID is the package's SPDXRef. It is the digest of the key rather
// than the key itself: an SPDXRef may carry only letters, digits, "." and
// "-", and a module path or a filesystem path carries "/" and "_" routinely.
func (p bomPackage) elementID() string {
	sum := sha256.Sum256([]byte(p.key()))
	return "SPDXRef-Package-" + hex.EncodeToString(sum[:8])
}

// bomRef is the CycloneDX identity. It is the purl where there is one,
// because that is the coordinate everything downstream matches on, and a
// urn derived from the same key otherwise — content with no ecosystem has
// no purl, and inventing one would put a coordinate in a field an advisory
// database will try to resolve.
func (p bomPackage) bomRef() string {
	if p.Purl != "" {
		return p.Purl
	}
	return "urn:z5labs:component:" + strings.TrimPrefix(p.elementID(), "SPDXRef-Package-")
}

// contributionBom is one thing that entered the image, and what it was
// itself made of.
type contributionBom struct {
	// Name is what the contribution is called in an error message: the
	// binary's name, or the path a file was contributed at.
	Name string
	// Subject is what entered the image.
	Subject bomPackage
	// Components are what the subject was made of — a binary's linked
	// modules, and nothing at all for content with no ecosystem.
	Components []bomPackage
}

// imageBom is one resolution of everything one platform's image contains.
// Both documents render from a value of this type and neither resolves
// anything of its own, which is what makes them consistent by construction
// rather than by review. It is the same rule daggerverse/go applies to a
// module graph, one level up.
type imageBom struct {
	Repository    string
	Digest        string
	Version       string
	Platform      string
	Created       time.Time
	Contributions []contributionBom
}

// variantBom is a variant's contributions, parsed and validated, waiting
// for the digest that will become their subject.
//
// The split exists because the two halves fail at different moments. Reading
// and parsing a contribution document is fallible and has nothing to do with
// the registry, so it happens before the first byte is pushed — the same
// reasoning newSigner already applies to provenance. Naming the subject
// needs the published digest, which does not exist until after the push.
type variantBom struct {
	Platform      dagger.Platform
	Contributions []contributionBom
}

// resolveContributions reads and parses every variant's contribution
// documents, before anything is pushed.
//
// A variant carrying no contributions is an error rather than an image with
// an empty document. An SBOM that accounts for nothing is exactly the
// artifact this whole file exists to prevent: it is well-formed, it is
// attached, and it is indistinguishable from a complete one.
func (a *App) resolveContributions(ctx context.Context) ([]variantBom, error) {
	out := make([]variantBom, 0, len(a.Variants))
	// One cache for the whole publish. A file or directory contributed once is
	// carried by every variant, so without it the same bytes are exported and
	// hashed once per platform; see digestCache.
	seen := digestCache{}
	for _, v := range a.Variants {
		if len(v.Contributions) == 0 {
			return nil, fmt.Errorf(
				"the %s image carries no contribution documents, so no document describing it can be assembled; "+
					"every byte that enters an image arrives with one", v.Platform)
		}
		parsed := make([]contributionBom, 0, len(v.Contributions))
		for _, c := range v.Contributions {
			// The whole seam devex#392 builds on is "bytes arrive described
			// or they do not arrive", and an absent document is the shape a
			// caller who left the argument out produces. It is refused here
			// rather than dereferenced, so the failure names the
			// contribution instead of arriving as a nil dereference from
			// inside the client.
			if c.File == nil {
				return nil, fmt.Errorf(
					"%s entered the %s image with no document describing it; every byte that enters an image arrives with one",
					c.Name, v.Platform)
			}
			raw, err := c.File.Contents(ctx)
			if err != nil {
				return nil, fmt.Errorf("read the document describing %s in the %s image: %v", c.Name, v.Platform, err)
			}
			bom, err := parseContribution(c.Name, []byte(raw))
			if err != nil {
				return nil, fmt.Errorf("the document describing %s in the %s image: %v", c.Name, v.Platform, err)
			}
			// The document is checked against the bytes it claims to describe
			// here, and only here. A document naming some other artifact's
			// digest is well-formed, attaches cleanly and is
			// indistinguishable from a right one once published, so it has to
			// be refused before the push rather than reported afterwards.
			// digest.go records what the check does and does not establish,
			// and why it lives at publish time rather than at the
			// contributing call.
			if err := verifyContributionDigest(ctx, seen, c, bom.Subject); err != nil {
				// The check reads the content as well as the document, so it
				// has two ways to fail and they are about different things.
				// A refused entry is a fault in what was contributed — no
				// document could have made that tree publishable — and
				// announcing it as a fault of the document would send the
				// reader to rewrite one that is fine.
				var bad *unsupportedEntry
				if errors.As(err, &bad) {
					return nil, fmt.Errorf("%s in the %s image: %v", c.Name, v.Platform, err)
				}
				return nil, fmt.Errorf("the document describing %s in the %s image %v", c.Name, v.Platform, err)
			}
			parsed = append(parsed, *bom)
		}
		out = append(out, variantBom{Platform: v.Platform, Contributions: parsed})
	}
	return out, nil
}

// parseContribution reads one contribution's SPDX 2.3 document.
//
// The document has to describe exactly one thing. SPDX allows a document to
// describe several, and a contribution that did would leave "what entered
// the image here" undecidable — so it is refused with a message saying so
// rather than resolved by picking the first.
func parseContribution(name string, raw []byte) (*contributionBom, error) {
	doc, err := spdxjson.Read(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("is not a readable SPDX %s JSON document: %v", spdxVersion, err)
	}
	subjectID, err := describedElement(doc)
	if err != nil {
		return nil, err
	}
	byID := make(map[common.ElementID]*v2_3.Package, len(doc.Packages))
	for _, pkg := range doc.Packages {
		byID[pkg.PackageSPDXIdentifier] = pkg
	}
	files, err := filesByOwner(doc)
	if err != nil {
		return nil, err
	}
	subject, ok := byID[subjectID]
	if !ok {
		return nil, fmt.Errorf("describes %s, which is not a package in it", subjectID)
	}
	out := &contributionBom{Name: name, Subject: packageFromSpdx(subject, files)}
	// Ordered by identifier so an assembled document is a pure function of
	// its inputs. tools-golang preserves document order, but a component
	// list that depended on it would make two renderings of one input
	// differ the first time a producer reordered its packages.
	ids := make([]common.ElementID, 0, len(doc.Packages))
	for id := range byID {
		if id != subjectID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		out.Components = append(out.Components, packageFromSpdx(byID[id], files))
	}
	return out, nil
}

// filesByOwner groups a document's file elements under the package that
// CONTAINS each of them.
//
// A CONTAINS whose right-hand side is a package rather than a file is left
// alone: that is how an assembled image document says what entered the
// image, and an assembled document is itself a legal contribution — which
// is how devex#401 composes one app's payload into another's image.
func filesByOwner(doc *v2_3.Document) (map[common.ElementID][]bomFile, error) {
	elements := make(map[common.ElementID]*v2_3.File, len(doc.Files))
	for _, f := range doc.Files {
		elements[f.FileSPDXIdentifier] = f
	}
	out := make(map[common.ElementID][]bomFile)
	for _, rel := range doc.Relationships {
		if !strings.EqualFold(rel.Relationship, "CONTAINS") {
			continue
		}
		f, ok := elements[rel.RefB.ElementRefID]
		if !ok {
			continue
		}
		file := bomFile{Path: f.FileName}
		for _, sum := range f.Checksums {
			switch sum.Algorithm {
			case common.SHA1:
				file.Sha1 = sum.Value
			case common.SHA256:
				file.Sha256 = sum.Value
			}
		}
		// SPDX 2.3 requires a SHA-1 on every analyzed file, and the package
		// verification code is defined over exactly those. A file without
		// one is refused rather than carried: a code computed over the
		// subset that had one is a *wrong* value rather than an absent one,
		// and a validator recomputing it disagrees with no way to tell why.
		if file.Sha1 == "" {
			return nil, fmt.Errorf(
				"the file %q carries no SHA-1 checksum, which SPDX %s requires of an analyzed file and computes the package verification code from",
				file.Path, spdxVersion)
		}
		out[rel.RefA.ElementRefID] = append(out[rel.RefA.ElementRefID], file)
	}
	return out, nil
}

// describedElement finds the one element the document is about.
func describedElement(doc *v2_3.Document) (common.ElementID, error) {
	var found []common.ElementID
	for _, rel := range doc.Relationships {
		switch strings.ToUpper(rel.Relationship) {
		case "DESCRIBES":
			if rel.RefA.ElementRefID == spdxDescribesFromDocDoc {
				found = append(found, rel.RefB.ElementRefID)
			}
		case "DESCRIBED_BY":
			if rel.RefB.ElementRefID == spdxDescribesFromDocDoc {
				found = append(found, rel.RefA.ElementRefID)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("declares no DESCRIBES relationship, so nothing in it says what entered the image")
	default:
		return "", fmt.Errorf("describes %d elements; a contribution document describes exactly one thing", len(found))
	}
}

// packageFromSpdx maps one SPDX package onto the internal model.
func packageFromSpdx(pkg *v2_3.Package, files map[common.ElementID][]bomFile) bomPackage {
	out := bomPackage{
		Name:             pkg.PackageName,
		Version:          pkg.PackageVersion,
		Purpose:          pkg.PrimaryPackagePurpose,
		LicenseDeclared:  orNoAssertion(pkg.PackageLicenseDeclared),
		LicenseConcluded: orNoAssertion(pkg.PackageLicenseConcluded),
		LicenseComment:   pkg.PackageLicenseComments,
		SourceInfo:       pkg.PackageSourceInfo,
		Supplier:         noAssertion,
	}
	if pkg.PackageSupplier != nil && pkg.PackageSupplier.Supplier != "" {
		out.Supplier = pkg.PackageSupplier.Supplier
	}
	for _, ref := range pkg.PackageExternalReferences {
		if strings.EqualFold(ref.RefType, "purl") {
			out.Purl = ref.Locator
			break
		}
	}
	for _, sum := range pkg.PackageChecksums {
		if sum.Algorithm == common.SHA256 {
			out.Sha256 = sum.Value
			break
		}
	}
	out.Files = append(out.Files, files[pkg.PackageSPDXIdentifier]...)
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out
}

func orNoAssertion(s string) string {
	if strings.TrimSpace(s) == "" {
		return noAssertion
	}
	return s
}

// assemble turns one variant's parsed contributions into the resolution
// both documents render from, anchored to the digest that was published.
func (vb variantBom) assemble(repository, digest, version string, created time.Time) *imageBom {
	return &imageBom{
		Repository:    repository,
		Digest:        digest,
		Version:       version,
		Platform:      string(vb.Platform),
		Created:       created.UTC(),
		Contributions: vb.Contributions,
	}
}

// imagePackage is the document's subject: the image that was published,
// named by its digest.
//
// This is the criterion that moved. The subject used to be a built artifact
// — a file with a checksum, which a consumer holding a pulled image had to
// take on trust was the same thing. Naming the digest makes the document
// checkable against what they pulled, and it is why assembly happens after
// the push rather than at build time.
func (ib *imageBom) imagePackage() bomPackage {
	version := ib.Version
	if version == "" {
		version = noAssertion
	}
	return bomPackage{
		Name:             ib.Repository,
		Version:          version,
		Purl:             ib.imagePurl(),
		Sha256:           digestHex(ib.Digest),
		Supplier:         creatorOrganization,
		Purpose:          "CONTAINER",
		LicenseDeclared:  noAssertion,
		LicenseConcluded: noAssertion,
		SourceInfo:       "the " + ib.Platform + " image published as " + ib.Repository + "@" + ib.Digest,
	}
}

// imagePurl renders the published image as a Package URL. The oci type
// takes the repository's last segment as the name and the digest as the
// version, with the full repository carried in a qualifier, because a purl
// name may not contain a "/".
func (ib *imageBom) imagePurl() string {
	name := basenameAfterSlash(ib.Repository)
	purl := "pkg:oci/" + strings.ToLower(name) + "@" + ib.Digest
	qualifiers := []string{"repository_url=" + ib.Repository}
	if ib.Platform != "" {
		qualifiers = append(qualifiers, "platform="+ib.Platform)
	}
	return purl + "?" + strings.Join(qualifiers, "&")
}

// digestHex is the hex half of a "sha256:<hex>" digest, which is what a
// checksum field wants. A digest in any other algorithm is not a SHA-256,
// and comes back empty rather than mangled into one — validate refuses the
// whole assembly on that, rather than letting it degrade into a document
// with no checksum.
func digestHex(digest string) string {
	hexPart, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return ""
	}
	return hexPart
}

// validate refuses an assembly that cannot produce a checkable document.
//
// A subject with no checksum is the failure this whole file is written to
// prevent, one level up: the document still names the image, still attaches
// and still parses, and has no verifiable link back to the bytes a consumer
// pulled. Registries can serve digests in other algorithms, so the answer is
// to fail rather than to assume they will not.
func (ib *imageBom) validate() error {
	if digestHex(ib.Digest) == "" {
		return fmt.Errorf(
			"the published digest %q is not a sha256 digest, so nothing in the document could name the bytes it describes",
			ib.Digest)
	}
	return nil
}

// resolution is one image's packages, de-duplicated and in a stable order,
// with the dependency graph between them.
//
// Both renderers take a value of this type and neither derives anything of
// its own from the contributions, which is the mechanism behind "the two
// formats cannot disagree" — there is one component set, one lookup table
// and one graph, and the formats differ only in how they spell them.
type resolution struct {
	// Subjects are the things that entered the image, in contribution
	// order.
	Subjects []bomPackage
	// Components are what those were made of, ordered by key.
	Components []bomPackage
	// ByKey resolves any key in DependsOn, subject or component alike.
	ByKey map[string]bomPackage
	// DependsOn maps a subject's key to the keys it was built from.
	DependsOn map[string][]string
}

// packages resolves the image once.
//
// The subject keys are collected in a first pass, before anything is
// classified. Doing it in one pass makes de-duplication order dependent in a
// way that silently loses a contribution: where one contribution links a
// module that a *later* contribution is the subject of — one app shipping
// another app's library, which is exactly what devex#401's composition
// produces — a single seen set claims that key as a component, and the later
// subject is then never emitted as one. The image would not CONTAIN it and
// its own DEPENDS_ON edges would be dropped with it, and swapping the two
// contributions would change the document. A thing that entered the image is
// a subject wherever else it also appears.
func (ib *imageBom) packages() *resolution {
	isSubject := make(map[string]bool, len(ib.Contributions))
	for _, c := range ib.Contributions {
		isSubject[c.Subject.key()] = true
	}

	out := &resolution{
		ByKey:     make(map[string]bomPackage),
		DependsOn: make(map[string][]string),
	}
	seen := make(map[string]bool)
	for _, c := range ib.Contributions {
		subjectKey := c.Subject.key()
		if !seen[subjectKey] {
			seen[subjectKey] = true
			out.ByKey[subjectKey] = c.Subject
			out.Subjects = append(out.Subjects, c.Subject)
		}
		refs := make([]string, 0, len(c.Components))
		for _, comp := range c.Components {
			key := comp.key()
			// A contribution that lists itself among its own components —
			// a document whose subject package is also a dependency of
			// itself — would otherwise produce a self-edge, which is not a
			// relationship any consumer can do anything with.
			if key != subjectKey {
				refs = append(refs, key)
			}
			if seen[key] || isSubject[key] {
				continue
			}
			seen[key] = true
			out.ByKey[key] = comp
			out.Components = append(out.Components, comp)
		}
		out.DependsOn[subjectKey] = append(out.DependsOn[subjectKey], refs...)
	}
	sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].key() < out.Components[j].key() })
	for k, refs := range out.DependsOn {
		sort.Strings(refs)
		out.DependsOn[k] = dedupeStrings(refs)
	}
	return out
}

func dedupeStrings(in []string) []string {
	out := in[:0:0]
	var last string
	for i, s := range in {
		if i > 0 && s == last {
			continue
		}
		out = append(out, s)
		last = s
	}
	return out
}

// spdxDocument renders the assembled image as SPDX 2.3 JSON.
func (ib *imageBom) spdxDocument() ([]byte, error) {
	if err := ib.validate(); err != nil {
		return nil, err
	}
	res := ib.packages()

	packages := []*v2_3.Package{spdxPackage(ib.imagePackage(), spdxImageID)}
	var files []*v2_3.File
	relationships := []*v2_3.Relationship{{
		RefA:         common.MakeDocElementID("", spdxDescribesFromDocDoc),
		RefB:         common.MakeDocElementID("", spdxImageID),
		Relationship: "DESCRIBES",
	}}
	for _, p := range append(append([]bomPackage{}, res.Subjects...), res.Components...) {
		packages = append(packages, spdxPackage(p, p.elementID()))
		owned, contains := spdxFileElements(p, p.elementID())
		files = append(files, owned...)
		relationships = append(relationships, contains...)
	}
	for _, subject := range res.Subjects {
		relationships = append(relationships, &v2_3.Relationship{
			RefA:         common.MakeDocElementID("", spdxImageID),
			RefB:         common.MakeDocElementID("", subject.elementID()),
			Relationship: "CONTAINS",
		})
	}
	for _, subject := range res.Subjects {
		for _, ref := range res.DependsOn[subject.key()] {
			relationships = append(relationships, &v2_3.Relationship{
				RefA:         common.MakeDocElementID("", subject.elementID()),
				RefB:         common.MakeDocElementID("", res.ByKey[ref].elementID()),
				Relationship: "DEPENDS_ON",
			})
		}
	}

	doc := &v2_3.Document{
		SPDXVersion:       spdxVersion,
		DataLicense:       spdxDataLicense,
		SPDXIdentifier:    common.ElementID(spdxDescribesFromDocDoc),
		DocumentName:      ib.Repository + "@" + ib.Digest,
		DocumentNamespace: ib.documentNamespace(),
		CreationInfo: &v2_3.CreationInfo{
			Creators: []common.Creator{
				{CreatorType: "Tool", Creator: imageDocumentCreator},
				{CreatorType: "Organization", Creator: creatorOrganization},
			},
			Created: ib.Created.Format(time.RFC3339),
		},
		Packages:      packages,
		Files:         files,
		Relationships: relationships,
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode spdx document: %v", err)
	}
	return append(out, '\n'), nil
}

// documentNamespace is unique per document without a random component, so
// two assemblies of one image differ only in their timestamp. SPDX requires
// uniqueness; a UUID would also give it and would make the documents
// impossible to compare.
func (ib *imageBom) documentNamespace() string {
	slug := strings.NewReplacer(":", "-", "/", "-").Replace(ib.Digest + "-" + ib.Platform)
	return documentNamespaceBase + strings.Trim(ib.Repository, "/") + "-" + slug
}

// spdxPackage renders one package. A package carrying files is rendered
// with FilesAnalyzed true and a verification code, because SPDX requires
// the code whenever files were analyzed; one carrying none is rendered with
// FilesAnalyzed false, which is a statement that no file-level analysis was
// claimed rather than one that no files exist.
func spdxPackage(p bomPackage, id string) *v2_3.Package {
	out := &v2_3.Package{
		PackageName:               p.Name,
		PackageSPDXIdentifier:     common.ElementID(strings.TrimPrefix(id, "SPDXRef-")),
		PackageVersion:            orNoAssertion(p.Version),
		PackageSupplier:           &common.Supplier{Supplier: p.Supplier},
		PackageDownloadLocation:   noAssertion,
		FilesAnalyzed:             false,
		IsFilesAnalyzedTagPresent: true,
		PackageLicenseConcluded:   orNoAssertion(p.LicenseConcluded),
		PackageLicenseDeclared:    orNoAssertion(p.LicenseDeclared),
		PackageLicenseComments:    p.LicenseComment,
		PackageCopyrightText:      noAssertion,
		PackageSourceInfo:         p.SourceInfo,
		PrimaryPackagePurpose:     p.Purpose,
	}
	if p.Supplier != noAssertion && p.Supplier != "" {
		out.PackageSupplier = &common.Supplier{SupplierType: "Organization", Supplier: p.Supplier}
	}
	if p.Sha256 != "" {
		out.PackageChecksums = []common.Checksum{{Algorithm: common.SHA256, Value: p.Sha256}}
	}
	if p.Purl != "" {
		out.PackageExternalReferences = []*v2_3.PackageExternalReference{{
			Category: "PACKAGE-MANAGER",
			RefType:  "purl",
			Locator:  p.Purl,
		}}
	}
	if len(p.Files) > 0 {
		out.FilesAnalyzed = true
		out.PackageVerificationCode = &common.PackageVerificationCode{Value: verificationCode(p.Files)}
	}
	return out
}

// spdxFileElements renders a package's analyzed files, and the CONTAINS
// relationships tying them to it.
//
// The files are document-level elements referenced by relationship rather
// than a list nested inside the package. That is what SPDX 2.3 JSON
// specifies, and it is also the only shape that survives a round trip
// through tools-golang: the library marshals a nested list happily and
// reads a document back through `hasFiles` and the top-level `files` array,
// so a document written the nested way parses as a package with no files at
// all — which is a contribution silently losing everything inside it.
func spdxFileElements(p bomPackage, id string) ([]*v2_3.File, []*v2_3.Relationship) {
	files := make([]*v2_3.File, 0, len(p.Files))
	relationships := make([]*v2_3.Relationship, 0, len(p.Files))
	for _, f := range p.Files {
		fileID := spdxFileID(id, f)
		file := &v2_3.File{
			FileName:           f.Path,
			FileSPDXIdentifier: common.ElementID(fileID),
			LicenseConcluded:   noAssertion,
			FileCopyrightText:  noAssertion,
		}
		if f.Sha1 != "" {
			file.Checksums = append(file.Checksums, common.Checksum{Algorithm: common.SHA1, Value: f.Sha1})
		}
		if f.Sha256 != "" {
			file.Checksums = append(file.Checksums, common.Checksum{Algorithm: common.SHA256, Value: f.Sha256})
		}
		files = append(files, file)
		relationships = append(relationships, &v2_3.Relationship{
			RefA:         common.MakeDocElementID("", id),
			RefB:         common.MakeDocElementID("", fileID),
			Relationship: "CONTAINS",
		})
	}
	return files, relationships
}

// spdxFileID is unique per file *and* per owning package, so two packages
// carrying the same path with the same bytes do not collide on one element.
func spdxFileID(owner string, f bomFile) string {
	sum := sha256.Sum256([]byte(owner + "/" + f.Path + "#" + f.Sha256))
	return "File-" + hex.EncodeToString(sum[:8])
}

// verificationCode is SPDX 2.3's package verification code: the SHA-1 of
// every analyzed file's SHA-1, sorted and concatenated. The algorithm is
// the spec's and is not a choice; SHA-1 is not being relied on for anything
// here beyond matching what a consumer's validator recomputes.
//
// Every file reaching here has a SHA-1 — walkTree computes one and
// filesByOwner refuses a contribution whose files lack one — so this never
// silently sums a subset. That is load bearing rather than incidental: a
// code computed over some of the files is wrong rather than missing, and a
// validator recomputing it has no way to say why it disagrees.
func verificationCode(files []bomFile) string {
	sums := make([]string, 0, len(files))
	for _, f := range files {
		sums = append(sums, f.Sha1)
	}
	sort.Strings(sums)
	return sha1Hex(strings.Join(sums, ""))
}

// cycloneDxDocument renders the assembled image as CycloneDX 1.6 JSON.
//
// Every value here comes off the same imageBom the SPDX renderer read, so
// the two cannot disagree about a component, a version, a checksum or a
// licence. What differs is vocabulary: CycloneDX has no NOASSERTION, so a
// package that declares nothing carries no licence entry at all rather than
// a string asserting the absence of one.
func (ib *imageBom) cycloneDxDocument() ([]byte, error) {
	if err := ib.validate(); err != nil {
		return nil, err
	}
	res := ib.packages()

	all := make([]cdx.Component, 0, len(res.Subjects)+len(res.Components))
	imageRefs := make([]string, 0, len(res.Subjects))
	dependencies := []cdx.Dependency{}
	for _, subject := range res.Subjects {
		all = append(all, cycloneDxComponent(subject))
		imageRefs = append(imageRefs, subject.bomRef())
		// res.ByKey and not res.Components: a subject that another
		// contribution also depends on is not in the component list, and
		// looking it up there would resolve to a zero package and emit a
		// bom-ref naming nothing in this document. The SPDX renderer reads
		// the same table, which is what keeps the two graphs identical.
		refs := make([]string, 0, len(res.DependsOn[subject.key()]))
		for _, ref := range res.DependsOn[subject.key()] {
			refs = append(refs, res.ByKey[ref].bomRef())
		}
		dependencies = append(dependencies, cdx.Dependency{Ref: subject.bomRef(), Dependencies: &refs})
	}
	for _, comp := range res.Components {
		all = append(all, cycloneDxComponent(comp))
		// An explicit empty entry rather than no entry: CycloneDX gives a
		// consumer no way to tell "this component depends on nothing" from
		// "nobody said", and the SPDX side states the former by having no
		// DEPENDS_ON out of a package that is listed in full.
		none := []string{}
		dependencies = append(dependencies, cdx.Dependency{Ref: comp.bomRef(), Dependencies: &none})
	}
	subjectComponent := cycloneDxComponent(ib.imagePackage())
	subjectComponent.Type = cdx.ComponentTypeContainer
	dependencies = append([]cdx.Dependency{{Ref: subjectComponent.BOMRef, Dependencies: &imageRefs}}, dependencies...)

	bom := cdx.NewBOM()
	bom.SerialNumber = ib.serialNumber()
	bom.Metadata = &cdx.Metadata{
		Timestamp: ib.Created.Format(time.RFC3339),
		Tools: &cdx.ToolsChoice{Components: &[]cdx.Component{{
			Type:   cdx.ComponentTypeApplication,
			Name:   imageDocumentCreator,
			Author: creatorOrganization,
		}}},
		Component: &subjectComponent,
		Supplier:  &cdx.OrganizationalEntity{Name: creatorOrganization},
	}
	bom.Components = &all
	bom.Dependencies = &dependencies

	// The library's own encoder rather than encoding/json: EncodeVersion
	// converts the BOM to the target revision and stamps the matching
	// $schema and specVersion.
	var buf bytes.Buffer
	enc := cdx.NewBOMEncoder(&buf, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	if err := enc.EncodeVersion(bom, cycloneDxSpecVersion); err != nil {
		return nil, fmt.Errorf("encode cyclonedx document: %v", err)
	}
	return buf.Bytes(), nil
}

// serialNumber derives the BOM's urn:uuid from the published digest rather
// than from a random source, so two assemblies of one image differ only in
// their timestamp.
func (ib *imageBom) serialNumber() string {
	raw, err := hex.DecodeString(digestHex(ib.Digest))
	if err != nil || len(raw) < 16 {
		sum := sha256.Sum256([]byte(ib.Digest + ib.Platform))
		raw = sum[:]
	}
	b := make([]byte, 16)
	copy(b, raw[:16])
	// The platform is mixed in because one manifest list is one digest and
	// several platforms, and two documents about it must not claim to be
	// the same document.
	if ib.Platform != "" {
		mix := sha256.Sum256([]byte(ib.Platform))
		for i := range b {
			b[i] ^= mix[i]
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return "urn:uuid:" + hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

// cycloneDxComponent renders one package in CycloneDX's vocabulary.
func cycloneDxComponent(p bomPackage) cdx.Component {
	out := cdx.Component{
		BOMRef:  p.bomRef(),
		Type:    cycloneDxType(p.Purpose),
		Name:    p.Name,
		Version: orNoAssertion(p.Version),
	}
	if p.Purl != "" {
		out.PackageURL = p.Purl
	}
	if p.Supplier != "" {
		out.Supplier = &cdx.OrganizationalEntity{Name: p.Supplier}
	}
	if p.SourceInfo != "" {
		out.Description = p.SourceInfo
	}
	if p.Sha256 != "" {
		out.Hashes = &[]cdx.Hash{{Algorithm: cdx.HashAlgoSHA256, Value: p.Sha256}}
	}
	out.Licenses = cycloneDxLicenses(p)
	if len(p.Files) > 0 {
		files := make([]cdx.Component, 0, len(p.Files))
		for _, f := range p.Files {
			file := cdx.Component{
				BOMRef: p.bomRef() + "#" + f.Path,
				Type:   cdx.ComponentTypeFile,
				Name:   f.Path,
			}
			hashes := []cdx.Hash{}
			if f.Sha1 != "" {
				hashes = append(hashes, cdx.Hash{Algorithm: cdx.HashAlgoSHA1, Value: f.Sha1})
			}
			if f.Sha256 != "" {
				hashes = append(hashes, cdx.Hash{Algorithm: cdx.HashAlgoSHA256, Value: f.Sha256})
			}
			if len(hashes) > 0 {
				file.Hashes = &hashes
			}
			files = append(files, file)
		}
		out.Components = &files
	}
	return out
}

// cycloneDxLicenses renders a package's licence, or nothing when there is
// nothing to say.
//
// Two things here are not obvious and both were wrong before.
//
// **The declared licence is not the only one that counts.** SPDX carries a
// declared and a concluded licence and they are independent: an ecosystem
// module that runs a classifier publishes its best match as declared and
// promotes it to concluded only above a confidence threshold, but a producer
// is equally free to conclude something it cannot say the artifact declared.
// Gating on the declared licence alone dropped that second case from
// CycloneDX while SPDX kept it, which is the two formats disagreeing —
// exactly what one resolution is supposed to make impossible.
//
// **A CycloneDX licence id is an enum, not a string.** `license.id` is
// validated against the SPDX licence list, so a `LicenseRef-` identifier —
// which validateLicenseExpression accepts on purpose, and which is the only
// way to name a licence the list does not have — makes the document
// schema-invalid. Those go in `license.name`, which is the free-text field
// for exactly this. Anything carrying an operator is an expression, and an
// operator is `AND`, `OR`, `WITH` or a parenthesis rather than "any space":
// a licence name with a space in it is not an expression.
func cycloneDxLicenses(p bomPackage) *cdx.Licenses {
	value, acknowledgement := p.LicenseDeclared, cdx.LicenseAcknowledgementDeclared
	if value == noAssertion {
		value, acknowledgement = p.LicenseConcluded, cdx.LicenseAcknowledgementConcluded
	} else if p.LicenseConcluded != noAssertion {
		acknowledgement = cdx.LicenseAcknowledgementConcluded
	}
	if value == noAssertion || value == "" {
		return nil
	}
	// A LicenseChoice carries either a single licence or an expression,
	// never both.
	if isLicenseExpression(value) {
		return &cdx.Licenses{{Expression: value, Acknowledgement: &acknowledgement}}
	}
	if strings.HasPrefix(value, "LicenseRef-") || strings.HasPrefix(value, "DocumentRef-") {
		return &cdx.Licenses{{License: &cdx.License{Name: value, Acknowledgement: acknowledgement}}}
	}
	return &cdx.Licenses{{License: &cdx.License{ID: value, Acknowledgement: acknowledgement}}}
}

// isLicenseExpression reports whether value composes licences rather than
// naming one. The operators are uppercase by the SPDX expression grammar, so
// this does not fold case: a licence identifier containing the letters "or"
// is not an expression.
func isLicenseExpression(value string) bool {
	if strings.ContainsAny(value, "()") {
		return true
	}
	for _, op := range []string{" AND ", " OR ", " WITH "} {
		if strings.Contains(value, op) {
			return true
		}
	}
	return false
}

// cycloneDxType maps SPDX's primary package purpose onto CycloneDX's
// component type. An unrecognized purpose becomes a library rather than
// being dropped: the component is real either way, and its type is the
// least load-bearing thing about it.
func cycloneDxType(purpose string) cdx.ComponentType {
	switch strings.ToUpper(purpose) {
	case "APPLICATION":
		return cdx.ComponentTypeApplication
	case "CONTAINER":
		return cdx.ComponentTypeContainer
	case "FILE":
		return cdx.ComponentTypeFile
	case "OPERATING-SYSTEM":
		return cdx.ComponentTypeOS
	default:
		return cdx.ComponentTypeLibrary
	}
}
