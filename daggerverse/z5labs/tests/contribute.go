package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"dagger/tests/internal/dagger"
)

// The mode and owner every contribution lands with. Written out here rather
// than read from the module for the reason wantImagePath is: they are a
// property of every image this pipeline publishes, and a test that imported
// the module's constants would agree with whatever the module changed them to.
const (
	wantFileMode      = "444"
	wantDirectoryMode = "555"
	wantOwner         = "65532:65532"
)

// A published image has no shell, so the mode and owner of what was
// contributed can only be read by mounting its rootfs into something that has
// one. alpineImage, pinned beside the other fixtures in main.go, is that.

// contributionFixture is the content the contribution tests put into an image
// and the documents describing it.
type contributionFixture struct {
	FilePath     string
	FileContents string
	FileDocument *dagger.File
	File         *dagger.File

	DirPath     string
	Dir         *dagger.Directory
	DirDocument *dagger.File
	// DirFiles are the tree's files, relative to DirPath.
	DirFiles []string
}

// newContributionFixture builds a CA bundle and a template tree, each with the
// document the module's own helpers produce for content that has no ecosystem.
//
// The documents come from FileDocument and DirectoryDocument rather than from
// a literal, because that is the path an adopter takes: a caller who has to
// hand-write SPDX to ship a certificate bundle writes something worthless, and
// a worthless document is worse than an absent one.
//
// The content arrives world-writable, which is the case the module's promise
// actually names — "a caller contributing a world-writable file, or one owned
// by root". Left at dag.Directory()'s defaults the mode assertions would only
// be proving 0644→0444 and 0755→0555, so a regression that passed the caller's
// mode straight through would still go red, but only for modes *less*
// permissive than the module's. The dangerous direction is the other one, and
// 0777 is what makes it load bearing.
func newContributionFixture(bundle string) *contributionFixture {
	file := dag.Directory().
		WithNewFile("ca-certificates.crt", bundle, dagger.DirectoryWithNewFileOpts{Permissions: 0o777}).
		File("ca-certificates.crt")
	tree := dag.Directory().
		WithNewFile("index.html", "<!doctype html>\n", dagger.DirectoryWithNewFileOpts{Permissions: 0o777}).
		WithNewFile("partials/nav.html", "<nav></nav>\n")
	return &contributionFixture{
		FilePath:     "/etc/ssl/certs/ca-certificates.crt",
		FileContents: bundle,
		File:         file,
		FileDocument: dag.Z5Labs().FileDocument(file, dagger.Z5LabsFileDocumentOpts{
			License: "MPL-2.0",
			Name:    "/etc/ssl/certs/ca-certificates.crt",
			Version: "2026.01.01",
		}),

		DirPath:     "/srv/templates",
		Dir:         tree,
		DirDocument: dag.Z5Labs().DirectoryDocument(tree, "/srv/templates"),
		DirFiles:    []string{"index.html", "partials/nav.html"},
	}
}

// contribute puts both of the fixture's contributions into app.
func (f *contributionFixture) contribute(app *dagger.Z5LabsApp) *dagger.Z5LabsApp {
	return app.
		WithFile(f.FilePath, f.File, f.FileDocument).
		WithDirectory(f.DirPath, f.Dir, f.DirDocument)
}

// AppContributionsLandInEveryVariant asserts that contributed content reaches
// every platform's image with the module's own mode and owner, and that an app
// nobody contributed to is untouched by the seam existing.
//
// Both halves are the point. Contributing to the host's variant alone would be
// invisible in a single-platform test and would ship an arm64 release missing
// its certificate bundle, so two platforms are built and both are read. And
// the mode and the owner are checked rather than assumed because they are the
// one thing a caller cannot state: the helpers take no permission and no owner
// argument precisely so that every image this pipeline publishes answers those
// two questions the same way, and a helper that passed the supplied content's
// mode through would satisfy every other assertion here.
//
// The metadata is read by mounting each variant's rootfs into an image that
// has a shell, because a scratch image has none. Mounted rather than copied:
// a copy is a second chance for ownership to be rewritten, which is the exact
// property under test.
func (t *Tests) AppContributionsLandInEveryVariant(ctx context.Context) error {
	const version = "v1.0.0"
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	platforms := []dagger.Platform{"linux/amd64", "linux/arm64"}
	fixture := newContributionFixture("-----BEGIN CERTIFICATE-----\nnot really a certificate\n-----END CERTIFICATE-----\n")

	plain := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: platforms})
	custom := fixture.contribute(plain)

	for _, platform := range platforms {
		ctr := custom.Container(platform)

		contents, err := ctr.File(fixture.FilePath).Contents(ctx)
		if err != nil {
			return fmt.Errorf("%s: read %s: %v", platform, fixture.FilePath, err)
		}
		if contents != fixture.FileContents {
			return fmt.Errorf("%s: %s holds %q, want the contributed bytes", platform, fixture.FilePath, contents)
		}
		for _, name := range fixture.DirFiles {
			if _, err := ctr.File(fixture.DirPath + "/" + name).Contents(ctx); err != nil {
				return fmt.Errorf("%s: read %s/%s: %v", platform, fixture.DirPath, name, err)
			}
		}

		// The whole tree is read in one exec, so a mode that is right on the
		// directory and wrong on a file inside it cannot pass.
		paths := []string{fixture.FilePath, fixture.DirPath}
		for _, name := range fixture.DirFiles {
			paths = append(paths, fixture.DirPath+"/"+name)
		}
		// The directories the copy had to create on the way are read too. They
		// are the image's structure rather than the caller's content, so they
		// are root-owned and 0755 — the layout a real base image would already
		// have, and deliberately not writable by the user the app runs as. A
		// contribution that made its parents world-writable, or owned by the
		// application, would be a hole this test would otherwise not see.
		parents := []string{"/etc", "/etc/ssl", "/etc/ssl/certs", "/srv"}
		modes, err := statInImage(ctx, ctr, append(append([]string{}, paths...), parents...))
		if err != nil {
			return fmt.Errorf("%s: %v", platform, err)
		}
		want := map[string]string{fixture.FilePath: wantFileMode + " " + wantOwner}
		for _, p := range paths[1:] {
			want[p] = wantDirectoryMode + " " + wantOwner
		}
		for _, p := range parents {
			want[p] = "755 0:0"
		}
		paths = append(paths, parents...)
		var wrong []string
		for _, p := range paths {
			if modes[p] != want[p] {
				wrong = append(wrong, fmt.Sprintf("%s is %q, want %q", p, modes[p], want[p]))
			}
		}
		if len(wrong) > 0 {
			return fmt.Errorf("%s: %s", platform, strings.Join(wrong, "; "))
		}
	}

	// An app with no contributions is exactly what the chain built, and stays
	// that way: contributing to one handle must not reach back into the app it
	// was derived from, and the image that app builds must hold the binary and
	// nothing else.
	platform := hostPlatform()
	before, err := plain.Container(platform).Rootfs().Digest(ctx)
	if err != nil {
		return fmt.Errorf("digest the uncustomized rootfs: %v", err)
	}
	entries, err := plain.Container(platform).Rootfs().Glob(ctx, "**")
	if err != nil {
		return fmt.Errorf("list the uncustomized rootfs: %v", err)
	}
	if got := onlyFiles(entries); len(got) != 1 || !strings.HasPrefix(got[0], "app/") {
		return fmt.Errorf("an app with no contributions holds %v, want the built binary alone", got)
	}
	customized, err := custom.Container(platform).Rootfs().Digest(ctx)
	if err != nil {
		return fmt.Errorf("digest the customized rootfs: %v", err)
	}
	if customized == before {
		return fmt.Errorf("the customized image has the same rootfs digest as the uncustomized one, so nothing was contributed")
	}
	after, err := plain.Container(platform).Rootfs().Digest(ctx)
	if err != nil {
		return fmt.Errorf("re-digest the uncustomized rootfs: %v", err)
	}
	if after != before {
		return fmt.Errorf("contributing to an app changed the app it was derived from: %q became %q", before, after)
	}
	return nil
}

// statInImage reports each path's mode and owner inside ctr's filesystem, as
// "<mode> <uid>:<gid>".
func statInImage(ctx context.Context, ctr *dagger.Container, paths []string) (map[string]string, error) {
	args := []string{"stat", "-c", "%n %a %u:%g"}
	for _, p := range paths {
		args = append(args, "/img"+p)
	}
	out, err := dag.Container().From(alpineImage).
		WithMountedDirectory("/img", ctr.Rootfs()).
		WithExec(args).
		Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("stat the image's contents: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		got[strings.TrimPrefix(name, "/img")] = rest
	}
	return got, nil
}

// onlyFiles drops the directory entries a recursive glob reports, so what is
// left is the files an image actually ships.
func onlyFiles(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e, "/") {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// AppRefusesContributionsThatCollide asserts the helpers refuse
// content that would land on top of content already in the image.
//
// The failure being refused is not a broken image — it is a *described* image
// that is wrong. Whatever loses the collision is still in the documents the
// publish attaches, so a consumer reads an SBOM naming two things where the
// image holds one, and nothing about the document says which. The entrypoint
// case is the same failure with the application's own binary on the losing
// side, which would also leave the provenance describing a build whose output
// is no longer in the image.
//
// The rules themselves are a table in Z5labs.ContributionPathSelfTest, which
// costs no build. What is asserted here is the part that cannot be checked in
// process: that they are wired into both helpers, and that the entrypoint they
// protect is the one the image really runs rather than a path this module
// assumed.
func (t *Tests) AppRefusesContributionsThatCollide(ctx context.Context) error {
	const version = "v1.0.0"
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{
		Platforms: []dagger.Platform{hostPlatform()},
	})
	entrypoint, err := app.Container(hostPlatform()).Entrypoint(ctx)
	if err != nil {
		return fmt.Errorf("read the image's entrypoint: %v", err)
	}
	if len(entrypoint) != 1 {
		return fmt.Errorf("expected a single entrypoint to protect, got %v", entrypoint)
	}
	fixture := newContributionFixture("bundle\n")

	cases := []struct {
		name string
		app  *dagger.Z5LabsApp
		want string
	}{
		{
			name: "a file on top of the application's own binary",
			app:  app.WithFile(entrypoint[0], fixture.File, fixture.FileDocument),
			want: entrypoint[0],
		},
		{
			name: "a tree over the directory the binary lives in",
			app:  app.WithDirectory("/app", fixture.Dir, fixture.DirDocument),
			want: entrypoint[0],
		},
		{
			name: "a second file at a path already contributed at",
			app:  app.WithFile(fixture.FilePath, fixture.File, fixture.FileDocument).WithFile(fixture.FilePath, fixture.File, fixture.FileDocument),
			want: fixture.FilePath,
		},
		{
			name: "a file inside a tree already contributed",
			app:  app.WithDirectory(fixture.DirPath, fixture.Dir, fixture.DirDocument).WithFile(fixture.DirPath+"/index.html", fixture.File, fixture.FileDocument),
			want: fixture.DirPath,
		},
		{
			name: "a relative path, which resolves against a working directory this pipeline never sets",
			app:  app.WithFile("etc/hosts", fixture.File, fixture.FileDocument),
			want: "is not an absolute path",
		},
	}
	for _, c := range cases {
		if _, err := c.app.ID(ctx); err == nil {
			return fmt.Errorf("expected %s to be refused, got nil", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			return fmt.Errorf("expected the refusal of %s to name %q, got: %s", c.name, c.want, err.Error())
		}
	}

	// The same paths are accepted when nothing is in the way, so the rule is
	// refusing collisions rather than refusing contributions.
	if _, err := fixture.contribute(app).ID(ctx); err != nil {
		return fmt.Errorf("expected two non-overlapping contributions to be accepted, got: %v", err)
	}
	return nil
}

// AppCustomizedImageStaysAttested asserts that an image a caller contributed
// to is attested exactly as one they did not: the documents account for every
// contribution, the provenance still describes what was published, and the
// annotations are still on the variant.
//
// This is the criterion the whole contribution seam turns on. A publish that
// accepted contributed content and attached documents describing only the
// binary would be well formed, would attach cleanly, and would be
// indistinguishable from a complete one — the same undetectable incompleteness
// that moved the documents' subject from the binary to the image in the first
// place. So the assertion is on what a consumer can fetch: the image CONTAINS
// three things, both formats agree on all three, and the envelope over the
// published digest still verifies against the identity that signed it.
func (t *Tests) AppCustomizedImageStaysAttested(ctx context.Context) error {
	const (
		version    = "v10.0.0"
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
	fixture := newContributionFixture("-----BEGIN CERTIFICATE-----\ncustomized\n-----END CERTIFICATE-----\n")
	app := fixture.contribute(dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{
		Platforms: []dagger.Platform{platform},
	}))
	refs, err := publishable(app, svc, secret, prov).Publish(ctx, []string{repository})
	if err != nil {
		return fmt.Errorf("Publish: %v", err)
	}
	digest, err := digestOf(refs[0])
	if err != nil {
		return err
	}
	registry := testRegistry(svc, secret)

	spdxRaw, _, err := attachedSbom(ctx, registry, repository, digest, spdxArtifactType)
	if err != nil {
		return err
	}
	cdxRaw, _, err := attachedSbom(ctx, registry, repository, digest, cycloneDxArtifactType)
	if err != nil {
		return err
	}
	spdx, err := readPublishedSpdx(spdxRaw)
	if err != nil {
		return err
	}
	cdx, err := readPublishedCycloneDx(cdxRaw)
	if err != nil {
		return err
	}

	// Every contribution is a thing the image CONTAINS, named as it was
	// contributed. The binary is in the list too: a document that replaced the
	// chain's contribution with the caller's would list three things and
	// describe a different image.
	want := []string{"/etc/ssl/certs/ca-certificates.crt", "/srv/templates", "hello"}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	for what, doc := range map[string]publishedSbom{"SPDX": spdx, "CycloneDX": cdx} {
		if strings.Join(doc.Contained, ",") != strings.Join(want, ",") {
			return fmt.Errorf("the %s document says the image contains %v, want %v", what, doc.Contained, want)
		}
		if doc.SubjectSha256 != hexDigest {
			return fmt.Errorf("the %s document's subject carries checksum %q, want the published digest %s",
				what, doc.SubjectSha256, hexDigest)
		}
		for _, name := range want {
			if doc.Components[name] == "" {
				return fmt.Errorf("the %s document lists no checksum for %s; it has %v", what, name, doc.Components)
			}
		}
		// One component's checksum is pinned to the bytes that were actually
		// contributed, rather than merely asserted to exist and to match the
		// other format. Two documents agree on a wrong digest exactly as
		// readily as on a right one, so without this the pair would not catch a
		// document describing something other than what is in the image.
		wantSum := sha256.Sum256([]byte(fixture.FileContents))
		if got := doc.Components[fixture.FilePath]; got != hex.EncodeToString(wantSum[:]) {
			return fmt.Errorf("the %s document gives %s the checksum %q, want %q, which the contributed bytes hash to",
				what, fixture.FilePath, got, hex.EncodeToString(wantSum[:]))
		}
	}
	for name, sum := range spdx.Components {
		if cdx.Components[name] != sum {
			return fmt.Errorf("the two documents disagree about %s: SPDX has %q, CycloneDX has %q",
				name, sum, cdx.Components[name])
		}
	}

	// The provenance describes what was actually published, which is the
	// customized digest and not the image the chain built on its own.
	envelope, err := attachedDocument(ctx, registry, repository, digest, provenanceArtifactType)
	if err != nil {
		return err
	}
	statement, err := verifyEnvelope(envelope, prov.Public)
	if err != nil {
		return err
	}
	if err := checkStatement(statement, digest, repository, prov.Claims); err != nil {
		return err
	}

	// And the annotations survive the customization: they are applied when the
	// image is built, so a contribution that rebuilt the variant rather than
	// adding to it would drop them.
	commitTime, err := headCommitTime(ctx, src)
	if err != nil {
		return err
	}
	image, err := fetchManifest(ctx, registry, repository, digest)
	if err != nil {
		return err
	}
	want4 := map[string]string{
		"org.opencontainers.image.revision": headSha,
		"org.opencontainers.image.source":   fixtureOriginURL,
		"org.opencontainers.image.version":  version,
		"org.opencontainers.image.created":  commitTime,
	}
	for key, value := range want4 {
		if got := image.Annotations[key]; got != value {
			return fmt.Errorf("the customized image's %s is %q, want %q", key, got, value)
		}
	}
	return nil
}

// AppCustomizedImageStillNeedsProvenance asserts that contributing content is
// not a route to publishing without provenance.
//
// It is a test of its own rather than a line in the refusal test above because
// the two failures it rules out are both plausible and both silent: a helper
// that rebuilt the app object could drop the machinery a caller had already
// supplied, and a publish path that took a different branch for a customized
// image could skip the refusal entirely. An image published without an
// attestation is indistinguishable from one published with, so the registry is
// checked afterwards too — the refusal has to happen before the push, not
// after it.
func (t *Tests) AppCustomizedImageStillNeedsProvenance(ctx context.Context) error {
	const (
		version    = "v11.0.0"
		repository = "hello"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	svc, pwdHex, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	fixture := newContributionFixture("bundle\n")
	app := fixture.contribute(dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{
		Platforms: []dagger.Platform{hostPlatform()},
	}))
	_, err = app.
		WithRegistry(registryAlias+":5000", "ci", secret).
		WithRegistryService(svc).
		WithInsecure().
		Publish(ctx, []string{repository})
	if err == nil {
		return fmt.Errorf("expected a customized image to be refused a publish with no id token machinery, got nil")
	}
	if !strings.Contains(err.Error(), "provenance") {
		return fmt.Errorf("expected the refusal to be about provenance, got: %s", err.Error())
	}
	code, err := curlProbeManifest(ctx, svc, registryAlias, "ci", pwdHex, repository, version)
	if err != nil {
		return fmt.Errorf("curl probe: %v", err)
	}
	if code == 200 {
		return fmt.Errorf("Publish refused the publish but manifest %s is present in the registry", version)
	}
	return nil
}
