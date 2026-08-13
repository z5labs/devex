package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spdx/tools-golang/spdx/v2/common"
	"github.com/spdx/tools-golang/spdx/v2/v2_3"
)

// ImageSbomSelfTest checks the rule the image-level documents exist to keep:
// everything that entered the image is in them, once, and the two formats
// say the same thing about it.
//
// It sits on the module rather than in tests/ for the reason
// ImageEnvironmentSelfTest records, and for one more that is specific to
// this rule. The interesting case is an image assembled from *several*
// sources, and until devex#392 lands there is no caller-facing way to put a
// second contribution into one — every image a published App can build has
// exactly one. So a suite built out of real publishes can only ever exercise
// the single-contribution case, which is the case that already worked before
// this existed. Driving the assembly directly is what makes the de-duplication
// and the cross-format agreement guarantees rather than comments.
//
// It runs in process over synthesized documents and needs no container, so it
// is cheap enough to be a check of its own.
//
// +check
// +cache="session"
func (m *Z5labs) ImageSbomSelfTest(ctx context.Context) error {
	if err := checkAssembledImageDocuments(); err != nil {
		return err
	}
	if err := checkContributionRefusals(); err != nil {
		return err
	}
	return checkLicenseExpressions()
}

// The digest and repository the self test assembles against. The digest is
// a real SHA-256 shape rather than a short string, because half of what the
// subject has to get right is being a checksum a consumer can compare.
const (
	selfTestRepository = "z5labs/example"
	selfTestDigest     = "sha256:1b0ae2f4d38f2d8fd1a0b6a1d5b1f4a5f7c3b2e19d8c7a6b5f4e3d2c1b0a9f8e"
	selfTestPlatform   = "linux/amd64"
)

// checkAssembledImageDocuments assembles one image out of three
// contributions and checks both rendered documents against the same
// expectations.
func checkAssembledImageDocuments() error {
	// Two ecosystem contributions that share a dependency, plus one with no
	// ecosystem at all. The shared dependency is the point of the pair: an
	// image whose document listed golang.org/x/text twice would inflate
	// every count a consumer takes off it, and nothing about a well-formed
	// document would say so.
	binaryOne, err := ecosystemContribution("app", "pkg:golang/example.com/app@v1.2.3", []string{
		"pkg:golang/golang.org/x/text@v0.35.0",
		"pkg:golang/github.com/google/uuid@v1.6.0",
	})
	if err != nil {
		return err
	}
	binaryTwo, err := ecosystemContribution("sidecar", "pkg:golang/example.com/sidecar@v0.1.0", []string{
		"pkg:golang/golang.org/x/text@v0.35.0",
	})
	if err != nil {
		return err
	}
	bundle, err := contributionDocument(bomPackage{
		Name:     "/etc/ssl/certs/ca-certificates.crt",
		Version:  "2026.01.01",
		Sha256:   "aa" + strings.Repeat("00", 31),
		Supplier: noAssertion,
		Purpose:  "FILE",
		Files: []bomFile{
			{Path: "ca-certificates.crt", Sha1: strings.Repeat("11", 20), Sha256: "aa" + strings.Repeat("00", 31)},
		},
	}, "MPL-2.0")
	if err != nil {
		return fmt.Errorf("render the ecosystem-less contribution: %v", err)
	}

	vb := variantBom{Platform: selfTestPlatform}
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{"app", binaryOne},
		{"sidecar", binaryTwo},
		{"/etc/ssl/certs/ca-certificates.crt", bundle},
	} {
		parsed, err := parseContribution(c.name, c.raw)
		if err != nil {
			return fmt.Errorf("parse the document describing %s: %v", c.name, err)
		}
		vb.Contributions = append(vb.Contributions, *parsed)
	}

	ib := vb.assemble(selfTestRepository, selfTestDigest, "v1.2.3", time.Unix(0, 0))
	spdxRaw, err := ib.spdxDocument()
	if err != nil {
		return fmt.Errorf("render the assembled SPDX document: %v", err)
	}
	cdxRaw, err := ib.cycloneDxDocument()
	if err != nil {
		return fmt.Errorf("render the assembled CycloneDX document: %v", err)
	}

	spdxSet, spdxSubject, contains, err := readAssembledSpdx(spdxRaw)
	if err != nil {
		return err
	}
	cdxSet, cdxSubject, cdxTopLevel, err := readAssembledCycloneDx(cdxRaw)
	if err != nil {
		return err
	}

	// The subject is the published digest, in both. This is the criterion
	// that moved in devex#409: a document naming a built artifact is one a
	// consumer holding a pulled image cannot check.
	for what, subject := range map[string]assembledSubject{"SPDX": spdxSubject, "CycloneDX": cdxSubject} {
		if subject.Sha256 != digestHex(selfTestDigest) {
			return fmt.Errorf("the %s document's subject carries checksum %q, want the published digest %s",
				what, subject.Sha256, digestHex(selfTestDigest))
		}
		if !strings.Contains(subject.Purl, selfTestDigest) {
			return fmt.Errorf("the %s document's subject purl %q does not name the published digest", what, subject.Purl)
		}
	}

	// The formats agree. Not "the counts match" — the coordinates and the
	// declared licences, because a component set that matches while the
	// licences differ is the same failure wearing a different hat.
	if err := sameComponents(spdxSet, cdxSet); err != nil {
		return err
	}

	// Every contribution is in the image, and the shared dependency is in it
	// once. Nine packages would mean golang.org/x/text was listed twice.
	wantComponents := []string{
		"/etc/ssl/certs/ca-certificates.crt@2026.01.01",
		"pkg:golang/example.com/app@v1.2.3",
		"pkg:golang/example.com/sidecar@v0.1.0",
		"pkg:golang/github.com/google/uuid@v1.6.0",
		"pkg:golang/golang.org/x/text@v0.35.0",
	}
	got := make([]string, 0, len(spdxSet))
	for key := range spdxSet {
		got = append(got, key)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(wantComponents, "\n") {
		return fmt.Errorf("the assembled component set is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantComponents, "\n"))
	}

	// The three things that *entered* the image are told apart from the
	// things they were made of, in both formats, so attribution survives the
	// merge that replaced the per-contribution documents.
	wantContained := []string{
		"/etc/ssl/certs/ca-certificates.crt@2026.01.01",
		"pkg:golang/example.com/app@v1.2.3",
		"pkg:golang/example.com/sidecar@v0.1.0",
	}
	if strings.Join(contains, "\n") != strings.Join(wantContained, "\n") {
		return fmt.Errorf("the image CONTAINS\n%s\nwant\n%s", strings.Join(contains, "\n"), strings.Join(wantContained, "\n"))
	}
	if strings.Join(cdxTopLevel, "\n") != strings.Join(wantContained, "\n") {
		return fmt.Errorf("the CycloneDX image depends on\n%s\nwant\n%s", strings.Join(cdxTopLevel, "\n"), strings.Join(wantContained, "\n"))
	}

	// The ecosystem-less contribution's file survives into the image
	// document. A directory summed as one opaque blob would satisfy "the
	// contribution is described" and leave "every file is accounted for"
	// false one level down.
	if files := spdxSet["/etc/ssl/certs/ca-certificates.crt@2026.01.01"].Files; len(files) != 1 || files[0] != "ca-certificates.crt" {
		return fmt.Errorf("the SPDX document lists %v for the certificate bundle, want its one file", files)
	}
	if files := cdxSet["/etc/ssl/certs/ca-certificates.crt@2026.01.01"].Files; len(files) != 1 || files[0] != "ca-certificates.crt" {
		return fmt.Errorf("the CycloneDX document lists %v for the certificate bundle, want its one file", files)
	}
	return nil
}

// checkContributionRefusals checks what happens to a document that cannot
// say what entered the image.
//
// Each of these has to be a refusal rather than a component quietly missing
// from the result, because the whole promise is that the image document
// accounts for everything: a contribution silently dropped is the exact
// artifact — well-formed, attached, incomplete, undetectable — that this
// mechanism exists to prevent.
func checkContributionRefusals() error {
	describesNothing, err := json.Marshal(&v2_3.Document{
		SPDXVersion:       spdxVersion,
		DataLicense:       spdxDataLicense,
		SPDXIdentifier:    common.ElementID(spdxDescribesFromDocDoc),
		DocumentName:      "nothing",
		DocumentNamespace: documentNamespaceBase + "selftest/nothing",
		CreationInfo:      &v2_3.CreationInfo{Created: time.Unix(0, 0).UTC().Format(time.RFC3339)},
		Packages:          []*v2_3.Package{spdxPackage(bomPackage{Name: "orphan", Supplier: noAssertion}, "Package-orphan")},
	})
	if err != nil {
		return fmt.Errorf("encode the describes-nothing fixture: %v", err)
	}
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "not a document at all",
			raw:  []byte("{}"),
		},
		{
			name: "a CycloneDX document where SPDX was required",
			raw:  []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","version":1}`),
		},
		{
			name: "a document that describes nothing",
			raw:  describesNothing,
			want: "declares no DESCRIBES relationship",
		},
	}
	for _, c := range cases {
		if _, err := parseContribution(c.name, c.raw); err == nil {
			return fmt.Errorf("expected %s to be refused, got nil", c.name)
		} else if c.want != "" && !strings.Contains(err.Error(), c.want) {
			return fmt.Errorf("expected the refusal of %s to carry %q, got: %v", c.name, c.want, err)
		}
	}
	return nil
}

// checkLicenseExpressions pins what a caller may say about the licence of
// content with no ecosystem.
func checkLicenseExpressions() error {
	for _, ok := range []string{"", "MIT", "Apache-2.0", "Apache-2.0 WITH LLVM-exception", "(MIT OR GPL-2.0-only)", "LicenseRef-Custom"} {
		if err := validateLicenseExpression(ok); err != nil {
			return fmt.Errorf("expected %q to be accepted as a licence expression, got: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"Copyright (c) 2026 Example, Inc. All rights reserved.",
		"https://example.com/LICENSE",
		"see the LICENSE file\nfor details",
	} {
		if err := validateLicenseExpression(bad); err == nil {
			return fmt.Errorf("expected %q to be refused as a licence expression, got nil", bad)
		}
	}

	// Stated and unstated have to be distinguishable in the document, not
	// merely in the caller's head: NOASSERTION from a caller who said
	// nothing must not read like a classifier's finding.
	stated, err := contributionDocument(bomPackage{Name: "bundle", Supplier: noAssertion, Purpose: "FILE"}, "MIT")
	if err != nil {
		return err
	}
	unstated, err := contributionDocument(bomPackage{Name: "bundle", Supplier: noAssertion, Purpose: "FILE"}, "")
	if err != nil {
		return err
	}
	statedBom, err := parseContribution("bundle", stated)
	if err != nil {
		return err
	}
	unstatedBom, err := parseContribution("bundle", unstated)
	if err != nil {
		return err
	}
	if statedBom.Subject.LicenseDeclared != "MIT" || statedBom.Subject.LicenseConcluded != "MIT" {
		return fmt.Errorf("a stated licence should be both declared and concluded, got declared=%q concluded=%q",
			statedBom.Subject.LicenseDeclared, statedBom.Subject.LicenseConcluded)
	}
	if unstatedBom.Subject.LicenseDeclared != noAssertion || unstatedBom.Subject.LicenseConcluded != noAssertion {
		return fmt.Errorf("an unstated licence should be NOASSERTION on both, got declared=%q concluded=%q",
			unstatedBom.Subject.LicenseDeclared, unstatedBom.Subject.LicenseConcluded)
	}
	if !strings.Contains(unstatedBom.Subject.LicenseComment, "stated no licence") {
		return fmt.Errorf("an unstated licence should say so in the comment, got %q", unstatedBom.Subject.LicenseComment)
	}
	return nil
}

// ecosystemContribution synthesizes the shape a language chain's document
// has: a subject with a purl, and the components it was built from.
//
// It is written against the v2_3 types directly rather than through this
// module's helpers, so the parser is exercised against a document it did not
// write. That is the case that matters — every contribution from an
// ecosystem module is one — and a fixture built from the assembler's own
// renderer would agree with it by construction.
func ecosystemContribution(name, purl string, componentPurls []string) ([]byte, error) {
	subject := bomPackage{
		Name:             name,
		Version:          strings.TrimPrefix(purl[strings.LastIndex(purl, "@"):], "@"),
		Purl:             purl,
		Sha256:           strings.Repeat("ab", 32),
		Supplier:         creatorOrganization,
		Purpose:          "APPLICATION",
		LicenseDeclared:  noAssertion,
		LicenseConcluded: noAssertion,
	}
	packages := []*v2_3.Package{spdxPackage(subject, subject.elementID())}
	relationships := []*v2_3.Relationship{{
		RefA:         common.MakeDocElementID("", spdxDescribesFromDocDoc),
		RefB:         common.MakeDocElementID("", subject.elementID()),
		Relationship: "DESCRIBES",
	}}
	for _, cp := range componentPurls {
		comp := bomPackage{
			Name:             strings.TrimPrefix(cp[:strings.LastIndex(cp, "@")], "pkg:golang/"),
			Version:          strings.TrimPrefix(cp[strings.LastIndex(cp, "@"):], "@"),
			Purl:             cp,
			Supplier:         noAssertion,
			Purpose:          "LIBRARY",
			LicenseDeclared:  "BSD-3-Clause",
			LicenseConcluded: "BSD-3-Clause",
		}
		packages = append(packages, spdxPackage(comp, comp.elementID()))
		relationships = append(relationships, &v2_3.Relationship{
			RefA:         common.MakeDocElementID("", subject.elementID()),
			RefB:         common.MakeDocElementID("", comp.elementID()),
			Relationship: "DEPENDS_ON",
		})
	}
	raw, err := json.MarshalIndent(&v2_3.Document{
		SPDXVersion:       spdxVersion,
		DataLicense:       spdxDataLicense,
		SPDXIdentifier:    common.ElementID(spdxDescribesFromDocDoc),
		DocumentName:      name,
		DocumentNamespace: documentNamespaceBase + "selftest/" + name,
		CreationInfo: &v2_3.CreationInfo{
			Creators: []common.Creator{{CreatorType: "Tool", Creator: "selftest"}},
			Created:  time.Unix(0, 0).UTC().Format(time.RFC3339),
		},
		Packages:      packages,
		Relationships: relationships,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the %s fixture: %v", name, err)
	}
	return raw, nil
}

// assembledComponent is what a consumer reads off either document: the
// coordinate, the declared licence and the files inside it. Both readers
// below decode the *rendered bytes* rather than reusing the internal model,
// so the check is over what was published and not over what was intended.
type assembledComponent struct {
	License string
	Files   []string
}

type assembledSubject struct {
	Sha256 string
	Purl   string
}

// readAssembledSpdx decodes the published SPDX document.
func readAssembledSpdx(raw []byte) (map[string]assembledComponent, assembledSubject, []string, error) {
	var doc struct {
		Packages []struct {
			SPDXID          string `json:"SPDXID"`
			Name            string `json:"name"`
			VersionInfo     string `json:"versionInfo"`
			LicenseDeclared string `json:"licenseDeclared"`
			Checksums       []struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"checksumValue"`
			} `json:"checksums"`
			ExternalRefs []struct {
				Type    string `json:"referenceType"`
				Locator string `json:"referenceLocator"`
			} `json:"externalRefs"`
		} `json:"packages"`
		Files []struct {
			SPDXID   string `json:"SPDXID"`
			FileName string `json:"fileName"`
		} `json:"files"`
		Relationships []struct {
			SpdxElementID      string `json:"spdxElementId"`
			RelatedSpdxElement string `json:"relatedSpdxElement"`
			RelationshipType   string `json:"relationshipType"`
		} `json:"relationships"`
	}
	var subject assembledSubject
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, subject, nil, fmt.Errorf("decode the assembled SPDX document: %v", err)
	}
	fileNames := make(map[string]string, len(doc.Files))
	for _, f := range doc.Files {
		fileNames[f.SPDXID] = f.FileName
	}
	set := make(map[string]assembledComponent, len(doc.Packages))
	keyByID := make(map[string]string, len(doc.Packages))
	filesOf := make(map[string][]string, len(doc.Packages))
	for _, rel := range doc.Relationships {
		if rel.RelationshipType != "CONTAINS" {
			continue
		}
		if name, ok := fileNames[rel.RelatedSpdxElement]; ok {
			filesOf[rel.SpdxElementID] = append(filesOf[rel.SpdxElementID], name)
		}
	}
	for _, pkg := range doc.Packages {
		key := pkg.Name + "@" + pkg.VersionInfo
		for _, ref := range pkg.ExternalRefs {
			if ref.Type == "purl" {
				key = ref.Locator
			}
		}
		keyByID[pkg.SPDXID] = key
		if pkg.SPDXID == spdxImageID {
			subject.Purl = key
			for _, sum := range pkg.Checksums {
				if sum.Algorithm == "SHA256" {
					subject.Sha256 = sum.Value
				}
			}
			continue
		}
		comp := assembledComponent{License: pkg.LicenseDeclared, Files: filesOf[pkg.SPDXID]}
		sort.Strings(comp.Files)
		set[key] = comp
	}
	var contains []string
	for _, rel := range doc.Relationships {
		if rel.RelationshipType != "CONTAINS" || rel.SpdxElementID != spdxImageID {
			continue
		}
		if _, isFile := fileNames[rel.RelatedSpdxElement]; isFile {
			continue
		}
		contains = append(contains, keyByID[rel.RelatedSpdxElement])
	}
	sort.Strings(contains)
	return set, subject, contains, nil
}

// readAssembledCycloneDx decodes the published CycloneDX document.
func readAssembledCycloneDx(raw []byte) (map[string]assembledComponent, assembledSubject, []string, error) {
	type cdxComponent struct {
		BOMRef     string `json:"bom-ref"`
		Name       string `json:"name"`
		Version    string `json:"version"`
		PackageURL string `json:"purl"`
		Hashes     []struct {
			Alg   string `json:"alg"`
			Value string `json:"content"`
		} `json:"hashes"`
		Licenses []struct {
			License struct {
				ID string `json:"id"`
			} `json:"license"`
			Expression string `json:"expression"`
		} `json:"licenses"`
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
	}
	var doc struct {
		Metadata struct {
			Component cdxComponent `json:"component"`
		} `json:"metadata"`
		Components   []cdxComponent `json:"components"`
		Dependencies []struct {
			Ref       string   `json:"ref"`
			DependsOn []string `json:"dependsOn"`
		} `json:"dependencies"`
	}
	var subject assembledSubject
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, subject, nil, fmt.Errorf("decode the assembled CycloneDX document: %v", err)
	}
	keyOf := func(c cdxComponent) string {
		if c.PackageURL != "" {
			return c.PackageURL
		}
		return c.Name + "@" + c.Version
	}
	subject.Purl = doc.Metadata.Component.PackageURL
	for _, h := range doc.Metadata.Component.Hashes {
		if h.Alg == "SHA-256" {
			subject.Sha256 = h.Value
		}
	}
	set := make(map[string]assembledComponent, len(doc.Components))
	keyByRef := make(map[string]string, len(doc.Components))
	for _, c := range doc.Components {
		comp := assembledComponent{License: noAssertion}
		if len(c.Licenses) > 0 {
			comp.License = c.Licenses[0].License.ID
			if comp.License == "" {
				comp.License = c.Licenses[0].Expression
			}
		}
		for _, f := range c.Components {
			comp.Files = append(comp.Files, f.Name)
		}
		set[keyOf(c)] = comp
		keyByRef[c.BOMRef] = keyOf(c)
	}
	var topLevel []string
	for _, dep := range doc.Dependencies {
		if dep.Ref != doc.Metadata.Component.BOMRef {
			continue
		}
		for _, ref := range dep.DependsOn {
			topLevel = append(topLevel, keyByRef[ref])
		}
	}
	sort.Strings(topLevel)
	return set, subject, topLevel, nil
}

// sameComponents is the assertion that the two formats cannot disagree.
//
// It compares coordinates *and* declared licences, because a component set
// that matches while the licences differ is the same failure wearing a
// different hat. The two documents render from one imageBom, so this holds
// by construction; the check is here so that a future renderer that resolved
// anything of its own goes red rather than shipping two documents a consumer
// has no way to reconcile.
func sameComponents(spdxSet, cdxSet map[string]assembledComponent) error {
	if len(spdxSet) != len(cdxSet) {
		return fmt.Errorf("the SPDX document lists %d components and the CycloneDX document lists %d: %v vs %v",
			len(spdxSet), len(cdxSet), sortedComponentKeys(spdxSet), sortedComponentKeys(cdxSet))
	}
	for key, want := range spdxSet {
		got, ok := cdxSet[key]
		if !ok {
			return fmt.Errorf("the SPDX document lists %s and the CycloneDX document does not: %v", key, sortedComponentKeys(cdxSet))
		}
		if want.License != got.License {
			return fmt.Errorf("the two documents disagree about %s: SPDX declares %q, CycloneDX declares %q", key, want.License, got.License)
		}
		if strings.Join(want.Files, ",") != strings.Join(got.Files, ",") {
			return fmt.Errorf("the two documents disagree about the files in %s: SPDX has %v, CycloneDX has %v", key, want.Files, got.Files)
		}
	}
	return nil
}

func sortedComponentKeys(m map[string]assembledComponent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
