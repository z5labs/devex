package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"dagger/tests/internal/dagger"
)

// AppSbomsDescribeThePublishedImage asserts that the documents on a
// published digest are about **the image**, and that the two formats agree
// about what is in it.
//
// The criterion this covers moved in devex#409. Before it, the documents
// described the binary a language chain compiled — which was complete only
// because a scratch image holding one static binary *is* its binary, and
// stops being true the moment anything else can enter the image. Three
// things are checked here and none of them could pass under the old shape:
//
//   - the subject is the published digest, so a consumer who pulled
//     `<repo>@<digest>` can tell the document is about the bytes they have
//     rather than about some artifact they cannot see;
//   - the image is a package that CONTAINS what was contributed to it, so
//     attribution survives the assembly that replaced the per-contribution
//     documents;
//   - the SPDX and the CycloneDX list the same components, over a real
//     publish rather than over synthesized input.
//
// It reads the documents back off the registry rather than off the module,
// because what a consumer can check is the only thing that counts: an
// assembly that is right and an attach that pushed something else are the
// same failure from outside.
//
// The multi-source case — an image with contributions from more than one
// place — is AppCustomizedImageStaysAttested, which publishes an image a
// caller contributed a file and a directory to and reads the same documents
// back. The de-duplication and merge rules underneath both are driven directly
// by Z5labs.ImageSbomSelfTest, which says why they are not a publish per case.
func (t *Tests) AppSbomsDescribeThePublishedImage(ctx context.Context) error {
	const (
		version    = "v9.0.0"
		repository = "z5labs/hello"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	headSha, err := headFullSha(ctx, src)
	if err != nil {
		return err
	}
	svc, _, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, headSha)
	if err != nil {
		return err
	}
	platform := hostPlatform()
	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{platform}})
	refs, err := publishable(app, svc, secret, prov).Publish(ctx, []string{repository})
	if err != nil {
		return fmt.Errorf("Publish: %v", err)
	}
	digest, err := digestOf(refs[0])
	if err != nil {
		return err
	}
	registry := testRegistry(svc, secret)

	// The document is selected by artifact type and identified by the title
	// annotation, which is the read path the package doc tells adopters to
	// use. A title that did not name the platform would make the per-platform
	// documents indistinguishable on a real multi-platform release.
	wantTitle := "hello-" + strings.ReplaceAll(string(platform), "/", "-")
	spdxRaw, title, err := attachedSbom(ctx, registry, repository, digest, spdxArtifactType)
	if err != nil {
		return err
	}
	if title != wantTitle+".spdx.json" {
		return fmt.Errorf("the SPDX document is titled %q, want %q", title, wantTitle+".spdx.json")
	}
	cdxRaw, title, err := attachedSbom(ctx, registry, repository, digest, cycloneDxArtifactType)
	if err != nil {
		return err
	}
	if title != wantTitle+".cdx.json" {
		return fmt.Errorf("the CycloneDX document is titled %q, want %q", title, wantTitle+".cdx.json")
	}

	spdx, err := readPublishedSpdx(spdxRaw)
	if err != nil {
		return err
	}
	cdx, err := readPublishedCycloneDx(cdxRaw)
	if err != nil {
		return err
	}

	hexDigest := strings.TrimPrefix(digest, "sha256:")
	for what, doc := range map[string]publishedSbom{"SPDX": spdx, "CycloneDX": cdx} {
		if doc.SubjectSha256 != hexDigest {
			return fmt.Errorf("the %s document's subject carries checksum %q, want the published digest %s",
				what, doc.SubjectSha256, hexDigest)
		}
		if !strings.Contains(doc.SubjectPurl, digest) || !strings.Contains(doc.SubjectPurl, repository) {
			return fmt.Errorf("the %s document's subject is %q, which does not name %s@%s",
				what, doc.SubjectPurl, repository, digest)
		}
		// The binary is in the image, and it is in it as something the image
		// CONTAINS rather than as the document's subject.
		if len(doc.Contained) != 1 || doc.Contained[0] != "hello" {
			return fmt.Errorf("the %s document says the image contains %v, want the one binary", what, doc.Contained)
		}
		if doc.Components["hello"] == "" {
			return fmt.Errorf("the %s document lists no checksum for the binary; it has %v", what, doc.Components)
		}
	}

	// The two formats cannot disagree, over a real publish. They render from
	// one resolution, so this holds by construction; the check is what makes
	// a future renderer that resolved anything of its own go red.
	if len(spdx.Components) != len(cdx.Components) {
		return fmt.Errorf("the SPDX document lists %v and the CycloneDX document lists %v",
			sortedNames(spdx.Components), sortedNames(cdx.Components))
	}
	for name, sum := range spdx.Components {
		if cdx.Components[name] != sum {
			return fmt.Errorf("the two documents disagree about %s: SPDX has checksum %q, CycloneDX has %q",
				name, sum, cdx.Components[name])
		}
	}
	return nil
}

// publishedSbom is what either format says, reduced to the things both
// express: the subject, what the image contains, and every component with
// its SHA-256.
type publishedSbom struct {
	SubjectSha256 string
	SubjectPurl   string
	Contained     []string
	Components    map[string]string
}

// attachedSbom fetches one attached document and the title it was attached
// under.
func attachedSbom(ctx context.Context, registry *dagger.OciRegistry, repository, subject, artifactType string) ([]byte, string, error) {
	found, err := referrersOf(ctx, registry, repository, subject, artifactType)
	if err != nil {
		return nil, "", err
	}
	if len(found) != 1 {
		return nil, "", fmt.Errorf("expected 1 referrer of type %s on %s, got %d", artifactType, subject, len(found))
	}
	referrer, err := fetchManifest(ctx, registry, repository, found[0].Digest)
	if err != nil {
		return nil, "", err
	}
	if len(referrer.Layers) != 1 {
		return nil, "", fmt.Errorf("expected the %s referrer to hold 1 layer, got %d", artifactType, len(referrer.Layers))
	}
	contents, err := registry.Fetch(repository, referrer.Layers[0].Digest).Contents(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s payload: %v", artifactType, err)
	}
	return []byte(contents), referrer.Layers[0].Annotations["org.opencontainers.image.title"], nil
}

// readPublishedSpdx decodes an attached SPDX document the way a consumer
// would: by finding what the document DESCRIBES and following the
// relationships out of it.
func readPublishedSpdx(raw []byte) (publishedSbom, error) {
	var doc struct {
		Packages []struct {
			SPDXID      string `json:"SPDXID"`
			Name        string `json:"name"`
			VersionInfo string `json:"versionInfo"`
			Checksums   []struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"checksumValue"`
			} `json:"checksums"`
			ExternalRefs []struct {
				Type    string `json:"referenceType"`
				Locator string `json:"referenceLocator"`
			} `json:"externalRefs"`
		} `json:"packages"`
		Relationships []struct {
			SpdxElementID      string `json:"spdxElementId"`
			RelatedSpdxElement string `json:"relatedSpdxElement"`
			RelationshipType   string `json:"relationshipType"`
		} `json:"relationships"`
	}
	out := publishedSbom{Components: map[string]string{}}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out, fmt.Errorf("decode the published SPDX document: %v", err)
	}
	subjectID := ""
	for _, rel := range doc.Relationships {
		if rel.RelationshipType == "DESCRIBES" && rel.SpdxElementID == "SPDXRef-DOCUMENT" {
			if subjectID != "" {
				return out, fmt.Errorf("the published SPDX document describes more than one element")
			}
			subjectID = rel.RelatedSpdxElement
		}
	}
	if subjectID == "" {
		return out, fmt.Errorf("the published SPDX document describes nothing")
	}
	nameByID := map[string]string{}
	for _, pkg := range doc.Packages {
		nameByID[pkg.SPDXID] = pkg.Name
		var sum string
		for _, c := range pkg.Checksums {
			if c.Algorithm == "SHA256" {
				sum = c.Value
			}
		}
		if pkg.SPDXID == subjectID {
			out.SubjectSha256 = sum
			for _, ref := range pkg.ExternalRefs {
				if ref.Type == "purl" {
					out.SubjectPurl = ref.Locator
				}
			}
			continue
		}
		out.Components[pkg.Name] = sum
	}
	for _, rel := range doc.Relationships {
		if rel.RelationshipType == "CONTAINS" && rel.SpdxElementID == subjectID {
			out.Contained = append(out.Contained, nameByID[rel.RelatedSpdxElement])
		}
	}
	sort.Strings(out.Contained)
	return out, nil
}

// readPublishedCycloneDx decodes an attached CycloneDX document the same
// way: the metadata component is the subject, and the dependencies out of
// it are what the image contains.
func readPublishedCycloneDx(raw []byte) (publishedSbom, error) {
	type component struct {
		BOMRef     string `json:"bom-ref"`
		Name       string `json:"name"`
		PackageURL string `json:"purl"`
		Hashes     []struct {
			Alg   string `json:"alg"`
			Value string `json:"content"`
		} `json:"hashes"`
	}
	var doc struct {
		Metadata struct {
			Component component `json:"component"`
		} `json:"metadata"`
		Components   []component `json:"components"`
		Dependencies []struct {
			Ref       string   `json:"ref"`
			DependsOn []string `json:"dependsOn"`
		} `json:"dependencies"`
	}
	out := publishedSbom{Components: map[string]string{}}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out, fmt.Errorf("decode the published CycloneDX document: %v", err)
	}
	out.SubjectPurl = doc.Metadata.Component.PackageURL
	for _, h := range doc.Metadata.Component.Hashes {
		if h.Alg == "SHA-256" {
			out.SubjectSha256 = h.Value
		}
	}
	nameByRef := map[string]string{}
	for _, c := range doc.Components {
		nameByRef[c.BOMRef] = c.Name
		var sum string
		for _, h := range c.Hashes {
			if h.Alg == "SHA-256" {
				sum = h.Value
			}
		}
		out.Components[c.Name] = sum
	}
	for _, dep := range doc.Dependencies {
		if dep.Ref != doc.Metadata.Component.BOMRef {
			continue
		}
		for _, ref := range dep.DependsOn {
			out.Contained = append(out.Contained, nameByRef[ref])
		}
	}
	sort.Strings(out.Contained)
	return out, nil
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AppDocumentHelpersDescribeContentWithNoEcosystem asserts that the two
// helpers a caller reaches for when their content has no ecosystem module
// produce a document that says something true about it.
//
// They exist because of a burden argument rather than a capability one:
// SPDX can describe a CA bundle as a package with a checksum and
// NOASSERTION, and always could. What decides whether the documents on a
// published image are worth anything is whether producing one for a
// certificate bundle is a chore — because a caller who finds it one will
// supply whatever satisfies the signature, and a worthless document is worse
// than an absent one, being indistinguishable from a real one.
//
// So this checks the two things that make the helpers not-a-chore: they
// compute the digests themselves, and the only thing they ask for is the one
// thing they cannot know. It also checks that a licence has to be an
// expression, which is the other half of the same argument — a field a
// consumer matches against a policy must not be able to hold prose.
func (t *Tests) AppDocumentHelpersDescribeContentWithNoEcosystem(ctx context.Context) error {
	const bundle = "-----BEGIN CERTIFICATE-----\nnot really a certificate\n-----END CERTIFICATE-----\n"
	wantSum := sha256.Sum256([]byte(bundle))

	file := dag.Directory().WithNewFile("ca-certificates.crt", bundle).File("ca-certificates.crt")
	raw, err := dag.Z5Labs().FileDocument(file, dagger.Z5LabsFileDocumentOpts{License: "MPL-2.0"}).Contents(ctx)
	if err != nil {
		return fmt.Errorf("FileDocument: %v", err)
	}
	doc, err := readContributionDocument([]byte(raw))
	if err != nil {
		return err
	}
	if doc.Name != "ca-certificates.crt" {
		return fmt.Errorf("the file document describes %q, want the file's own name", doc.Name)
	}
	if doc.Sha256 != hex.EncodeToString(wantSum[:]) {
		return fmt.Errorf("the file document carries checksum %q, want %q the file's bytes hash to",
			doc.Sha256, hex.EncodeToString(wantSum[:]))
	}
	if doc.LicenseDeclared != "MPL-2.0" || doc.LicenseConcluded != "MPL-2.0" {
		return fmt.Errorf("a stated licence should be declared and concluded, got %q and %q",
			doc.LicenseDeclared, doc.LicenseConcluded)
	}
	if len(doc.Files) != 0 {
		return fmt.Errorf("the file document enumerates %v; a package that is one file is its own checksum", doc.Files)
	}

	// A directory is enumerated, because "the contribution is described" and
	// "every file in the image is accounted for" are different promises and
	// only the second one is the point.
	tree := dag.Directory().
		WithNewFile("templates/index.html", "<!doctype html>").
		WithNewFile("templates/partials/nav.html", "<nav></nav>")
	raw, err = dag.Z5Labs().DirectoryDocument(tree, "templates").Contents(ctx)
	if err != nil {
		return fmt.Errorf("DirectoryDocument: %v", err)
	}
	doc, err = readContributionDocument([]byte(raw))
	if err != nil {
		return err
	}
	if !doc.FilesAnalyzed || doc.VerificationCode == "" {
		return fmt.Errorf("a directory document should analyze its files and carry a verification code, got %#v", doc)
	}
	wantFiles := []string{"templates/index.html", "templates/partials/nav.html"}
	if strings.Join(doc.Files, ",") != strings.Join(wantFiles, ",") {
		return fmt.Errorf("the directory document lists %v, want %v", doc.Files, wantFiles)
	}
	if doc.LicenseDeclared != "NOASSERTION" || !strings.Contains(doc.LicenseComment, "stated no licence") {
		return fmt.Errorf("an unstated licence should be NOASSERTION and say so, got %q / %q",
			doc.LicenseDeclared, doc.LicenseComment)
	}

	// A tree with nothing in it is refused rather than described as an
	// empty package. "This contribution has no files" and "this contribution
	// was not looked at" are the same document, and the second one is the
	// artifact the whole mechanism exists to prevent.
	if _, err := dag.Z5Labs().DirectoryDocument(dag.Directory(), "empty").Contents(ctx); err == nil {
		return fmt.Errorf("expected an empty directory to be refused, got nil")
	}

	// A symlink is skipped rather than followed or hashed. Following it
	// would put a digest in the document for bytes that are not in the
	// image; hashing the link itself would put one nothing can verify
	// against a pulled layer. The real file beside it still lands.
	linked := dag.Container().From("alpine:3.22").
		WithNewFile("/tree/real.txt", "real").
		WithExec([]string{"ln", "-s", "real.txt", "/tree/link.txt"}).
		Directory("/tree")
	raw, err = dag.Z5Labs().DirectoryDocument(linked, "tree").Contents(ctx)
	if err != nil {
		return fmt.Errorf("DirectoryDocument over a tree with a symlink: %v", err)
	}
	doc, err = readContributionDocument([]byte(raw))
	if err != nil {
		return err
	}
	if strings.Join(doc.Files, ",") != "real.txt" {
		return fmt.Errorf("the directory document lists %v, want the regular file alone", doc.Files)
	}

	// Prose is refused rather than published into a field a policy engine
	// reads as an identifier.
	if _, err := dag.Z5Labs().FileDocument(file, dagger.Z5LabsFileDocumentOpts{
		License: "Copyright (c) 2026 Example, Inc.",
	}).Contents(ctx); err == nil {
		return fmt.Errorf("expected a prose licence to be refused, got nil")
	}
	return nil
}

// contributionDocumentView is the part of a one-package contribution
// document these assertions read.
type contributionDocumentView struct {
	Name             string
	Sha256           string
	LicenseDeclared  string
	LicenseConcluded string
	LicenseComment   string
	FilesAnalyzed    bool
	VerificationCode string
	Files            []string
}

// readContributionDocument decodes a helper-produced document and checks the
// shape every contribution has to have: exactly one package, and a DESCRIBES
// relationship naming it.
func readContributionDocument(raw []byte) (contributionDocumentView, error) {
	var doc struct {
		SPDXVersion string `json:"spdxVersion"`
		Packages    []struct {
			SPDXID                  string `json:"SPDXID"`
			Name                    string `json:"name"`
			FilesAnalyzed           bool   `json:"filesAnalyzed"`
			LicenseDeclared         string `json:"licenseDeclared"`
			LicenseConcluded        string `json:"licenseConcluded"`
			LicenseComments         string `json:"licenseComments"`
			PackageVerificationCode struct {
				Value string `json:"packageVerificationCodeValue"`
			} `json:"packageVerificationCode"`
			Checksums []struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"checksumValue"`
			} `json:"checksums"`
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
	var out contributionDocumentView
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out, fmt.Errorf("decode the contribution document: %v", err)
	}
	if doc.SPDXVersion != "SPDX-2.3" {
		return out, fmt.Errorf("the contribution document is %q, want SPDX-2.3", doc.SPDXVersion)
	}
	if len(doc.Packages) != 1 {
		return out, fmt.Errorf("the contribution document holds %d packages, want exactly the one thing it describes", len(doc.Packages))
	}
	pkg := doc.Packages[0]
	describes := 0
	for _, rel := range doc.Relationships {
		if rel.RelationshipType == "DESCRIBES" && rel.SpdxElementID == "SPDXRef-DOCUMENT" && rel.RelatedSpdxElement == pkg.SPDXID {
			describes++
		}
	}
	if describes != 1 {
		return out, fmt.Errorf("the contribution document does not DESCRIBE its one package")
	}
	out = contributionDocumentView{
		Name:             pkg.Name,
		LicenseDeclared:  pkg.LicenseDeclared,
		LicenseConcluded: pkg.LicenseConcluded,
		LicenseComment:   pkg.LicenseComments,
		FilesAnalyzed:    pkg.FilesAnalyzed,
		VerificationCode: pkg.PackageVerificationCode.Value,
	}
	for _, c := range pkg.Checksums {
		if c.Algorithm == "SHA256" {
			out.Sha256 = c.Value
		}
	}
	// A file the package does not CONTAIN is not this package's file, which
	// is the distinction the assembler makes too.
	names := map[string]string{}
	for _, f := range doc.Files {
		names[f.SPDXID] = f.FileName
	}
	for _, rel := range doc.Relationships {
		if rel.RelationshipType != "CONTAINS" || rel.SpdxElementID != pkg.SPDXID {
			continue
		}
		if name, ok := names[rel.RelatedSpdxElement]; ok {
			out.Files = append(out.Files, name)
		}
	}
	sort.Strings(out.Files)
	return out, nil
}
