package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"
)

// The generic constructor's tests: an App built from executables the
// archetype did not compile.
//
// Every executable below is produced by calling the `go` module directly,
// which is what makes these tests mean anything. The point of Z5labs.App is
// that a binary from outside this pipeline — somebody else's CLI, a Rust or
// Zig build, a vendor artifact — gets the same hardening, the same
// multi-platform publish, the same annotations and the same attestations, so a
// test that reached for the Go chain to get its input would be testing the
// chain again. Nothing here is a git working tree either; there is no HEAD to
// read, which is the honest shape of a prebuilt payload.
//
// wantEntryMode is the mode an application's own executable lands with. It is
// written out here rather than read from the module for the reason
// wantFileMode is: it is a property of every image this pipeline publishes,
// and a test importing the module's constant would agree with whatever the
// module changed it to.
const wantEntryMode = "555"

// prebuiltEntry compiles dir for platform outside the archetype and returns
// the executable, exactly as a caller with a binary from anywhere else would
// hold one.
func prebuiltEntry(dir *dagger.Directory, name string, platform dagger.Platform) *dagger.File {
	return dag.Go().Build(dir, dagger.GoBuildOpts{
		Pkg:          ".",
		ArtifactName: name,
		Platform:     string(platform),
		DisableCgo:   true,
		Trimpath:     true,
		Strip:        true,
	}).File(name)
}

// prebuiltApp assembles an App from one prebuilt executable per platform,
// each with the document the module's own helper produces for content whose
// ecosystem has none.
//
// The document comes from FileDocument rather than from a literal because that
// is the path an adopter takes, and because it computes the executable's
// SHA-256 itself — which is what the publish checks the document against.
func prebuiltApp(dir *dagger.Directory, name, version string, platforms []dagger.Platform) *dagger.Z5LabsAppBuilder {
	builder := dag.Z5Labs().App(version)
	for _, platform := range platforms {
		entry := prebuiltEntry(dir, name, platform)
		builder = builder.WithVariant(platform, entry, dag.Z5Labs().FileDocument(entry, dagger.Z5LabsFileDocumentOpts{
			License: "Apache-2.0",
			Name:    name,
			Version: version,
		}))
	}
	return builder
}

// AppFromPrebuiltExecutablesIsHardenedLikeAnyOther asserts that an App built
// with no language chain involved produces exactly the image the chain
// produces: the standardized executable directory and PATH, the module's mode
// and ownership on the entry, nothing else in the filesystem, and an
// entrypoint that runs.
//
// The hardening is the whole reason this seam exists rather than being left to
// each project's hand-rolled Dockerfile, so it is asserted rather than assumed.
// A caller-supplied entrypoint is not an exemption from any of it: the module
// still builds the image around the executable, which is exactly what
// distinguishes a bounded seam from the caller-supplied *container* that
// devex#400 refused.
//
// Both platforms are read because packaging only the host's would be invisible
// in a single-platform test and would ship an arm64 release built around an
// amd64 binary — the failure devex#397 names, and the reason no platform here
// is ever inferred from a file.
func (t *Tests) AppFromPrebuiltExecutablesIsHardenedLikeAnyOther(ctx context.Context) error {
	const (
		version = "v1.0.0"
		name    = "hello"
	)
	platforms := []dagger.Platform{"linux/amd64", "linux/arm64"}
	app := prebuiltApp(helloDir(), name, version, platforms).Build()

	entrypoint := "/app/" + name
	for _, platform := range platforms {
		ctr := app.Container(platform)

		got, err := ctr.Entrypoint(ctx)
		if err != nil {
			return fmt.Errorf("%s: read the entrypoint: %v", platform, err)
		}
		if len(got) != 1 || got[0] != entrypoint {
			return fmt.Errorf("%s: entrypoint is %v, want [%s]", platform, got, entrypoint)
		}
		if err := assertStandardEnvironment(ctx, ctr, string(platform)); err != nil {
			return err
		}

		// The executable is owned by the non-root user and is read-and-execute
		// rather than writable, which is the one thing a caller cannot state
		// and the reason the module builds the image rather than accepting one.
		modes, err := statInImage(ctx, ctr, []string{entrypoint})
		if err != nil {
			return fmt.Errorf("%s: %v", platform, err)
		}
		want := wantEntryMode + " " + wantOwner
		if modes[entrypoint] != want {
			return fmt.Errorf("%s: %s is %q, want %q", platform, entrypoint, modes[entrypoint], want)
		}

		// Nothing else is in the image. A seam that quietly brought a shell, a
		// loader or a package database with it would be a bigger surface than
		// the one this pipeline promises, and it would not show up in any
		// assertion above.
		entries, err := ctr.Rootfs().Glob(ctx, "**")
		if err != nil {
			return fmt.Errorf("%s: list the rootfs: %v", platform, err)
		}
		files := onlyFiles(entries)
		if len(files) != 1 || files[0] != strings.TrimPrefix(entrypoint, "/") {
			return fmt.Errorf("%s: the image holds %v, want the contributed executable alone", platform, files)
		}
	}

	// One platform can actually be run here, and running it is what proves the
	// mode and the ownership are compatible with the entrypoint rather than
	// merely correct-looking.
	out, err := app.Container(hostPlatform()).
		WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run the prebuilt app image's entrypoint: %v", err)
	}
	if out != "hello\n" {
		return fmt.Errorf("expected %q, got %q", "hello\n", out)
	}
	return nil
}

// AppFromPrebuiltExecutablesPublishesAttested asserts that an App assembled
// from prebuilt executables publishes exactly like one a language chain
// produced: both SBOMs and the signed provenance attached to the published
// digest, and the image annotated.
//
// This is the criterion the whole story turns on — "published, annotated and
// attested exactly like one Go.App produced" — and it is checked by reading
// the registry rather than the module, because what a consumer can fetch is
// the only thing that counts.
//
// The annotations are asserted in both directions. The version is present,
// because the caller stated it. The revision, the source and the created time
// are *absent*, because nothing observed them: an App assembled from prebuilt
// bytes read no working tree, and a key present and blank would be
// indistinguishable to a consumer from a revision that really is "". The same
// silence shows up in the predicate, which records no source and no package.
func (t *Tests) AppFromPrebuiltExecutablesPublishesAttested(ctx context.Context) error {
	const (
		version    = "v2.0.0"
		name       = "hello"
		repository = "z5labs/prebuilt"
	)
	svc, _, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	platform := hostPlatform()
	app := prebuiltApp(helloDir(), name, version, []dagger.Platform{platform}).Build()
	refs, err := publishable(app, svc, secret, prov).Publish(ctx, []string{repository})
	if err != nil {
		return fmt.Errorf("Publish: %v", err)
	}
	digest, err := digestOf(refs[0])
	if err != nil {
		return err
	}
	registry := testRegistry(svc, secret)

	for _, artifactType := range []string{spdxArtifactType, cycloneDxArtifactType, provenanceArtifactType} {
		found, err := referrersOf(ctx, registry, repository, digest, artifactType)
		if err != nil {
			return err
		}
		if len(found) != 1 {
			return fmt.Errorf("expected exactly 1 referrer of type %s on %s, got %d", artifactType, digest, len(found))
		}
	}

	// The document describes the image and says the image contains the
	// executable that was handed in, which is the assembly working over a
	// contribution no language chain produced.
	spdxRaw, _, err := attachedSbom(ctx, registry, repository, digest, spdxArtifactType)
	if err != nil {
		return err
	}
	spdx, err := readPublishedSpdx(spdxRaw)
	if err != nil {
		return err
	}
	if hexDigest := strings.TrimPrefix(digest, "sha256:"); spdx.SubjectSha256 != hexDigest {
		return fmt.Errorf("the SPDX document's subject carries checksum %q, want the published digest %s",
			spdx.SubjectSha256, hexDigest)
	}
	if len(spdx.Contained) != 1 || spdx.Contained[0] != name {
		return fmt.Errorf("the SPDX document says the image contains %v, want the prebuilt executable alone", spdx.Contained)
	}

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
	if err := assertNoSourceClaimed(statement); err != nil {
		return err
	}

	index, err := fetchManifest(ctx, registry, repository, digest)
	if err != nil {
		return err
	}
	// A single-platform publish stores an index naming one variant or a bare
	// manifest, and which one is the registry's business; the annotations are
	// on the variant either way.
	variants := []string{digest}
	if len(index.Manifests) > 0 {
		variants = nil
		for _, entry := range index.Manifests {
			variants = append(variants, entry.Digest)
		}
	}
	for _, reference := range variants {
		got, err := fetchManifest(ctx, registry, repository, reference)
		if err != nil {
			return err
		}
		if want := version; got.Annotations["org.opencontainers.image.version"] != want {
			return fmt.Errorf("%s: expected the version annotation %q, got %q",
				reference, want, got.Annotations["org.opencontainers.image.version"])
		}
		for _, key := range []string{
			"org.opencontainers.image.revision",
			"org.opencontainers.image.source",
			"org.opencontainers.image.created",
		} {
			if value, ok := got.Annotations[key]; ok {
				return fmt.Errorf(
					"%s: expected no %s annotation on an app assembled from prebuilt executables, got %q; "+
						"nothing observed a source, and a blank key reads as an empty value rather than as an absent one",
					reference, key, value)
			}
		}
	}
	return nil
}

// assertNoSourceClaimed checks that a predicate for an App assembled from
// prebuilt executables claims no source tree and no package.
//
// It is the other half of "build identity is not an argument". The constructor
// takes no commit and no origin, so the honest predicate says nothing about
// either — and a future seam that let a caller state one would go red here
// rather than shipping a provenance that records what somebody typed.
func assertNoSourceClaimed(statement map[string]any) error {
	predicate, err := object(statement["predicate"], "predicate")
	if err != nil {
		return err
	}
	definition, err := object(predicate["buildDefinition"], "predicate.buildDefinition")
	if err != nil {
		return err
	}
	if got, ok := definition["resolvedDependencies"]; ok && got != nil {
		return fmt.Errorf("expected no resolvedDependencies for an app assembled from prebuilt executables, got %v", got)
	}
	internal, err := object(definition["internalParameters"], "predicate.buildDefinition.internalParameters")
	if err != nil {
		return err
	}
	if got, ok := internal["pkg"]; ok {
		return fmt.Errorf("expected no pkg in the predicate for an app that compiled nothing, got %v", got)
	}
	return nil
}

// AppFromPrebuiltComposesATreeShapedPayload asserts the payload shape a
// language chain cannot produce: an executable plus the files it needs to run.
//
// This is what devex#410 exists to unblock. The contribution seam was designed
// around payloads that might be a *tree* — an entry and the content beside it
// — and the reason the single-file assumption looked safe for so long is that
// Go was the only chain there was, so every payload was one static binary.
// With the generic constructor the suite can assemble a tree payload and put
// the composition, the collision refusal and the exec check over it, without
// waiting for a second language chain to exist.
//
// The fixture is an executable that is *useless* without the tree: it lists
// what it found and reads a file out of it, so an image where the entry landed
// and the tree did not fails here rather than passing an exec check that only
// proves the binary starts.
func (t *Tests) AppFromPrebuiltComposesATreeShapedPayload(ctx context.Context) error {
	const (
		version  = "v1.2.3"
		name     = "payload"
		treePath = "/srv/payload"
	)
	platform := hostPlatform()
	tree := dag.Directory().
		WithNewFile("greeting.txt", "hello from the payload\n").
		WithNewFile("data/rows.csv", "a,b\n1,2\n")
	app := prebuiltApp(payloadDir(), name, version, []dagger.Platform{platform}).Build().
		WithDirectory(treePath, tree, dag.Z5Labs().DirectoryDocument(tree, treePath))

	// Composition: the entry and everything it needs are both in the image,
	// and the program says so by reading them.
	out, err := app.Container(platform).
		WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run the tree-payload image: %v", err)
	}
	const want = "hello from the payload data/rows.csv,greeting.txt\n"
	if out != want {
		return fmt.Errorf("expected %q, got %q", want, out)
	}

	// The tree lands with the module's own mode and owner, exactly as it does
	// on an app a language chain built. Nothing about the payload arriving
	// prebuilt relaxes that.
	entrypoint := "/app/" + name
	modes, err := statInImage(ctx, app.Container(platform), []string{
		entrypoint, treePath, treePath + "/greeting.txt", treePath + "/data/rows.csv",
	})
	if err != nil {
		return err
	}
	wantModes := map[string]string{
		entrypoint:                  wantEntryMode + " " + wantOwner,
		treePath:                    wantDirectoryMode + " " + wantOwner,
		treePath + "/greeting.txt":  wantDirectoryMode + " " + wantOwner,
		treePath + "/data/rows.csv": wantDirectoryMode + " " + wantOwner,
	}
	var wrong []string
	for _, path := range sortedNames(wantModes) {
		if modes[path] != wantModes[path] {
			wrong = append(wrong, fmt.Sprintf("%s is %q, want %q", path, modes[path], wantModes[path]))
		}
	}
	if len(wrong) > 0 {
		return fmt.Errorf("%s", strings.Join(wrong, "; "))
	}

	// The collision refusal reaches a prebuilt entry too. The entrypoint is
	// read off the container rather than assumed by the module, so this is the
	// end-to-end half of a rule whose table lives in
	// Z5labs.ContributionPathSelfTest — and the case that matters here is that
	// the protected path is one no language chain named.
	stray := dag.Directory().WithNewFile("stray", "stray\n").File("stray")
	strayDoc := dag.Z5Labs().FileDocument(stray, dagger.Z5LabsFileDocumentOpts{Name: "stray"})
	for _, collision := range []struct {
		path string
		hit  string
		why  string
	}{
		{path: entrypoint, hit: "already there", why: "on top of the prebuilt entrypoint itself"},
		{path: treePath + "/greeting.txt", hit: "inside " + treePath, why: "inside the contributed payload tree"},
	} {
		if _, err := app.WithFile(collision.path, stray, strayDoc).ID(ctx); err == nil {
			return fmt.Errorf("expected contributing %s (%s) to be refused, got nil", collision.path, collision.why)
		} else if !strings.Contains(err.Error(), collision.hit) {
			return fmt.Errorf("expected the refusal of %s (%s) to carry %q, got: %v",
				collision.path, collision.why, collision.hit, err)
		}
	}
	return nil
}

// AppBuilderRefusesAnInconsistentVariantSet asserts the refusals that make an
// App from prebuilt executables trustworthy, through the real API.
//
// The rules themselves are a table in Z5labs.VariantSetSelfTest, which costs
// no build. What cannot be checked in process is that they are wired into
// WithVariant and Build at all — and the empty set especially, because an App
// with no variants is the one that would swallow every contribution made to it
// in silence and publish nothing.
func (t *Tests) AppBuilderRefusesAnInconsistentVariantSet(ctx context.Context) error {
	const (
		version = "v1.0.0"
		name    = "hello"
	)
	amd64 := prebuiltEntry(helloDir(), name, "linux/amd64")
	amd64Doc := dag.Z5Labs().FileDocument(amd64, dagger.Z5LabsFileDocumentOpts{Name: name})
	other := prebuiltEntry(helloDir(), "hello-arm", "linux/arm64")
	otherDoc := dag.Z5Labs().FileDocument(other, dagger.Z5LabsFileDocumentOpts{Name: "hello-arm"})

	cases := []struct {
		builder *dagger.Z5LabsAppBuilder
		refusal string
		why     string
	}{
		{
			builder: dag.Z5Labs().App(version),
			refusal: "at least one variant",
			why:     "an app with no executables at all, which would publish nothing and swallow every contribution",
		},
		{
			builder: dag.Z5Labs().App(version).
				WithVariant("linux/amd64", amd64, amd64Doc).
				WithVariant("linux/amd64", amd64, amd64Doc),
			refusal: "already contributed",
			why:     "one platform twice, where the second silently replaces the first",
		},
		{
			builder: dag.Z5Labs().App(version).
				WithVariant("linux/amd64", amd64, amd64Doc).
				WithVariant("linux/arm64", other, otherDoc),
			refusal: "is called hello",
			why:     "entries named differently, leaving the entrypoint a different path per architecture",
		},
		// A malformed platform is deliberately absent from this table, and the
		// absence is a finding rather than an omission. Platform is a scalar
		// the engine normalizes containerd-style before a module sees it, so a
		// bare "linux" arrives as "linux/amd64" and is accepted — the module's
		// own shape check never sees a value it could refuse. Asserting a
		// refusal here would have been asserting against the engine, and the
		// check that remains in the module is the guard for the callers inside
		// it, where no normalization happens.
		{
			builder: dag.Z5Labs().App("1.0.0+build.1").WithVariant("linux/amd64", amd64, amd64Doc),
			refusal: "build metadata",
			why:     "a version that cannot be an image tag, refused by the terminal like any other",
		},
	}
	for _, c := range cases {
		_, err := c.builder.Build().ID(ctx)
		if err == nil {
			return fmt.Errorf("expected %s to be refused, got nil", c.why)
		}
		if !strings.Contains(err.Error(), c.refusal) {
			return fmt.Errorf("expected the refusal of %s to carry %q, got: %v", c.why, c.refusal, err)
		}
	}
	return nil
}

// AppRefusesADocumentThatDescribesOtherBytes asserts a publish refuses a
// contribution whose document names some other artifact's digest.
//
// This is the mitigation that makes an asserted document worth having. A
// language chain's document is derived from the artifact and cannot disagree
// with it; everything the generic constructor and the contribution helpers
// accept is a claim someone made, and before this a claim about entirely
// different bytes was well-formed, attached cleanly and indistinguishable from
// a right one. The refusal happens before anything is pushed, which is checked
// by looking for the tag afterwards — a document verified after the push would
// leave a published release to be retracted rather than a publish that failed.
func (t *Tests) AppRefusesADocumentThatDescribesOtherBytes(ctx context.Context) error {
	const (
		version    = "v4.0.0"
		name       = "hello"
		repository = "z5labs/mismatched"
	)
	svc, pwdHex, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	platform := hostPlatform()

	// The document is a real, well-formed SPDX document produced by the
	// module's own helper — it is simply about a different file. That is the
	// case worth refusing: a malformed document was already refused, and a
	// document nobody could parse was never the risk.
	elsewhere := dag.Directory().WithNewFile("elsewhere", "not what is in the image\n").File("elsewhere")
	entry := prebuiltEntry(helloDir(), name, platform)
	app := dag.Z5Labs().App(version).
		WithVariant(platform, entry, dag.Z5Labs().FileDocument(elsewhere, dagger.Z5LabsFileDocumentOpts{Name: name})).
		Build()

	_, err = publishable(app, svc, secret, prov).Publish(ctx, []string{repository})
	if err == nil {
		return fmt.Errorf("expected a publish whose document describes other bytes to be refused, got nil")
	}
	if !strings.Contains(err.Error(), "describes other bytes") {
		return fmt.Errorf("expected the refusal to say the document describes other bytes, got: %v", err)
	}

	code, err := curlProbeManifest(ctx, svc, registryAlias, "ci", pwdHex, repository, version)
	if err != nil {
		return fmt.Errorf("probe the registry after the refusal: %v", err)
	}
	if code == 200 {
		return fmt.Errorf("Publish refused the publish but manifest %s:%s is present in the registry", repository, version)
	}
	return nil
}

// payloadDir returns the tree-shaped-payload fixture: a main package that is
// useless without the files contributed beside it.
func payloadDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/payload")
}
