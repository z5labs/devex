package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"
)

// Composing one application's payload into another's image.
//
// The self-tests on the module drive the rules — which payloads may be
// composed, which platform sets pair, which environments cannot coexist, and
// what a failure to start looks like on stderr. What is here is the half those
// cannot reach: that the rules really are wired into WithApp, that a composed
// payload lands where the plugin directory promises and runs there, that a
// complete application's own files come along at their own paths, and that the
// derived image is published, annotated and attested like any other.
//
// The composed applications are assembled with the generic constructor rather
// than the Go chain, and that is deliberate: the shape this seam exists for is
// a base CLI plus a generator per language, and the generators are routinely
// artifacts somebody else built. Nothing here gives the Go chain a special
// path into composition, because it does not have one.
//
// wantPluginName is the file name every plugin fixture is composed under, and
// wantComposedEntry is where it has to land. The path is written out rather
// than assembled from the module's constants for the reason wantImagePath is:
// it is the contract a base image's plugin discovery is written against, and a
// test that imported the module's constant would agree with whatever the
// module changed it to.
const (
	wantPluginName    = "gen"
	wantComposedEntry = "/usr/local/bin/gen"
)

// composedPlugin assembles the plugin fixture as an App, one prebuilt
// executable per platform, ready to be composed into a base.
func composedPlugin(name, version string, platforms []dagger.Platform) *dagger.Z5LabsApp {
	return prebuiltApp(pluginDir(), name, version, platforms).Build()
}

// AppComposesAnotherAppsPayload asserts that a composed payload lands in the
// plugin directory of every platform's image, with the module's own mode and
// ownership, and that the derived image is otherwise the base unchanged.
//
// Both halves are the point, and the second is the one this seam exists to
// protect. A derived image that quietly became a *different* program wearing
// the base's filesystem is the failure the whole design refuses, so the
// entrypoint, the environment and the base's own binary are all asserted to
// have survived rather than been restated: a composition that rebuilt the
// image around the plugin would satisfy every assertion about where the plugin
// landed.
//
// Two platforms are built because composing into the host's variant alone
// would be invisible in a single-platform test and would ship an arm64 release
// whose plugin is missing — or, worse, holds an amd64 executable, which is the
// failure devex#397 names.
//
// The plugin is then run two ways on the host's variant: by its absolute path,
// which proves it landed executable, and by its bare name, which proves it is
// on the PATH where a base image's plugin discovery will look for it. The
// second is the whole contract and the first alone would not establish it.
func (t *Tests) AppComposesAnotherAppsPayload(ctx context.Context) error {
	const (
		baseVersion   = "v3.0.0"
		pluginVersion = "v0.4.1"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	platforms := []dagger.Platform{"linux/amd64", "linux/arm64"}
	base := dag.Z5Labs().Go(src).App(baseVersion, dagger.Z5LabsGoChainAppOpts{Platforms: platforms})
	derived := base.WithApp(composedPlugin(wantPluginName, pluginVersion, platforms))

	for _, platform := range platforms {
		ctr := derived.Container(platform)

		modes, err := statInImage(ctx, ctr, []string{wantComposedEntry, "/app/hello"})
		if err != nil {
			return fmt.Errorf("%s: %v", platform, err)
		}
		if got, want := modes[wantComposedEntry], wantEntryMode+" "+wantOwner; got != want {
			return fmt.Errorf("%s: %s is %q, want %q", platform, wantComposedEntry, got, want)
		}
		// The base's own binary is still there, still executable and still
		// owned the same way. A composition that replaced the image rather
		// than adding to it would drop it and every assertion about the
		// plugin would still pass.
		if got, want := modes["/app/hello"], wantEntryMode+" "+wantOwner; got != want {
			return fmt.Errorf("%s: the base's own binary is %q, want %q", platform, got, want)
		}

		entrypoint, err := ctr.Entrypoint(ctx)
		if err != nil {
			return fmt.Errorf("%s: read entrypoint: %v", platform, err)
		}
		if len(entrypoint) != 1 || entrypoint[0] != "/app/hello" {
			return fmt.Errorf("%s: the derived image's entrypoint is %v, want the base's [/app/hello]", platform, entrypoint)
		}
		if err := assertStandardEnvironment(ctx, ctr, string(platform)); err != nil {
			return err
		}
	}

	host := derived.Container(hostPlatform())
	out, err := host.WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run the derived image's entrypoint: %w", err)
	}
	if out != "hello\n" {
		return fmt.Errorf("the derived image's entrypoint printed %q, want the base's %q", out, "hello\n")
	}
	out, err = host.WithExec([]string{wantComposedEntry}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run %s: %w", wantComposedEntry, err)
	}
	if out != "plugin ok\n" {
		return fmt.Errorf("%s printed %q, want %q", wantComposedEntry, out, "plugin ok\n")
	}
	// By bare name, which resolves through the image's PATH. This is what a
	// base image's plugin discovery does, and it is the reason the composed
	// entry goes to the plugin directory rather than anywhere else.
	out, err = host.WithExec([]string{wantPluginName}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run %s through the image's PATH: %w", wantPluginName, err)
	}
	if out != "plugin ok\n" {
		return fmt.Errorf("%s on the PATH printed %q, want %q", wantPluginName, out, "plugin ok\n")
	}

	// Composing into an app must not reach back into the app it was derived
	// from: the base is still publishable on its own, holding its binary and
	// nothing else.
	entries, err := base.Container(hostPlatform()).Rootfs().Glob(ctx, "**")
	if err != nil {
		return fmt.Errorf("list the base rootfs: %v", err)
	}
	if got := onlyFiles(entries); len(got) != 1 || !strings.HasPrefix(got[0], "app/") {
		return fmt.Errorf("the base app holds %v after a composition, want the built binary alone", got)
	}
	return nil
}

// AppComposesACompleteApplication asserts that the application being composed
// may be a complete one — an executable plus the tree it needs — and that its
// own contributions come along at their own paths.
//
// This is the half of the design that keeps a composable application
// buildable, testable and publishable the way every other application is,
// rather than a slim carrier that only makes sense once merged. The payload
// fixture is useless without the tree beside it, so running it in the derived
// image is what proves nothing was silently discarded: the entry alone would
// land, run, and fail.
func (t *Tests) AppComposesACompleteApplication(ctx context.Context) error {
	const (
		baseVersion   = "v3.1.0"
		payloadName   = "payload"
		treePath      = "/srv/payload"
		greetingBytes = "hi\n"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	platform := hostPlatform()
	tree := dag.Directory().
		WithNewFile("greeting.txt", greetingBytes).
		WithNewFile("data/extra.txt", "extra\n")
	complete := prebuiltApp(payloadDir(), payloadName, "v1.2.3", []dagger.Platform{platform}).
		Build().
		WithDirectory(treePath, tree, dag.Z5Labs().DirectoryDocument(tree, treePath))
	derived := dag.Z5Labs().Go(src).
		App(baseVersion, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{platform}}).
		WithApp(complete)
	ctr := derived.Container(platform)

	// The tree kept its own path. There is no obvious other answer — a
	// contributed /etc/thing.conf belongs at /etc/thing.conf — and the program
	// reads it from a constant, so a composition that relocated it would fail
	// here rather than anywhere the relocation could be observed.
	out, err := ctr.WithExec([]string{"/usr/local/bin/" + payloadName}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run the composed payload: %w", err)
	}
	if want := "hi data/extra.txt,greeting.txt\n"; out != want {
		return fmt.Errorf("the composed payload printed %q, want %q", out, want)
	}

	// And the tree carries the module's mode and ownership here too. It is
	// re-applied on the way in rather than inherited from the image it came
	// out of, so a composition that copied the source image's metadata across
	// would show up as the wrong owner rather than as a missing file.
	modes, err := statInImage(ctx, ctr, []string{treePath, treePath + "/greeting.txt"})
	if err != nil {
		return err
	}
	for _, p := range []string{treePath, treePath + "/greeting.txt"} {
		if got, want := modes[p], wantDirectoryMode+" "+wantOwner; got != want {
			return fmt.Errorf("%s is %q, want %q", p, got, want)
		}
	}
	return nil
}

// AppComposesADerivedImage asserts that an application that has already been
// composed into may itself be composed, and that the plugins it picked up on
// the way stay runnable.
//
// A derived image is an ordinary App, so composing one is a documented use and
// the payload of the result is the union of both sides'. The failure this test
// exists for is that the union is not uniform: the composed application's own
// entry is declared, while a plugin it picked up earlier travels as an ordinary
// contribution — so a composition that decided what lands executable from the
// declaration alone would carry the earlier plugin across read-only and strip
// the one bit that makes it runnable. That failure is caught at publish, as a
// payload that will not start, which blames the payload rather than the
// composition; so it is caught here, where the mode can be read.
//
// Every executable in the final image is run rather than merely stat'd, and by
// bare name where it is on the PATH. A mode assertion alone would not catch a
// plugin that landed executable and unreachable.
func (t *Tests) AppComposesADerivedImage(ctx context.Context) error {
	inner, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	outer, err := gitFixture(ctx, stampedDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	platforms := []dagger.Platform{hostPlatform()}
	derived := dag.Z5Labs().Go(inner).
		App("v7.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: platforms}).
		WithApp(composedPlugin(wantPluginName, "v0.3.0", platforms))
	composed := dag.Z5Labs().Go(outer).
		App("v8.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: platforms}).
		WithApp(derived)
	ctr := composed.Container(hostPlatform())

	// The outer application's own binary, the application composed into it,
	// and the plugin that application had already picked up. All three are
	// executables and all three have to land as one.
	paths := []string{"/app/stamped", "/usr/local/bin/hello", wantComposedEntry}
	modes, err := statInImage(ctx, ctr, paths)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if got, want := modes[p], wantEntryMode+" "+wantOwner; got != want {
			return fmt.Errorf("%s is %q, want %q", p, got, want)
		}
	}
	// The outer image's entrypoint is still its own, not either of the
	// applications composed into it.
	entrypoint, err := ctr.Entrypoint(ctx)
	if err != nil {
		return fmt.Errorf("read entrypoint: %v", err)
	}
	if len(entrypoint) != 1 || entrypoint[0] != "/app/stamped" {
		return fmt.Errorf("the twice-derived image's entrypoint is %v, want [/app/stamped]", entrypoint)
	}
	for name, want := range map[string]string{"hello": "hello\n", wantPluginName: "plugin ok\n"} {
		out, err := ctr.WithExec([]string{name}).Stdout(ctx)
		if err != nil {
			return fmt.Errorf("run %s through the image's PATH: %w", name, err)
		}
		if out != want {
			return fmt.Errorf("%s printed %q, want %q", name, out, want)
		}
	}
	return nil
}

// AppRefusesToComposeAcrossPlatforms asserts that a platform set which does
// not match exactly is refused, in both directions.
//
// A platform the payload was not built for would publish an index whose
// manifest for that architecture holds an executable for another one: the
// failure arrives at exec time, with the kernel's message, in front of a
// consumer, and nothing in the manifest list says anything is wrong. A
// platform only the payload carries is a variant the caller built and would
// have dropped without a word. Neither can be resolved by picking a side.
func (t *Tests) AppRefusesToComposeAcrossPlatforms(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	both := []dagger.Platform{"linux/amd64", "linux/arm64"}
	one := []dagger.Platform{"linux/amd64"}

	cases := []struct {
		base, plugin []dagger.Platform
		want         string
		why          string
	}{
		{base: both, plugin: one, want: "nothing would be composed into linux/arm64", why: "a platform the payload was not built for"},
		{base: one, plugin: both, want: "the payload built for linux/arm64 would be discarded", why: "a platform only the payload carries"},
	}
	for _, c := range cases {
		app := dag.Z5Labs().Go(src).App("v4.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: c.base})
		_, err := app.WithApp(composedPlugin(wantPluginName, "v0.1.0", c.plugin)).ID(ctx)
		if err == nil {
			return fmt.Errorf("expected a base carrying %v and a payload carrying %v to be refused (%s), got nil", c.base, c.plugin, c.why)
		}
		if !strings.Contains(err.Error(), c.want) {
			return fmt.Errorf("expected the refusal of %s to mention %q, got: %s", c.why, c.want, err.Error())
		}
	}
	return nil
}

// AppRefusesToComposeOntoOccupiedPaths asserts that a collision is refused and
// that the refusal names both sides.
//
// Both sides, because a caller told only that a path is taken has to go and
// find out by what — and the two cases here are told apart by nothing else.
// Composing the same plugin twice collides with an application composed
// earlier; contributing a file where a plugin already is collides the other
// way round, through the seam that was there first. Either way the image would
// hold one thing while its documents describe two, which is the undetectable
// incompleteness this whole mechanism exists to prevent.
func (t *Tests) AppRefusesToComposeOntoOccupiedPaths(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	platforms := []dagger.Platform{hostPlatform()}
	app := dag.Z5Labs().Go(src).App("v5.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: platforms}).
		WithApp(composedPlugin(wantPluginName, "v0.1.0", platforms))

	_, err = app.WithApp(composedPlugin(wantPluginName, "v0.2.0", platforms)).ID(ctx)
	if err == nil {
		return fmt.Errorf("expected composing a second application whose entry is called %s to be refused, got nil", wantPluginName)
	}
	for _, want := range []string{wantComposedEntry, "the entry of the application composed at"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the collision refusal to mention %q, got: %s", want, err.Error())
		}
	}

	note := dag.Directory().WithNewFile("note", "note\n").File("note")
	_, err = app.WithFile(wantComposedEntry, note, dag.Z5Labs().FileDocument(note)).ID(ctx)
	if err == nil {
		return fmt.Errorf("expected contributing a file at %s to be refused, got nil", wantComposedEntry)
	}
	if !strings.Contains(err.Error(), "the entry of the application composed at") {
		return fmt.Errorf("expected the refusal to name what holds %s, got: %s", wantComposedEntry, err.Error())
	}
	return nil
}

// AppRefusesToPublishAPayloadThatCannotRun asserts that the entry of every
// composed payload is executed in the derived image before the first byte is
// pushed, and that one which cannot start fails the build.
//
// This is the criterion no API shape can establish. Enumerating a payload is
// always a guess — a script's imports, a template opened on the first request,
// a dlopen, a loader that lives in the image the executable came out of — so
// completeness is proven by running it. The fixture here is the crudest
// version of the same failure: bytes that are not an executable at all, which
// come back from the runtime as an exec format error, exactly as an executable
// built for another architecture does.
//
// The refusal has to happen before the push, so the registry is probed
// afterwards: an image published and then reported as broken is one a consumer
// can already have pulled.
func (t *Tests) AppRefusesToPublishAPayloadThatCannotRun(ctx context.Context) error {
	const (
		version    = "v6.0.0"
		repository = "z5labs/broken"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	headSha, err := headFullSha(ctx, src)
	if err != nil {
		return err
	}
	svc, pwdHex, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, headSha)
	if err != nil {
		return err
	}
	platform := hostPlatform()
	// An entry that is not an executable, packaged exactly the way a real one
	// is: the module never inspects the bytes, which is the point — nothing
	// short of running them can tell a payload that works from one that does
	// not.
	notAnExecutable := dag.Directory().
		WithNewFile(wantPluginName, "#!/nonexistent/interpreter\n", dagger.DirectoryWithNewFileOpts{Permissions: 0o555}).
		File(wantPluginName)
	broken := dag.Z5Labs().App("v0.0.1").
		WithVariant(platform, notAnExecutable, dag.Z5Labs().FileDocument(notAnExecutable)).
		Build()
	derived := dag.Z5Labs().Go(src).
		App(version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{platform}}).
		WithApp(broken)

	_, err = publishable(derived, svc, secret, prov).Publish(ctx, []string{repository})
	if err == nil {
		return fmt.Errorf("expected a publish of an image whose composed payload cannot start to be refused, got nil")
	}
	for _, want := range []string{wantComposedEntry, "could not be started"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the refusal to mention %q, got: %s", want, err.Error())
		}
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

// AppComposedImageStaysAttested asserts that a derived image publishes to a
// repository of its own, and that its documents and its provenance describe
// what is actually in it — both sides' payloads, per platform.
//
// The repository is deliberately neither application's binary name. A derived
// image is published as `avroc-gen-go` while the executables inside it are
// called `avroc` and `avroc-gen-go`, so a publish that derived the repository
// from anything in the image would be wrong for exactly the shape this seam
// exists to serve.
//
// The document assertion is the union, and it is what makes composition
// honest: an SBOM that accounted for the base and not the plugin would be
// well-formed, would attach cleanly and would be indistinguishable from a
// complete one. The provenance assertion covers the one fact nothing else
// records — the derived image carries the base's version, so without the
// composed entry in the predicate nothing anywhere says which release of the
// plugin shipped.
func (t *Tests) AppComposedImageStaysAttested(ctx context.Context) error {
	const (
		baseVersion   = "v12.0.0"
		pluginVersion = "v0.9.7"
		repository    = "z5labs/hello-gen"
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
	platforms := []dagger.Platform{hostPlatform()}
	derived := dag.Z5Labs().Go(src).
		App(baseVersion, dagger.Z5LabsGoChainAppOpts{Platforms: platforms}).
		WithApp(composedPlugin(wantPluginName, pluginVersion, platforms))

	refs, err := publishable(derived, svc, secret, prov).Publish(ctx, []string{repository})
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
	// Both payloads, and nothing else. The names are the ones each side
	// contributed under: the base's binary and the plugin's, which is what a
	// consumer reading the document sees in the image.
	want := []string{wantPluginName, "hello"}
	for what, doc := range map[string]publishedSbom{"SPDX": spdx, "CycloneDX": cdx} {
		got := append([]string{}, doc.Contained...)
		if len(got) != len(want) {
			return fmt.Errorf("the %s document says the derived image contains %v, want %v", what, got, want)
		}
		for _, name := range want {
			if doc.Components[name] == "" {
				return fmt.Errorf("the %s document lists no checksum for %s; it has %v", what, name, doc.Components)
			}
		}
	}
	for name, sum := range spdx.Components {
		if cdx.Components[name] != sum {
			return fmt.Errorf("the two documents disagree about %s: SPDX has %q, CycloneDX has %q", name, sum, cdx.Components[name])
		}
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
	if err := checkComposedInProvenance(statement, wantComposedEntry, pluginVersion, baseVersion); err != nil {
		return err
	}
	return nil
}

// checkComposedInProvenance asserts the predicate records every composed
// payload, and that the image is still published under the base's version.
//
// The version pair is asserted together on purpose. The derived image carries
// the base's version because the release it belongs to is the base's release,
// and that decision is exactly what makes the composed entry's version load
// bearing: drop it and the only version anywhere is the base's, so nothing
// says which release of the plugin a consumer is running.
func checkComposedInProvenance(statement map[string]any, entry, pluginVersion, baseVersion string) error {
	predicate, err := object(statement["predicate"], "predicate")
	if err != nil {
		return err
	}
	definition, err := object(predicate["buildDefinition"], "predicate.buildDefinition")
	if err != nil {
		return err
	}
	internal, err := object(definition["internalParameters"], "predicate.buildDefinition.internalParameters")
	if err != nil {
		return err
	}
	if got, _ := internal["version"].(string); got != baseVersion {
		return fmt.Errorf("the predicate says the image was published as %q, want the base's version %q", got, baseVersion)
	}
	raw, ok := internal["composed"].([]any)
	if !ok {
		return fmt.Errorf("the predicate records no composed payloads; internalParameters is %v", internal)
	}
	if len(raw) != 1 {
		return fmt.Errorf("the predicate records %d composed payloads, want 1", len(raw))
	}
	composed, err := object(raw[0], "predicate.buildDefinition.internalParameters.composed[0]")
	if err != nil {
		return err
	}
	if got, _ := composed["entry"].(string); got != entry {
		return fmt.Errorf("the predicate says the composed payload landed at %q, want %q", got, entry)
	}
	if got, _ := composed["version"].(string); got != pluginVersion {
		return fmt.Errorf("the predicate says the composed payload is %q, want %q", got, pluginVersion)
	}
	return nil
}

// pluginDir returns the composition fixture: a main package that prints
// something no other fixture prints.
func pluginDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/plugin")
}
