package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"dagger/tests/internal/dagger"
)

// sbomDir returns the SBOM fixture: a main package with one external
// dependency, so the documents have a component to describe.
func sbomDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/sbom")
}

// sbomBinary builds the fixture and returns the compiled binary. The
// documents describe this file, so the tests have to hold the same
// artifact the module reads its build info out of.
func sbomBinary(goImageTag string) *dagger.File {
	return dag.Go(dagger.GoOpts{Version: goImageTag}).
		Build(sbomDir(), dagger.GoBuildOpts{Pkg: ".", ArtifactName: "sbom"}).
		File("sbom")
}

// spdxDocument is the part of an SPDX 2.3 document these tests assert on.
type spdxDocument struct {
	SPDXVersion       string `json:"spdxVersion"`
	DataLicense       string `json:"dataLicense"`
	SPDXID            string `json:"SPDXID"`
	Name              string `json:"name"`
	DocumentNamespace string `json:"documentNamespace"`
	CreationInfo      struct {
		Creators []string `json:"creators"`
		Created  string   `json:"created"`
	} `json:"creationInfo"`
	Packages []struct {
		Name             string `json:"name"`
		SPDXID           string `json:"SPDXID"`
		VersionInfo      string `json:"versionInfo"`
		Supplier         string `json:"supplier"`
		DownloadLocation string `json:"downloadLocation"`
		LicenseConcluded string `json:"licenseConcluded"`
		LicenseDeclared  string `json:"licenseDeclared"`
		Checksums        []struct {
			Algorithm string `json:"algorithm"`
			Value     string `json:"checksumValue"`
		} `json:"checksums"`
		ExternalRefs []struct {
			Category string `json:"referenceCategory"`
			RefType  string `json:"referenceType"`
			Locator  string `json:"referenceLocator"`
		} `json:"externalRefs"`
	} `json:"packages"`
	Relationships []struct {
		From string `json:"spdxElementId"`
		To   string `json:"relatedSpdxElement"`
		Kind string `json:"relationshipType"`
	} `json:"relationships"`
}

// cycloneDxDocument is the part of a CycloneDX document these tests
// assert on.
type cycloneDxDocument struct {
	BOMFormat    string `json:"bomFormat"`
	SpecVersion  string `json:"specVersion"`
	SerialNumber string `json:"serialNumber"`
	Metadata     struct {
		Timestamp string `json:"timestamp"`
		Component struct {
			Name       string `json:"name"`
			Type       string `json:"type"`
			PackageURL string `json:"purl"`
			Hashes     []struct {
				Algorithm string `json:"alg"`
				Content   string `json:"content"`
			} `json:"hashes"`
		} `json:"component"`
		Tools struct {
			Components []struct {
				Name string `json:"name"`
			} `json:"components"`
		} `json:"tools"`
	} `json:"metadata"`
	Components []struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		PackageURL string `json:"purl"`
		Licenses   []struct {
			License *struct {
				ID              string `json:"id"`
				Acknowledgement string `json:"acknowledgement"`
			} `json:"license"`
			Expression      string `json:"expression"`
			Acknowledgement string `json:"acknowledgement"`
		} `json:"licenses"`
	} `json:"components"`
	Dependencies []struct {
		Ref          string   `json:"ref"`
		Dependencies []string `json:"dependsOn"`
	} `json:"dependencies"`
}

// SpdxDocumentIsCompliant asserts the SPDX document carries the elements
// a consumer requires, not merely that it parses.
//
// The library guarantees the syntax; nothing guarantees the fields are
// populated, and an SBOM missing a supplier or a unique identifier fails
// the minimum-elements check every regulated consumer runs even though
// it is perfectly well-formed JSON. So this walks the NTIA minimum
// elements one at a time: supplier, component name, version, unique
// identifier, dependency relationship, author of the SBOM data, and
// timestamp.
func (t *Tests) SpdxDocumentIsCompliant(ctx context.Context, goImageTag string) error {
	doc, err := readSpdx(ctx, goImageTag)
	if err != nil {
		return err
	}
	if doc.SPDXVersion != "SPDX-2.3" {
		return fmt.Errorf("expected spdxVersion SPDX-2.3, got %q", doc.SPDXVersion)
	}
	if doc.DataLicense != "CC0-1.0" {
		return fmt.Errorf("expected dataLicense CC0-1.0, got %q", doc.DataLicense)
	}
	if doc.SPDXID != "SPDXRef-DOCUMENT" {
		return fmt.Errorf("expected SPDXID SPDXRef-DOCUMENT, got %q", doc.SPDXID)
	}
	if doc.DocumentNamespace == "" {
		return fmt.Errorf("document carries no documentNamespace")
	}
	// Author of the SBOM data, and the timestamp.
	if len(doc.CreationInfo.Creators) == 0 {
		return fmt.Errorf("document names no creator")
	}
	if doc.CreationInfo.Created == "" {
		return fmt.Errorf("document carries no created timestamp")
	}
	if len(doc.Packages) < 2 {
		return fmt.Errorf("expected the subject package and at least one dependency, got %d packages", len(doc.Packages))
	}
	for _, pkg := range doc.Packages {
		// Component name, version, supplier, unique identifier.
		if pkg.Name == "" {
			return fmt.Errorf("package %s carries no name", pkg.SPDXID)
		}
		if pkg.VersionInfo == "" {
			return fmt.Errorf("package %s carries no versionInfo", pkg.Name)
		}
		if pkg.Supplier == "" {
			return fmt.Errorf("package %s carries no supplier", pkg.Name)
		}
		if pkg.DownloadLocation == "" {
			return fmt.Errorf("package %s carries no downloadLocation", pkg.Name)
		}
		if !strings.HasPrefix(pkg.SPDXID, "SPDXRef-") {
			return fmt.Errorf("package %s has a malformed SPDXID %q", pkg.Name, pkg.SPDXID)
		}
		if len(pkg.ExternalRefs) == 0 {
			return fmt.Errorf("package %s carries no purl, so it has no identifier outside this document", pkg.Name)
		}
		for _, ref := range pkg.ExternalRefs {
			if ref.RefType == "purl" && !strings.HasPrefix(ref.Locator, "pkg:golang/") {
				return fmt.Errorf("package %s has a non-Go purl %q", pkg.Name, ref.Locator)
			}
		}
	}
	// Dependency relationships, and a document that says what it is about.
	describes, dependsOn := 0, 0
	for _, rel := range doc.Relationships {
		switch rel.Kind {
		case "DESCRIBES":
			describes++
		case "DEPENDS_ON":
			dependsOn++
		}
	}
	if describes != 1 {
		return fmt.Errorf("expected exactly 1 DESCRIBES relationship, got %d", describes)
	}
	if dependsOn != len(doc.Packages)-1 {
		return fmt.Errorf("expected %d DEPENDS_ON relationships, got %d", len(doc.Packages)-1, dependsOn)
	}
	// The subject is the built artifact, and it is identified by its own
	// bytes rather than by a name that could describe anything.
	subject := doc.Packages[0]
	if subject.SPDXID != "SPDXRef-Binary" {
		return fmt.Errorf("expected the first package to be the subject, got %s", subject.SPDXID)
	}
	if len(subject.Checksums) == 0 {
		return fmt.Errorf("the subject package carries no checksum, so nothing ties the document to the artifact")
	}
	return nil
}

// CycloneDxDocumentIsCompliant asserts the CycloneDX document is at the
// pinned spec version and carries the same required elements.
func (t *Tests) CycloneDxDocumentIsCompliant(ctx context.Context, goImageTag string) error {
	doc, err := readCycloneDx(ctx, goImageTag)
	if err != nil {
		return err
	}
	if doc.BOMFormat != "CycloneDX" {
		return fmt.Errorf("expected bomFormat CycloneDX, got %q", doc.BOMFormat)
	}
	// The version is a choice this module makes, not the library's
	// default: asserting it is what stops a library upgrade silently
	// re-targeting every document this pipeline produces.
	if doc.SpecVersion != "1.6" {
		return fmt.Errorf("expected specVersion 1.6, got %q", doc.SpecVersion)
	}
	if !strings.HasPrefix(doc.SerialNumber, "urn:uuid:") {
		return fmt.Errorf("expected a urn:uuid serial number, got %q", doc.SerialNumber)
	}
	if doc.Metadata.Timestamp == "" {
		return fmt.Errorf("document carries no metadata timestamp")
	}
	if len(doc.Metadata.Tools.Components) == 0 {
		return fmt.Errorf("document names no producing tool")
	}
	if doc.Metadata.Component.Type != "application" {
		return fmt.Errorf("expected the subject component to be an application, got %q", doc.Metadata.Component.Type)
	}
	if len(doc.Metadata.Component.Hashes) == 0 {
		return fmt.Errorf("the subject component carries no hash, so nothing ties the document to the artifact")
	}
	if len(doc.Components) == 0 {
		return fmt.Errorf("document lists no components")
	}
	for _, comp := range doc.Components {
		if comp.Name == "" || comp.Version == "" {
			return fmt.Errorf("component %+v is missing a name or a version", comp)
		}
		if !strings.HasPrefix(comp.PackageURL, "pkg:golang/") {
			return fmt.Errorf("component %s has a non-Go purl %q", comp.Name, comp.PackageURL)
		}
	}
	if len(doc.Dependencies) == 0 {
		return fmt.Errorf("document records no dependency relationships")
	}
	return nil
}

// SbomFormatsAgreeOnComponents asserts the two documents describe the
// same component set.
//
// This is the property that a single resolution buys and that two
// independent tools cannot offer at any price: two documents about one
// binary that disagree about a component or a licence are an audit
// finding, and nothing downstream can adjudicate which is right. The
// licences are compared as well as the coordinates, because a component
// set that matches while the licences differ is the same failure wearing
// a different hat.
func (t *Tests) SbomFormatsAgreeOnComponents(ctx context.Context, goImageTag string) error {
	spdx, err := readSpdx(ctx, goImageTag)
	if err != nil {
		return err
	}
	cdx, err := readCycloneDx(ctx, goImageTag)
	if err != nil {
		return err
	}

	fromSpdx := map[string]string{}
	for _, pkg := range spdx.Packages {
		if pkg.SPDXID == "SPDXRef-Binary" {
			continue
		}
		purl := ""
		for _, ref := range pkg.ExternalRefs {
			if ref.RefType == "purl" {
				purl = ref.Locator
			}
		}
		// CycloneDX has no NOASSERTION, so an unidentified licence is
		// an absent entry there and the literal string here.
		declared := pkg.LicenseDeclared
		if declared == "NOASSERTION" {
			declared = ""
		}
		fromSpdx[purl] = declared
	}

	fromCdx := map[string]string{}
	for _, comp := range cdx.Components {
		declared := ""
		for _, choice := range comp.Licenses {
			if choice.Expression != "" {
				declared = choice.Expression
			} else if choice.License != nil {
				declared = choice.License.ID
			}
		}
		fromCdx[comp.PackageURL] = declared
	}

	if len(fromSpdx) != len(fromCdx) {
		return fmt.Errorf("component counts differ: SPDX %d, CycloneDX %d\nSPDX: %v\nCycloneDX: %v",
			len(fromSpdx), len(fromCdx), sortedKeys(fromSpdx), sortedKeys(fromCdx))
	}
	for purl, declared := range fromSpdx {
		other, ok := fromCdx[purl]
		if !ok {
			return fmt.Errorf("SPDX names %s but CycloneDX does not; CycloneDX has %v", purl, sortedKeys(fromCdx))
		}
		if other != declared {
			return fmt.Errorf("%s: SPDX declares licence %q, CycloneDX declares %q", purl, declared, other)
		}
	}
	return nil
}

// SbomResolvesDependencyLicences asserts a licence is actually resolved
// and that the declared/concluded distinction is populated rather than
// left at NOASSERTION for everything.
//
// A Go binary carries no licence text, so this is the half of the
// document that can only come from the source: if licence resolution
// silently did nothing, every field below would still be present and
// every one would say NOASSERTION. That is why the assertion is on a
// specific dependency with a known licence rather than on the shape.
func (t *Tests) SbomResolvesDependencyLicences(ctx context.Context, goImageTag string) error {
	doc, err := readSpdx(ctx, goImageTag)
	if err != nil {
		return err
	}
	const dependency = "github.com/google/uuid"
	for _, pkg := range doc.Packages {
		if pkg.Name != dependency {
			continue
		}
		if pkg.LicenseDeclared != "BSD-3-Clause" {
			return fmt.Errorf("expected %s to declare BSD-3-Clause, got %q", dependency, pkg.LicenseDeclared)
		}
		// The classifier matched the whole file, so the licence is
		// concluded and not merely declared. A low-confidence match
		// would leave this NOASSERTION with the declared licence intact.
		if pkg.LicenseConcluded != "BSD-3-Clause" {
			return fmt.Errorf("expected %s to conclude BSD-3-Clause, got %q", dependency, pkg.LicenseConcluded)
		}
		return nil
	}
	return fmt.Errorf("expected the document to name %s; it lists %v", dependency, spdxPackageNames(doc))
}

// SbomDescribesTheBinaryNotTheSourceTree asserts the component list is
// what was linked in rather than what go.mod happens to require.
//
// The distinction is the reason the graph is read out of the compiled
// artifact: a source tree's requirement list includes modules no code
// imports, and a document built from it over-reports. The fixture's
// go.mod requires exactly one module and its binary links exactly that
// one, so the check is that the document holds the linked module and no
// tooling-only entries beside it.
func (t *Tests) SbomDescribesTheBinaryNotTheSourceTree(ctx context.Context, goImageTag string) error {
	doc, err := readSpdx(ctx, goImageTag)
	if err != nil {
		return err
	}
	names := spdxPackageNames(doc)
	want := []string{"github.com/google/uuid", "sbom"}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		return fmt.Errorf("expected the document to describe exactly %v, got %v", want, names)
	}
	return nil
}

// readSpdx generates and parses the SPDX document for the fixture.
func readSpdx(ctx context.Context, goImageTag string) (*spdxDocument, error) {
	raw, err := dag.Go(dagger.GoOpts{Version: goImageTag}).
		Spdx(sbomBinary(goImageTag), sbomDir()).
		Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("Go.Spdx: %w", err)
	}
	doc := &spdxDocument{}
	if err := json.Unmarshal([]byte(raw), doc); err != nil {
		return nil, fmt.Errorf("decode spdx document: %v", err)
	}
	return doc, nil
}

// readCycloneDx generates and parses the CycloneDX document for the
// fixture.
func readCycloneDx(ctx context.Context, goImageTag string) (*cycloneDxDocument, error) {
	raw, err := dag.Go(dagger.GoOpts{Version: goImageTag}).
		CycloneDx(sbomBinary(goImageTag), sbomDir()).
		Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("Go.CycloneDx: %w", err)
	}
	doc := &cycloneDxDocument{}
	if err := json.Unmarshal([]byte(raw), doc); err != nil {
		return nil, fmt.Errorf("decode cyclonedx document: %v", err)
	}
	return doc, nil
}

// spdxPackageNames lists every package name in the document.
func spdxPackageNames(doc *spdxDocument) []string {
	out := make([]string, 0, len(doc.Packages))
	for _, pkg := range doc.Packages {
		out = append(out, pkg.Name)
	}
	return out
}

// sortedKeys renders a map's keys in a stable order, so a failure
// message reads the same on every run.
func sortedKeys(in map[string]string) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
