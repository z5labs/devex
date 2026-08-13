// Package main implements the test module for daggerverse/z5labs.
// Each test is exposed as a standalone Dagger function so it can be
// invoked individually during TDD; All wires them up for parallel
// execution under `dagger call all`.
package main

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
)

// registryAlias is the WithServiceBinding alias used wherever a test
// containerized client needs to reach the local registry:2 service.
const registryAlias = "registry"

// fixtureOriginURL is the origin remote every git fixture carries. It is
// never fetched from — `git remote add` creates no remote-tracking refs —
// and exists so the fixture has the one piece of git state a real clone
// has and `git init` does not: a URL to publish as the image's
// org.opencontainers.image.source.
const fixtureOriginURL = "https://example.com/z5labs/fixture.git"

// curlImage matches the pin used by the other test modules in this
// repo (envoy, otel, grafana-stack). ":latest" is a moving target.
const curlImage = "curlimages/curl:8.10.1"

// skopeoImage is what these tests read individual platform variants back
// out of the registry with. The module under test does not use skopeo —
// it publishes through the oci module — so this pin is the test harness's
// alone and is free to move independently. ":latest" is a moving target.
const skopeoImage = "quay.io/skopeo/stable:v1.22.2"

// wantImagePath is the PATH the module promises every image it builds
// carries, and wantPluginDir is the directory on it that an extension's
// executables land in.
//
// Both are written out here rather than read from the module: they are a
// contract with everyone who writes a `FROM` or a `COPY --from=` line
// against a published image, and a test that imported the module's own
// constants would agree with whatever the module changed them to. That is
// the whole point of pinning them — changing either breaks published
// Dockerfiles, so changing either has to break this test first.
const (
	wantImagePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	wantPluginDir = "/usr/local/bin"
)

// hostPlatform is the platform a single-platform test builds for. Test
// module code runs in a linux container on the engine, so runtime.GOARCH
// here is the engine's architecture — the only one a test can execute.
func hostPlatform() dagger.Platform { return dagger.Platform("linux/" + runtime.GOARCH) }

// pullVariant pulls the given platform's variant of image:tag out of the
// local registry and imports it as a container, so a test can look at the
// exact bytes that were published for that platform.
//
// skopeo rather than Container.From: an image reference resolves in the
// engine's BuildKit context, which does not see this session's service
// bindings, while skopeo running inside a service-bound container does.
// The password is a plain env var rather than a secret on purpose —
// Dagger excludes secret values from cache keys, and this exec must not
// be served from an earlier session's registry.
//
// Import resolves the archive against the container's platform, which
// defaults to the engine's; a foreign-architecture variant is invisible
// unless the container is created for that platform explicitly.
// nonce varies this pull's cache key. Pass a distinct value when a test
// pulls the same tag more than once and needs the later pulls to be real
// pulls rather than the first one's cached result.
func pullVariant(svc *dagger.Service, host, user, pwd, image, tag string, platform dagger.Platform, nonce string) *dagger.Container {
	arch := string(platform)
	if _, a, ok := strings.Cut(string(platform), "/"); ok {
		arch = a
	}
	ref := fmt.Sprintf("docker://%s:5000/%s:%s", host, image, tag)
	tarball := dag.Container().From(skopeoImage).
		WithServiceBinding(host, svc).
		WithEnvVariable("NONCE", nonce).
		WithEnvVariable("REGISTRY_USERNAME", user).
		WithEnvVariable("REGISTRY_PASSWORD", pwd).
		WithExec([]string{"sh", "-c",
			`skopeo copy --src-tls-verify=false --override-os linux --override-arch "$1" --src-creds="$REGISTRY_USERNAME:$REGISTRY_PASSWORD" "$2" docker-archive:/img.tar`,
			"sh", arch, ref,
		}).
		File("/img.tar")
	return dag.Container(dagger.ContainerOpts{Platform: platform}).Import(tarball)
}

type Tests struct{}

// All runs every z5labs test. parallel caps concurrency; defaults to 0
// (unbounded fan-out — GH Actions schedules each `dagger check` job onto
// its own runner, so in-runner parallelism is bounded by the VM).
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("GoCiPassesForValidSource", t.GoCiPassesForValidSource)
	jobs = jobs.WithJob("GoCiFailsForFailingTest", t.GoCiFailsForFailingTest)
	jobs = jobs.WithJob("GoCiRoutesLintVersion", t.GoCiRoutesLintVersion)
	jobs = jobs.WithJob("GoCiLintConfigOverridesBundledPolicy", t.GoCiLintConfigOverridesBundledPolicy)
	jobs = jobs.WithJob("GoCiRunsWithRaceByDefault", t.GoCiRunsWithRaceByDefault)
	jobs = jobs.WithJob("GoCiChainsEveryWithMethod", t.GoCiChainsEveryWithMethod)
	jobs = jobs.WithJob("AppValidatesTheVersion", t.AppValidatesTheVersion)
	jobs = jobs.WithJob("AppRejectsSourceWithoutGitMetadata", t.AppRejectsSourceWithoutGitMetadata)
	jobs = jobs.WithJob("AppContainerRunsTheEntrypoint", t.AppContainerRunsTheEntrypoint)
	jobs = jobs.WithJob("AppContainersCoverEveryPlatformInOrder", t.AppContainersCoverEveryPlatformInOrder)
	jobs = jobs.WithJob("AppImagesCarryTheStandardEnvironment", t.AppImagesCarryTheStandardEnvironment)
	jobs = jobs.WithJob("AppStampsEveryPlatformVariant", t.AppStampsEveryPlatformVariant)
	jobs = jobs.WithJob("AppRebuildIsByteIdenticalPerPlatform", t.AppRebuildIsByteIdenticalPerPlatform)
	jobs = jobs.WithJob("AppPublishReturnsDigestPinnedReferences", t.AppPublishReturnsDigestPinnedReferences)
	jobs = jobs.WithJob("AppPublishesEveryRepositoryNamed", t.AppPublishesEveryRepositoryNamed)
	jobs = jobs.WithJob("AppPublishesTheContainersItReturned", t.AppPublishesTheContainersItReturned)
	jobs = jobs.WithJob("AppPublishRefusesAnUnusableTarget", t.AppPublishRefusesAnUnusableTarget)
	jobs = jobs.WithJob("AppRefusesPlaintextRegistryUnlessInsecure", t.AppRefusesPlaintextRegistryUnlessInsecure)
	jobs = jobs.WithJob("AppAnnotatesEveryPlatformVariant", t.AppAnnotatesEveryPlatformVariant)
	jobs = jobs.WithJob("AppAttachesSbomsAndProvenance", t.AppAttachesSbomsAndProvenance)
	jobs = jobs.WithJob("AppAttestsTwoSegmentRepositories", t.AppAttestsTwoSegmentRepositories)
	jobs = jobs.WithJob("AppRefusesToPublishWithoutProvenanceMachinery", t.AppRefusesToPublishWithoutProvenanceMachinery)
	jobs = jobs.WithJob("AppRedactsCredentialsFromTheSourceAnnotation", t.AppRedactsCredentialsFromTheSourceAnnotation)

	return jobs.Run(ctx)
}

// localRegistry stands up a docker registry:2 service with htpasswd auth.
// Returns the service, the plaintext password (for curl probes), and the
// password as a *dagger.Secret (for App.WithRegistry). User is always "ci".
func localRegistry(ctx context.Context) (*dagger.Service, string, *dagger.Secret, error) {
	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("random sha256 (password): %v", err)
	}
	// Secret names appear in trace UIs and logs, so derive the name
	// from an independent random — never from the password value.
	nameHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("random sha256 (secret name): %v", err)
	}
	secret := dag.SetSecret("z5labs-registry-pwd-"+nameHex[:16], pwdHex)
	// Password is fed to htpasswd via a secret env var so it does not
	// appear in container args (which surface in traces/logs). NONCE
	// is a separate per-call random — Dagger intentionally excludes
	// secret values from cache keys, so without a non-secret nonce
	// the WithExec would return a cached htpasswd file from an
	// earlier session's password.
	htpasswdFile := dag.Container().From("httpd:2.4-alpine").
		WithEnvVariable("NONCE", nameHex).
		WithSecretVariable("REGISTRY_PASSWORD", secret).
		WithExec([]string{"sh", "-c", `htpasswd -Bbn ci "$REGISTRY_PASSWORD" > /tmp/htpasswd`}).
		File("/tmp/htpasswd")
	svc := dag.Container().From("registry:2").
		WithMountedFile("/auth/htpasswd", htpasswdFile).
		// Manifest deletion is left off, which is distribution's default
		// and GHCR's behaviour. registry:2 implements no referrers API
		// either, so a client attaching a second referrer to one subject
		// falls back to the referrers *tag* scheme, replaces the index
		// under that tag, and would delete the index it replaced. This
		// suite used to turn deletion on so that delete would succeed —
		// which made it green over a registry no publish target
		// resembles, and hid devex#360 until GHCR failed a real release.
		// The oci module skips the collection instead.
		WithEnvVariable("REGISTRY_AUTH", "htpasswd").
		WithEnvVariable("REGISTRY_AUTH_HTPASSWD_REALM", "Registry").
		WithEnvVariable("REGISTRY_AUTH_HTPASSWD_PATH", "/auth/htpasswd").
		WithExposedPort(5000).
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true})
	return svc, pwdHex, secret, nil
}

// publishable configures app with everything a publish requires: the local
// registry, the credential, plain HTTP, and the provenance machinery.
//
// Every publishing test needs all four, and none of them is optional —
// a publish that cannot produce provenance is refused, which is what
// AppRefusesToPublishWithoutProvenanceMachinery asserts from the other
// side.
func publishable(app *dagger.Z5LabsApp, svc *dagger.Service, secret *dagger.Secret, prov *provenanceHarness) *dagger.Z5LabsApp {
	return prov.apply(app.
		WithRegistry(registryAlias+":5000", "ci", secret).
		WithRegistryService(svc).
		WithInsecure())
}

// manifestAccept is the Accept header set every manifest read sends.
//
// A registry serves whichever manifest kind the client will take and
// answers 404 for one it will not, so an incomplete set here reports a
// published image as absent. The OCI *image manifest* is in the list
// because a single-platform publish stores one: an index naming one
// variant and a bare manifest are both legal, and which you get is the
// registry's business rather than something a test should depend on.
// Leaving it out made a publish that had demonstrably landed — the tag was
// in /v2/<name>/tags/list — read back as a 404.
const manifestAccept = `-H 'Accept: application/vnd.oci.image.index.v1+json' ` +
	`-H 'Accept: application/vnd.oci.image.manifest.v1+json' ` +
	`-H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' ` +
	`-H 'Accept: application/vnd.docker.distribution.manifest.v2+json'`

// curlProbeManifest issues a basic-auth GET against the registry's
// manifest endpoint and returns the HTTP status code. host is the
// registry hostname reachable from this session (use Service.Hostname).
func curlProbeManifest(ctx context.Context, svc *dagger.Service, host, user, pwd, image, tag string) (int, error) {
	out, err := dag.Container().From(curlImage).
		WithServiceBinding(host, svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`curl -s -o /dev/null -w "%%{http_code}" %s -u %s:%s http://%s:5000/v2/%s/manifests/%s`,
			manifestAccept, user, pwd, host, image, tag,
		)}).
		Stdout(ctx)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// curlManifestDigest returns the digest the registry itself reports for
// image:tag, read from the Docker-Content-Digest response header. It is the
// registry's view of what it stored, which is what makes it an independent
// check on a digest the module under test reported.
//
// The Accept headers matter beyond reachability: the digest of a manifest
// list is not the digest of any image inside it, so naming the index types
// first is what makes this the digest of what was actually pushed. See
// manifestAccept for why the image-manifest type is in the set too.
func curlManifestDigest(ctx context.Context, svc *dagger.Service, host, user, pwd, image, tag string) (string, error) {
	out, err := dag.Container().From(curlImage).
		WithServiceBinding(host, svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`curl -fsS -o /dev/null -D - %s -u %s:%s http://%s:5000/v2/%s/manifests/%s`,
			manifestAccept, user, pwd, host, image, tag,
		)}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "docker-content-digest") {
			continue
		}
		return strings.TrimSpace(value), nil
	}
	return "", fmt.Errorf("no Docker-Content-Digest header in response headers: %q", out)
}

// GoCiPassesForValidSource asserts that Go.Ci against a clean, vet-clean,
// gofmt-clean library fixture returns no error.
//
// This is also what proves the bundled configs/golangci.yml and the
// pinned golangci-lint speak the same dialect. The two majors reject each
// other's config files outright, so a v1 config reaching a v2 binary — or
// the reverse — fails this test before any linter runs.
func (t *Tests) GoCiPassesForValidSource(ctx context.Context) error {
	if err := dag.Z5Labs().Go(helloLibDir()).Ci(ctx); err != nil {
		return fmt.Errorf("Go.Ci on hello-lib: %w", err)
	}
	return nil
}

// GoCiRoutesLintVersion asserts WithLint's version reaches the lint stage
// rather than being accepted and dropped.
//
// It pins a version the `go` module refuses to read a major out of, so the
// assertion is a message naming that version — which can only have come
// from the lint stage. Proving routing this way costs no container work,
// and a pin that *is* valid is exercised where the behaviour lives, in the
// `go` module's own suite.
func (t *Tests) GoCiRoutesLintVersion(ctx context.Context) error {
	err := dag.Z5Labs().Go(helloLibDir()).
		WithLint(dagger.Z5LabsGoChainWithLintOpts{Version: "1.64.8"}).
		Ci(ctx)
	if err == nil {
		return fmt.Errorf(`expected Go.Ci with lint version "1.64.8" to fail, got nil`)
	}
	if msg := err.Error(); !strings.Contains(msg, `golangci-lint version "1.64.8"`) {
		return fmt.Errorf("expected the error to name the rejected lint version, got: %s", msg)
	}
	return nil
}

// GoCiRunsWithRaceByDefault asserts the test stage runs with the race
// detector unless a caller says otherwise, and that WithTest(false) is
// what says otherwise.
//
// The fixture's test passes under `go test` and fails under `go test
// -race`, so the two halves of this assertion cannot both hold unless the
// default really is on and the flag really reaches the stage. Asserting
// the default this way is what keeps it a property of the chain rather
// than of whoever constructed it: a GoChain built without the detector set
// would pass every other test in this suite.
func (t *Tests) GoCiRunsWithRaceByDefault(ctx context.Context) error {
	err := dag.Z5Labs().Go(raceLibDir()).Ci(ctx)
	if err == nil {
		return fmt.Errorf("expected Go.Ci on race-lib to fail with the race detector on by default, got nil")
	}
	if msg := err.Error(); !strings.Contains(msg, "exit code: 1") && !strings.Contains(msg, "FAIL") {
		return fmt.Errorf("expected a test-stage failure on race-lib, got: %s", msg)
	}

	if err := dag.Z5Labs().Go(raceLibDir()).WithTest(false).Ci(ctx); err != nil {
		return fmt.Errorf("Go.Ci on race-lib with WithTest(false): %w", err)
	}
	return nil
}

// GoCiChainsEveryWithMethod asserts each With* method returns a chain the
// next call can be made on, and that a fully configured chain still runs.
//
// WithBuild's tags reach App rather than Ci, so what is asserted here is
// that supplying them neither errors nor disturbs the checks; that they
// reach a build is App's business.
func (t *Tests) GoCiChainsEveryWithMethod(ctx context.Context) error {
	err := dag.Z5Labs().Go(helloLibDir()).
		WithLint(dagger.Z5LabsGoChainWithLintOpts{}).
		WithTest(true).
		WithBuild([]string{"integration"}).
		Ci(ctx)
	if err != nil {
		return fmt.Errorf("Go.Ci on a fully configured chain: %w", err)
	}
	return nil
}

// GoCiLintConfigOverridesBundledPolicy asserts WithLint's config replaces
// the bundled configs/golangci.yml rather than being accepted and dropped.
//
// The supplied file is a well-formed v2 config enabling a linter that does
// not exist, which golangci-lint refuses to start with. The same fixture
// passes under the bundled policy — that is GoCiPassesForValidSource — so a
// failure here can only come from the caller's file having reached the
// stage.
//
// The assertion is on golangci-lint's exit code rather than on its message
// because the message does not survive the module boundary: a failing
// WithExec inside a dependency arrives as `exit code: N` and nothing else,
// with the command's output visible only in the trace. Exit 3 is
// golangci-lint's general failure code as opposed to 1 for issues it
// found, so what it rules out is the stage merely reporting lint findings;
// it is not specific to configuration. The evidence that the caller's file
// is what reached the stage is the differential above — this fixture
// passes under the bundled policy — and the exit code only pins the
// failure to the lint stage rather than to fmt, vet or test.
func (t *Tests) GoCiLintConfigOverridesBundledPolicy(ctx context.Context) error {
	cfg := dag.Directory().
		WithNewFile(".golangci.yml", "version: \"2\"\n\nlinters:\n  default: none\n  enable:\n    - nosuchlinterexists\n").
		File(".golangci.yml")

	err := dag.Z5Labs().Go(helloLibDir()).
		WithLint(dagger.Z5LabsGoChainWithLintOpts{Config: cfg}).
		Ci(ctx)
	if err == nil {
		return fmt.Errorf("expected Go.Ci with a config naming an unknown linter to fail, got nil")
	}
	if msg := err.Error(); !strings.Contains(msg, "exit code: 3") {
		return fmt.Errorf("expected golangci-lint's config-error exit code 3, got: %s", msg)
	}
	return nil
}

// GoCiFailsForFailingTest asserts that Go.Ci surfaces a test failure as an
// error containing "FAIL" or "exit code: 1".
//
// The test stage is not opt-in — this calls Ci with no With* configuration
// at all — so a library whose tests fail cannot pass the check by simply
// not asking for tests.
func (t *Tests) GoCiFailsForFailingTest(ctx context.Context) error {
	err := dag.Z5Labs().Go(failingLibDir()).Ci(ctx)
	if err == nil {
		return fmt.Errorf("expected Go.Ci on failing-lib to error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exit code: 1") && !strings.Contains(msg, "FAIL") {
		return fmt.Errorf("expected error to contain \"exit code: 1\" or \"FAIL\", got: %s", msg)
	}
	return nil
}

// versionCase is one row of AppValidatesTheVersion's table.
type versionCase struct {
	version string
	// want is a substring the refusal must carry, or "" when the version
	// has to be accepted.
	want string
	// why records what the row is for, so a failure names the rule rather
	// than the string.
	why string
}

// AppValidatesTheVersion asserts the version a caller states is checked
// against the OCI tag charset, as a table rather than only by releasing.
//
// A table is the point. The version used to be derived from HEAD and
// sanitized — any character outside the charset became "-" — which is the
// right trade for a value the pipeline invented and the wrong one for a
// value the caller states, because two versions that differ only outside
// the charset would sanitize to one tag and the second publish would
// silently replace the first. There is no way to observe that from a
// release: both publishes succeed. So the rule is asserted directly, on
// every shape of input that distinguishes it, including the accepting
// cases — a validator that refused everything would pass a table of
// refusals alone.
//
// The build is never evaluated here. Validation happens before App reads
// anything, so a refused version costs one call and no compile, and an
// accepted one costs the git read the fixture has already paid for.
func (t *Tests) AppValidatesTheVersion(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	cases := []versionCase{
		{version: "v1.2.3", why: "the ordinary tagged release"},
		{version: "1.0.0", why: "a version with no v prefix"},
		{version: "1.0.0-rc.1", why: "a SemVer prerelease, which is entirely inside the charset"},
		{version: "latest", why: "a plain moving tag"},
		{version: "_internal", why: `"_" is the one non-alphanumeric an OCI tag may open with`},
		{version: "2026.08.12", why: "a date-shaped version"},
		{version: "abc1234-2026-01-01T00-00-00Z", why: "the shape the old HEAD-derived version had"},
		{version: strings.Repeat("a", 128), why: "exactly the 128-character limit"},

		{version: "", want: "version is required", why: "an omitted version"},
		{
			version: "1.0.0+build.7",
			want:    "build metadata",
			why:     "SemVer build metadata, which must be refused rather than mangled",
		},
		{
			version: "1.0.0+build.7",
			want:    "1.0.0",
			why:     "the refusal has to name what the two builds would collapse to",
		},
		{version: "-1.0.0", want: "starts with", why: `an OCI tag may not open with "-"`},
		{version: ".1.0.0", want: "starts with", why: `an OCI tag may not open with "."`},
		{version: "release/v1.2.3", want: "not in the OCI tag charset", why: "a slash, which the old sanitizer rewrote"},
		{version: "v1 0", want: "not in the OCI tag charset", why: "a space"},
		{version: "v1.0.0#1", want: "not in the OCI tag charset", why: "punctuation outside the charset"},
		{version: strings.Repeat("a", 129), want: "OCI tag limit", why: "one character past the limit"},
	}
	for _, c := range cases {
		_, err := dag.Z5Labs().Go(src).
			App(c.version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}}).
			ID(ctx)
		if c.want == "" {
			if err != nil {
				return fmt.Errorf("expected version %q to be accepted (%s), got: %v", c.version, c.why, err)
			}
			continue
		}
		if err == nil {
			return fmt.Errorf("expected version %q to be refused (%s), got nil", c.version, c.why)
		}
		if !strings.Contains(err.Error(), c.want) {
			return fmt.Errorf("expected the refusal of %q (%s) to carry %q, got: %s", c.version, c.why, c.want, err.Error())
		}
	}
	return nil
}

// AppRejectsSourceWithoutGitMetadata asserts App refuses a source tree
// that is not a git working tree, and says so.
//
// The commit stamp comes from HEAD and from nothing else, so a tree with
// no HEAD cannot be built into an app at all. Failing here rather than
// somewhere inside the compile is what keeps the message about the input
// the caller got wrong instead of about a bare git error.
func (t *Tests) AppRejectsSourceWithoutGitMetadata(ctx context.Context) error {
	_, err := dag.Z5Labs().Go(helloDir()).App("v1.0.0").ID(ctx)
	if err == nil {
		return fmt.Errorf("expected App on a source with no .git to error, got nil")
	}
	if !strings.Contains(err.Error(), "git working tree") {
		return fmt.Errorf("expected the error to mention \"git working tree\", got: %s", err.Error())
	}
	return nil
}

// AppContainerRunsTheEntrypoint asserts App.Container produces an image
// whose entrypoint runs the compiled binary.
func (t *Tests) AppContainerRunsTheEntrypoint(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	out, err := dag.Z5Labs().Go(src).
		App("v1.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}}).
		Container(hostPlatform()).
		WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run the app image's entrypoint: %w", err)
	}
	if out != "hello\n" {
		return fmt.Errorf("expected %q, got %q", "hello\n", out)
	}
	return nil
}

// AppContainersCoverEveryPlatformInOrder asserts Containers returns one
// image per platform in the order the platforms were given, and that
// Container names the same images individually.
//
// Order is asserted because it is the only thing that tells the variants
// apart from the outside: Containers hands back containers and nothing
// else, so a caller matching a platform to an image is matching by index.
// Naming a platform that was not built is an error rather than an empty
// result, because a silently absent variant is a publish that quietly
// ships fewer architectures than it was asked for.
func (t *Tests) AppContainersCoverEveryPlatformInOrder(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	platforms := []dagger.Platform{"linux/arm64", "linux/amd64"}
	app := dag.Z5Labs().Go(src).App("v1.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: platforms})
	containers, err := app.Containers(ctx)
	if err != nil {
		return fmt.Errorf("Containers: %v", err)
	}
	if len(containers) != len(platforms) {
		return fmt.Errorf("expected %d images, got %d", len(platforms), len(containers))
	}
	for i, want := range platforms {
		got, err := containers[i].Platform(ctx)
		if err != nil {
			return fmt.Errorf("read platform of image %d: %v", i, err)
		}
		if got != want {
			return fmt.Errorf("expected image %d to be %s, got %s", i, want, got)
		}
		single, err := app.Container(want).Platform(ctx)
		if err != nil {
			return fmt.Errorf("Container(%s): %v", want, err)
		}
		if single != want {
			return fmt.Errorf("Container(%s) returned a %s image", want, single)
		}
	}
	if _, err := app.Container("linux/riscv64").Platform(ctx); err == nil {
		return fmt.Errorf("expected Container on a platform this app was not built for to error, got nil")
	}
	return nil
}

// AppImagesCarryTheStandardEnvironment asserts every image carries exactly
// the standardized environment, and that the entrypoint does not depend on
// it.
//
// Exactly, not at least: the contract an extension writes against is that
// the environment is knowable, and a stray variable — a credential leaked
// in from a build step, a debug flag — ships silently inside something
// people pull. The plugin directory is asserted to be on the PATH by name
// because that is the promise a `COPY` line is written against, and the
// entrypoint is asserted absolute because the app must run whatever the
// PATH says: PATH is for what an extension adds, not for finding the app.
func (t *Tests) AppImagesCarryTheStandardEnvironment(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	platforms := []dagger.Platform{"linux/amd64", "linux/arm64"}
	app := dag.Z5Labs().Go(src).App("v1.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: platforms})
	for _, platform := range platforms {
		ctr := app.Container(platform)
		if err := assertStandardEnvironment(ctx, ctr, string(platform)); err != nil {
			return err
		}
		entrypoint, err := ctr.Entrypoint(ctx)
		if err != nil {
			return fmt.Errorf("%s: read entrypoint: %v", platform, err)
		}
		if len(entrypoint) != 1 || !strings.HasPrefix(entrypoint[0], "/") {
			return fmt.Errorf("%s: expected a single absolute entrypoint, got %v", platform, entrypoint)
		}
		// An entrypoint that happened to sit in the plugin directory would
		// make "the app does not need the PATH" true by accident.
		if strings.HasPrefix(entrypoint[0], wantPluginDir+"/") {
			return fmt.Errorf("%s: the app's own binary is in the plugin directory %s, which is an extension's to fill", platform, wantPluginDir)
		}
	}
	return nil
}

// assertStandardEnvironment checks ctr's environment is exactly the
// standardized set.
func assertStandardEnvironment(ctx context.Context, ctr *dagger.Container, what string) error {
	vars, err := ctr.EnvVariables(ctx)
	if err != nil {
		return fmt.Errorf("%s: read the image environment: %v", what, err)
	}
	got := map[string]string{}
	for i := range vars {
		name, err := vars[i].Name(ctx)
		if err != nil {
			return fmt.Errorf("%s: read an environment variable's name: %v", what, err)
		}
		value, err := vars[i].Value(ctx)
		if err != nil {
			return fmt.Errorf("%s: read %s: %v", what, name, err)
		}
		got[name] = value
	}
	if len(got) != 1 {
		return fmt.Errorf("%s: expected the image environment to be PATH alone, got %v", what, got)
	}
	if got["PATH"] != wantImagePath {
		return fmt.Errorf("%s: expected PATH=%q, got %q", what, wantImagePath, got["PATH"])
	}
	onPath := false
	for _, dir := range strings.Split(got["PATH"], ":") {
		if dir == wantPluginDir {
			onPath = true
		}
	}
	if !onPath {
		return fmt.Errorf("%s: the plugin directory %s is not on the image's PATH %q", what, wantPluginDir, got["PATH"])
	}
	return nil
}

// gitFixture overlays a fresh single-commit git repo on base. branch is
// the working-branch name; tags is a slice of annotated tags created on
// the single commit.
//
// The tags no longer decide anything — the version is the caller's — but a
// fixture that can carry them keeps the git state a real repository has.
func gitFixture(ctx context.Context, base *dagger.Directory, branch string, tags []string) (*dagger.Directory, error) {
	ctr := dag.Go().Container(base).
		WithEnvVariable("GIT_AUTHOR_NAME", "CI").
		WithEnvVariable("GIT_AUTHOR_EMAIL", "ci@example.com").
		WithEnvVariable("GIT_COMMITTER_NAME", "CI").
		WithEnvVariable("GIT_COMMITTER_EMAIL", "ci@example.com").
		WithExec([]string{"git", "init", "--initial-branch=" + branch, "."}).
		WithExec([]string{"git", "add", "."}).
		WithExec([]string{"git", "commit", "-m", "initial"}).
		WithExec([]string{"git", "remote", "add", "origin", fixtureOriginURL})
	for _, tag := range tags {
		ctr = ctr.WithExec([]string{"git", "tag", "-a", tag, "-m", tag})
	}
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	return ctr.Directory("/src"), nil
}

// helloDir returns the on-disk hello (app) fixture.
func helloDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/hello")
}

// stampedDir returns the stamped (app) fixture: a main package that
// declares the two package-level vars the build stamps and prints them.
func stampedDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/stamped")
}

// helloLibDir returns the on-disk hello-lib fixture (library variant).
func helloLibDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/hello-lib")
}

// raceLibDir returns the race-lib fixture: a library whose test passes
// under `go test` and fails under `go test -race`.
func raceLibDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/race-lib")
}

// failingLibDir returns the failing-lib fixture (test fails).
func failingLibDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/failing-lib")
}

// headShortSha returns `git rev-parse --short HEAD` for a git-backed
// source, so a test can compare the stamp against the commit it came from.
func headShortSha(ctx context.Context, src *dagger.Directory) (string, error) {
	out, err := dag.Go().Container(src).
		WithExec([]string{"git", "rev-parse", "--short", "HEAD"}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(out), nil
}

// stampOf runs the stamped fixture's entrypoint in ctr and returns the
// version and commit it reports.
func stampOf(ctx context.Context, ctr *dagger.Container) (version, commit string, err error) {
	out, err := ctr.
		WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).
		Stdout(ctx)
	if err != nil {
		return "", "", fmt.Errorf("run stamped binary: %v", err)
	}
	return parseStampLine(out)
}

// parseStampLine parses the stamped fixture's single line of output,
// "version=<v> commit=<c>". Neither value can contain a space: commit is a
// short SHA and version is an OCI-tag-safe string.
func parseStampLine(out string) (version, commit string, err error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected stamp output %q", out)
	}
	version, okV := strings.CutPrefix(fields[0], "version=")
	commit, okC := strings.CutPrefix(fields[1], "commit=")
	if !okV || !okC {
		return "", "", fmt.Errorf("unexpected stamp output %q", out)
	}
	return version, commit, nil
}

// alpineImage is the minimal container a built binary is inspected in.
// ":latest" is a moving target, so the tag is pinned.
const alpineImage = "alpine:3.22"

// binaryContains reports whether the raw bytes of bin contain needle.
// grep -a treats the binary as text so a match is reported rather than
// collapsed into "binary file matches".
func binaryContains(ctx context.Context, bin *dagger.File, needle string) (bool, error) {
	out, err := dag.Container().From(alpineImage).
		WithFile("/bin/app", bin).
		WithExec([]string{"sh", "-c", `grep -a -q -- "$1" /bin/app && echo yes || echo no`, "sh", needle}).
		Stdout(ctx)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// AppStampsEveryPlatformVariant asserts a multi-platform build stamps
// every variant with the caller's version and the commit read from HEAD.
//
// A publish collapses the per-platform images into a single manifest list,
// so a stamp applied at the image or publish layer would land on an
// artifact whose variants have already been merged; only a stamp applied in
// the per-variant compile reaches them all. Each variant is therefore
// checked individually: the one matching the engine's architecture is
// executed, and the foreign one — which cannot be executed here — is
// searched for the stamped bytes.
func (t *Tests) AppStampsEveryPlatformVariant(ctx context.Context) error {
	const version = "v9.9.9"
	src, err := gitFixture(ctx, stampedDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	sha, err := headShortSha(ctx, src)
	if err != nil {
		return err
	}
	platforms := []dagger.Platform{"linux/amd64", "linux/arm64"}
	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: platforms})
	for _, p := range platforms {
		ctr := app.Container(p)
		if p == hostPlatform() {
			gotVersion, gotCommit, err := stampOf(ctx, ctr)
			if err != nil {
				return fmt.Errorf("%s: %v", p, err)
			}
			if gotVersion != version || gotCommit != sha {
				return fmt.Errorf("%s: expected version=%q commit=%q, got version=%q commit=%q", p, version, sha, gotVersion, gotCommit)
			}
			continue
		}
		bin := ctr.File("/app/stamped")
		for _, want := range []string{version, sha} {
			found, err := binaryContains(ctx, bin, want)
			if err != nil {
				return fmt.Errorf("%s: scan binary: %v", p, err)
			}
			if !found {
				return fmt.Errorf("%s: expected the binary to carry %q", p, want)
			}
		}
		// Negative control: a scan that answers yes to everything would
		// make the two assertions above vacuous.
		found, err := binaryContains(ctx, bin, "v0.0.0-never-stamped")
		if err != nil {
			return fmt.Errorf("%s: scan binary: %v", p, err)
		}
		if found {
			return fmt.Errorf("%s: binary scan reports a string that was never stamped", p)
		}
	}
	return nil
}

// pinnedGitFixture overlays a git repo whose commit is a pure function of
// the fixture's contents: the author and committer dates are pinned, so
// two calls produce the same commit SHA and therefore the same stamp.
//
// nonce does two jobs, and the second one is what makes a reproducibility
// assertion mean anything. It varies the git exec's cache key, so the two
// calls are two git invocations rather than one cached result; and it names
// an untracked file left in the working tree afterwards, so the two trees
// are not byte-identical.
//
// Without that file the engine's content addressing would collapse the
// second compile into the first — two identical inputs, one cached output —
// and the test would be comparing an artifact against itself, which passes
// however the build was written. The file is untracked and is not a Go
// source file, so it changes neither the commit the stamps come from nor
// anything the compiler reads: the two builds must genuinely re-run and
// must genuinely agree.
func pinnedGitFixture(ctx context.Context, base *dagger.Directory, branch, nonce string) (*dagger.Directory, error) {
	const commitDate = "2024-01-02T03:04:05+00:00"
	ctr := dag.Go().Container(base).
		WithEnvVariable("NONCE", nonce).
		WithEnvVariable("GIT_AUTHOR_NAME", "CI").
		WithEnvVariable("GIT_AUTHOR_EMAIL", "ci@example.com").
		WithEnvVariable("GIT_AUTHOR_DATE", commitDate).
		WithEnvVariable("GIT_COMMITTER_NAME", "CI").
		WithEnvVariable("GIT_COMMITTER_EMAIL", "ci@example.com").
		WithEnvVariable("GIT_COMMITTER_DATE", commitDate).
		WithExec([]string{"git", "init", "--initial-branch=" + branch, "."}).
		WithExec([]string{"git", "add", "."}).
		WithExec([]string{"git", "commit", "-m", "initial"}).
		WithExec([]string{"git", "remote", "add", "origin", fixtureOriginURL}).
		WithNewFile("/src/untracked-"+nonce+".txt", nonce)
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	return ctr.Directory("/src"), nil
}

// AppRebuildIsByteIdenticalPerPlatform asserts building one (commit,
// version) pair twice produces byte-identical binaries for every platform.
//
// The two runs build from independently created working trees of the same
// commit, differing by one untracked file so that the engine cannot serve
// the second compile from the first — see pinnedGitFixture, where that is
// the whole reason the file exists. What makes the outputs agree is that
// the stamp and everything else in the link line are functions of the
// commit and the stated version alone: a wall clock, a build host path or a
// nondeterministic link order would show up here as two different digests.
func (t *Tests) AppRebuildIsByteIdenticalPerPlatform(ctx context.Context) error {
	const version = "v4.5.6"
	platforms := []dagger.Platform{"linux/amd64", "linux/arm64"}
	runs := make([]map[dagger.Platform]string, 0, 2)
	for _, nonce := range []string{"run-a", "run-b"} {
		src, err := pinnedGitFixture(ctx, stampedDir(), "main", nonce)
		if err != nil {
			return fmt.Errorf("pinnedGitFixture %s: %v", nonce, err)
		}
		app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: platforms})
		digests := make(map[dagger.Platform]string, len(platforms))
		for _, p := range platforms {
			d, err := app.Container(p).File("/app/stamped").
				Digest(ctx, dagger.FileDigestOpts{ExcludeMetadata: true})
			if err != nil {
				return fmt.Errorf("%s %s: digest: %v", nonce, p, err)
			}
			digests[p] = d
		}
		runs = append(runs, digests)
	}
	for _, p := range platforms {
		if runs[0][p] != runs[1][p] {
			return fmt.Errorf("%s: expected a byte-identical rebuild, got %q then %q", p, runs[0][p], runs[1][p])
		}
	}
	return nil
}

// AppPublishReturnsDigestPinnedReferences asserts Publish reports what it
// published, as references pinned to the digest the registry holds.
//
// A tag is a mutable name, so a caller anchoring an attestation, a
// deployment or a release note to a publish has to be handed something
// immutable. The digest is checked against the registry's own view of what
// it stored rather than against a value this pipeline computed, which is
// what makes it an independent check.
func (t *Tests) AppPublishReturnsDigestPinnedReferences(ctx context.Context) error {
	const (
		version    = "v2.0.0"
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
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}})
	refs, err := publishable(app, svc, secret, prov).Publish(ctx, []string{repository})
	if err != nil {
		return fmt.Errorf("Publish: %v", err)
	}
	if len(refs) != 1 {
		return fmt.Errorf("expected 1 reference for 1 repository and 1 tag, got %v", refs)
	}
	stored, err := curlManifestDigest(ctx, svc, registryAlias, "ci", pwdHex, repository, version)
	if err != nil {
		return fmt.Errorf("read stored digest: %v", err)
	}
	want := fmt.Sprintf("%s:5000/%s:%s@%s", registryAlias, repository, version, stored)
	if refs[0] != want {
		return fmt.Errorf("expected the reference %q, got %q", want, refs[0])
	}
	return nil
}

// AppPublishesEveryRepositoryNamed asserts one manifest list lands per
// repository, under the app's version, with a reference returned for each.
//
// More than one repository in a call is not a curiosity: a release that
// goes to a public registry and to an internal mirror is one build, and
// re-running the build per destination would publish bytes that are only
// probably the same.
func (t *Tests) AppPublishesEveryRepositoryNamed(ctx context.Context) error {
	const version = "v1.5.0"
	repositories := []string{"hello", "z5labs/hello-mirror"}
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	svc, pwdHex, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}})
	refs, err := publishable(app, svc, secret, prov).Publish(ctx, repositories)
	if err != nil {
		return fmt.Errorf("Publish: %v", err)
	}
	if len(refs) != len(repositories) {
		return fmt.Errorf("expected %d references, got %v", len(repositories), refs)
	}
	for i, repository := range repositories {
		code, err := curlProbeManifest(ctx, svc, registryAlias, "ci", pwdHex, repository, version)
		if err != nil {
			return fmt.Errorf("curl probe %s: %v", repository, err)
		}
		if code != 200 {
			return fmt.Errorf("expected %s:%s to return 200, got %d", repository, version, code)
		}
		if !strings.Contains(refs[i], "/"+repository+":"+version+"@sha256:") {
			return fmt.Errorf("expected reference %d to name %s:%s pinned to a digest, got %q", i, repository, version, refs[i])
		}
	}
	// The tag listing is the check that the version is the only tag: a
	// publish deriving a second tag from the branch or the commit would
	// still leave every assertion above true.
	tags, err := listTags(ctx, svc, registryAlias, "ci", pwdHex, "hello")
	if err != nil {
		return fmt.Errorf("listTags: %v", err)
	}
	if len(tags) != 1 || tags[0] != version {
		return fmt.Errorf("expected the version to be the only published tag, got %v", tags)
	}
	return nil
}

// AppPublishesTheContainersItReturned asserts the bytes a publish pushed
// are the bytes Container handed back, and that the published image still
// carries the standardized environment.
//
// This is the check seam. A caller inspecting an image and then publishing
// it is only doing something meaningful if the two are the same artifact;
// App and Container are session-cached precisely so that one chained call
// builds once. The binary is compared by digest rather than by rerunning
// it, and the published variant is pulled back out of the registry so the
// comparison is against what a consumer would get.
//
// What this cannot distinguish on its own is a second build that happened
// to be identical — which is exactly what AppRebuildIsByteIdenticalPerPlatform
// says would happen. The structural half of the guarantee is the caching
// directive; this is the half that can fail.
func (t *Tests) AppPublishesTheContainersItReturned(ctx context.Context) error {
	const (
		version    = "v1.1.0"
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
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}})
	built, err := app.Container(hostPlatform()).File("/app/hello").
		Digest(ctx, dagger.FileDigestOpts{ExcludeMetadata: true})
	if err != nil {
		return fmt.Errorf("digest the built binary: %v", err)
	}
	if _, err := publishable(app, svc, secret, prov).Publish(ctx, []string{repository}); err != nil {
		return fmt.Errorf("Publish: %v", err)
	}
	pulled := pullVariant(svc, registryAlias, "ci", pwdHex, repository, version, hostPlatform(), "")
	published, err := pulled.File("/app/hello").
		Digest(ctx, dagger.FileDigestOpts{ExcludeMetadata: true})
	if err != nil {
		return fmt.Errorf("digest the published binary: %v", err)
	}
	if built != published {
		return fmt.Errorf("Container returned %s, the registry holds %s", built, published)
	}
	return assertStandardEnvironment(ctx, pulled, "the published "+string(hostPlatform())+" variant")
}

// AppPublishRefusesAnUnusableTarget asserts the publish refuses a target it
// cannot honour, rather than publishing somewhere the caller did not mean.
//
// Every case here is a refusal that has to happen before any byte moves.
// A repository carrying a registry address is the one that would otherwise
// succeed: "ghcr.io/z5labs/app" appended to "ghcr.io" publishes to
// ghcr.io/ghcr.io/z5labs/app, which the registry accepts and which is
// discovered by somebody failing to pull it.
func (t *Tests) AppPublishRefusesAnUnusableTarget(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	svc, _, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	app := dag.Z5Labs().Go(src).App("v1.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}})
	configured := publishable(app, svc, secret, prov)

	cases := []struct {
		repositories []string
		want         string
		why          string
	}{
		{repositories: nil, want: "at least one repository", why: "nothing to publish to"},
		{repositories: []string{""}, want: "repository is required", why: "an empty repository"},
		{
			repositories: []string{"ghcr.io/z5labs/hello"},
			want:         "registry address",
			why:          "a repository that is really a reference, which would otherwise publish under a doubled host",
		},
		{repositories: []string{"hello:v1"}, want: "must not carry a tag", why: "a tag, which the version already states"},
		{repositories: []string{"hello@sha256:abc"}, want: "must not carry a digest", why: "a digest, which Publish returns rather than takes"},
		{repositories: []string{"/hello"}, want: "must not start or end", why: "a leading separator"},
	}
	for _, c := range cases {
		_, err := configured.Publish(ctx, c.repositories)
		if err == nil {
			return fmt.Errorf("expected Publish(%v) to be refused (%s), got nil", c.repositories, c.why)
		}
		if !strings.Contains(err.Error(), c.want) {
			return fmt.Errorf("expected the refusal of %v (%s) to carry %q, got: %s", c.repositories, c.why, c.want, err.Error())
		}
	}

	// And a publish with no registry configured at all, which is the same
	// class of mistake reached from the other side.
	_, err = prov.apply(app).Publish(ctx, []string{"hello"})
	if err == nil {
		return fmt.Errorf("expected Publish with no registry configured to be refused, got nil")
	}
	if !strings.Contains(err.Error(), "withRegistry") {
		return fmt.Errorf("expected the refusal to name withRegistry, got: %s", err.Error())
	}
	return nil
}

// AppRefusesPlaintextRegistryUnlessInsecure asserts TLS verification is not
// inferred from WithRegistryService being set.
//
// The publish path used to disable verification whenever a service was
// present, so a caller who supplied one for their own reasons — a private
// registry that happens to be a Dagger service — silently published over an
// unverified connection they never asked for. Verification is now the
// caller's explicit choice, and with WithInsecure left off a plain-HTTP
// registry is refused rather than accommodated.
func (t *Tests) AppRefusesPlaintextRegistryUnlessInsecure(ctx context.Context) error {
	const (
		version    = "v3.0.0"
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
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}})
	_, err = prov.apply(app.
		WithRegistry(registryAlias+":5000", "ci", secret).
		WithRegistryService(svc)).
		Publish(ctx, []string{repository})
	if err == nil {
		return fmt.Errorf("expected Publish to refuse a plain-HTTP registry with insecure off, got nil")
	}
	// The refusal has to mean nothing was pushed, not that the push
	// succeeded and the report was wrong.
	code, err := curlProbeManifest(ctx, svc, registryAlias, "ci", pwdHex, repository, version)
	if err != nil {
		return fmt.Errorf("curl probe: %v", err)
	}
	if code == 200 {
		return fmt.Errorf("Publish reported a failure but manifest %s is present in the registry", version)
	}
	return nil
}

// listTags queries the registry's /v2/<image>/tags/list endpoint and
// returns the parsed tag list. host is the registry hostname reachable
// from this session.
func listTags(ctx context.Context, svc *dagger.Service, host, user, pwd, image string) ([]string, error) {
	out, err := dag.Container().From(curlImage).
		WithServiceBinding(host, svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`curl -fs -u %s:%s http://%s:5000/v2/%s/tags/list`,
			user, pwd, host, image,
		)}).
		Stdout(ctx)
	if err != nil {
		return nil, err
	}
	tags, err := parseTagsList(out)
	if err != nil {
		return nil, err
	}
	return withoutReferrerTags(tags), nil
}

// referrerTag matches the referrers *tag scheme*: an index of everything
// attached to one subject, stored under "sha256-<hex>" of that subject's
// digest. A registry without the OCI 1.1 referrers API — which is
// registry:2 here and ghcr.io in production — stores attestations that
// way, so every published image now grows one of these beside its real
// tags.
var referrerTag = regexp.MustCompile(`^sha256-[0-9a-f]{64}$`)

// withoutReferrerTags drops referrer fallback tags from a tag listing.
//
// Callers of listTags are asking which tags this pipeline *published*,
// and a referrer index is not one: it is addressed by the digest it hangs
// off, it is created by the attach and not by the publish, and counting
// it would make "published exactly one tag" a statement about how many
// attestations happened to be attached.
func withoutReferrerTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if referrerTag.MatchString(tag) {
			continue
		}
		out = append(out, tag)
	}
	return out
}

// parseTagsList extracts the `tags` array from a registry tags/list
// JSON response. Minimal parser so we don't need a json import.
func parseTagsList(body string) ([]string, error) {
	i := strings.Index(body, "\"tags\"")
	if i < 0 {
		return nil, fmt.Errorf("tags field not found in %q", body)
	}
	body = body[i:]
	open := strings.IndexByte(body, '[')
	close := strings.IndexByte(body, ']')
	if open < 0 || close < 0 || close < open {
		return nil, fmt.Errorf("malformed tags array in %q", body)
	}
	inner := strings.TrimSpace(body[open+1 : close])
	if inner == "" || inner == "null" {
		return nil, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"")
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}
