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
// out of the registry with. The module under test no longer uses skopeo —
// it publishes through the oci module — so this pin is the test harness's
// alone and is free to move independently. ":latest" is a moving target.
const skopeoImage = "quay.io/skopeo/stable:v1.22.2"

// hostPlatform is the platform Builder builds for. Test module code runs
// in a linux container on the engine, so runtime.GOARCH here is the
// engine's architecture — the same one Builder resolves.
func hostPlatform() string { return "linux/" + runtime.GOARCH }

// hostArch is hostPlatform's GOARCH component, as skopeo's
// --override-arch spells it.
func hostArch() string { return runtime.GOARCH }

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
func pullVariant(svc *dagger.Service, host, user, pwd, image, tag, platform, nonce string) *dagger.Container {
	arch := platform
	if _, a, ok := strings.Cut(platform, "/"); ok {
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
	return dag.Container(dagger.ContainerOpts{Platform: dagger.Platform(platform)}).Import(tarball)
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
	jobs = jobs.WithJob("GoLibCiPassesForValidSource", t.GoLibCiPassesForValidSource)
	jobs = jobs.WithJob("GoLibCiFailsForFailingTest", t.GoLibCiFailsForFailingTest)
	jobs = jobs.WithJob("GoLibCiRoutesLintVersion", t.GoLibCiRoutesLintVersion)
	jobs = jobs.WithJob("BuilderBinaryProducesCompiledBinary", t.BuilderBinaryProducesCompiledBinary)
	jobs = jobs.WithJob("BuilderContainerProducesScratchImageWithBinary", t.BuilderContainerProducesScratchImageWithBinary)
	jobs = jobs.WithJob("GoAppCiRejectsMissingGitDir", t.GoAppCiRejectsMissingGitDir)
	jobs = jobs.WithJob("GoAppCiPassesForValidSource", t.GoAppCiPassesForValidSource)
	jobs = jobs.WithJob("GoAppCiSkipsPublishWhenNoRefMatches", t.GoAppCiSkipsPublishWhenNoRefMatches)
	jobs = jobs.WithJob("GoAppCiErrorsWhenPublishOnMatchesButCredsMissing", t.GoAppCiErrorsWhenPublishOnMatchesButCredsMissing)
	jobs = jobs.WithJob("GoAppCiPublishesOnMatchingBranch", t.GoAppCiPublishesOnMatchingBranch)
	jobs = jobs.WithJob("GoAppCiPublishesOnMatchingTag", t.GoAppCiPublishesOnMatchingTag)
	jobs = jobs.WithJob("GoAppCiPublishesToAllMatchingTags", t.GoAppCiPublishesToAllMatchingTags)
	jobs = jobs.WithJob("GoAppCiReturnsThePushedDigest", t.GoAppCiReturnsThePushedDigest)
	jobs = jobs.WithJob("GoAppCiRefusesPlaintextRegistryUnlessInsecure", t.GoAppCiRefusesPlaintextRegistryUnlessInsecure)
	jobs = jobs.WithJob("GoAppCiNormalizesRemoteOriginRefs", t.GoAppCiNormalizesRemoteOriginRefs)
	jobs = jobs.WithJob("GoAppCiTagBeatsBranch", t.GoAppCiTagBeatsBranch)
	jobs = jobs.WithJob("GoAppBuildFailsWithoutGitMetadata", t.GoAppBuildFailsWithoutGitMetadata)
	jobs = jobs.WithJob("GoAppStampsWhenPublishOnDoesNotMatch", t.GoAppStampsWhenPublishOnDoesNotMatch)
	jobs = jobs.WithJob("GoAppCiStampedBinaryMatchesImageTagAndBuilder", t.GoAppCiStampedBinaryMatchesImageTagAndBuilder)
	jobs = jobs.WithJob("GoAppCiStampsEveryPlatformVariant", t.GoAppCiStampsEveryPlatformVariant)
	jobs = jobs.WithJob("GoAppCiRebuildIsByteIdenticalPerPlatform", t.GoAppCiRebuildIsByteIdenticalPerPlatform)
	jobs = jobs.WithJob("GoAppCiAnnotatesEveryPlatformVariant", t.GoAppCiAnnotatesEveryPlatformVariant)
	jobs = jobs.WithJob("GoAppCiAttachesSbomsAndProvenance", t.GoAppCiAttachesSbomsAndProvenance)
	jobs = jobs.WithJob("GoAppCiAttestsTwoSegmentBinaryNames", t.GoAppCiAttestsTwoSegmentBinaryNames)
	jobs = jobs.WithJob("GoAppCiRefusesToPublishWithoutProvenanceMachinery", t.GoAppCiRefusesToPublishWithoutProvenanceMachinery)
	jobs = jobs.WithJob("GoAppCiRedactsCredentialsFromTheSourceAnnotation", t.GoAppCiRedactsCredentialsFromTheSourceAnnotation)

	return jobs.Run(ctx)
}

// localRegistry stands up a docker registry:2 service with htpasswd auth.
// Returns the service, the plaintext password (for curl probes), and the
// password as a *dagger.Secret (for GoApp.Auth). User is always "ci".
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

// curlProbeManifest issues a basic-auth GET against the registry's
// manifest endpoint and returns the HTTP status code. host is the
// registry hostname reachable from this session (use Service.Hostname).
func curlProbeManifest(ctx context.Context, svc *dagger.Service, host, user, pwd, image, tag string) (int, error) {
	out, err := dag.Container().From(curlImage).
		WithServiceBinding(host, svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`curl -s -o /dev/null -w "%%{http_code}" -H 'Accept: application/vnd.oci.image.index.v1+json' -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' -u %s:%s http://%s:5000/v2/%s/manifests/%s`,
			user, pwd, host, image, tag,
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
// The Accept headers matter: a registry serves whichever manifest kind the
// client will take, and the digest of a manifest list is not the digest of
// any image inside it. Naming the index types first is what makes this the
// digest of what was actually pushed.
func curlManifestDigest(ctx context.Context, svc *dagger.Service, host, user, pwd, image, tag string) (string, error) {
	out, err := dag.Container().From(curlImage).
		WithServiceBinding(host, svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`curl -fsS -o /dev/null -D - -H 'Accept: application/vnd.oci.image.index.v1+json' -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' -u %s:%s http://%s:5000/v2/%s/manifests/%s`,
			user, pwd, host, image, tag,
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

// GoLibCiPassesForValidSource asserts that GoLib.Ci against a clean,
// vet-clean, gofmt-clean library fixture returns no error.
//
// This is also what proves the bundled configs/golangci.yml and the
// pinned golangci-lint speak the same dialect. The two majors reject each
// other's config files outright, so a v1 config reaching a v2 binary — or
// the reverse — fails this test before any linter runs.
func (t *Tests) GoLibCiPassesForValidSource(ctx context.Context) error {
	if err := dag.Z5Labs().GoLib(helloLibDir()).Ci(ctx); err != nil {
		return fmt.Errorf("GoLib.Ci on hello-lib: %w", err)
	}
	return nil
}

// GoLibCiRoutesLintVersion asserts the archetype's lintVersion reaches the
// lint stage rather than being accepted and dropped.
//
// It pins a version the `go` module refuses to read a major out of, so the
// assertion is a message naming that version — which can only have come
// from the lint stage. Proving routing this way costs no container work,
// and a pin that *is* valid is exercised where the behaviour lives, in the
// `go` module's own suite.
func (t *Tests) GoLibCiRoutesLintVersion(ctx context.Context) error {
	err := dag.Z5Labs().GoLib(helloLibDir(), dagger.Z5LabsGoLibOpts{
		LintVersion: "1.64.8",
	}).Ci(ctx)
	if err == nil {
		return fmt.Errorf(`expected GoLib.Ci with lintVersion "1.64.8" to fail, got nil`)
	}
	if msg := err.Error(); !strings.Contains(msg, `golangci-lint version "1.64.8"`) {
		return fmt.Errorf("expected the error to name the rejected lint version, got: %s", msg)
	}
	return nil
}

// GoAppCiPublishesOnMatchingTag asserts that a matching tag ref pushes
// to <registry>/<binary>:<stripped-tag>.
func (t *Tests) GoAppCiPublishesOnMatchingTag(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", []string{"v1.2.3"})
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           "^refs/tags/v.+",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
		Insecure:            true,
	})
	if _, err := app.Ci(ctx); err != nil {
		return fmt.Errorf("Ci: %v", err)
	}
	code, err := curlProbeManifest(ctx, svc, host, "ci", pwdHex, "hello", "v1.2.3")
	if err != nil {
		return fmt.Errorf("curl probe: %v", err)
	}
	if code != 200 {
		return fmt.Errorf("expected manifest v1.2.3 to return 200, got %d", code)
	}
	return nil
}

// GoAppCiPublishesToAllMatchingTags asserts that when multiple tag refs
// match, every one is pushed under its own image tag.
func (t *Tests) GoAppCiPublishesToAllMatchingTags(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", []string{"v1.0.0", "v1.0.1"})
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           "^refs/tags/v.+",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
		Insecure:            true,
	})
	if _, err := app.Ci(ctx); err != nil {
		return fmt.Errorf("Ci: %v", err)
	}
	for _, want := range []string{"v1.0.0", "v1.0.1"} {
		code, err := curlProbeManifest(ctx, svc, host, "ci", pwdHex, "hello", want)
		if err != nil {
			return fmt.Errorf("curl probe %s: %v", want, err)
		}
		if code != 200 {
			return fmt.Errorf("expected manifest %s to return 200, got %d", want, code)
		}
	}
	return nil
}

// GoAppCiReturnsThePushedDigest asserts Ci reports the digest of what it
// published, and that the digest is the registry's own rather than a value
// this pipeline computed for itself. A caller anchoring an attestation, a
// deployment or a release note to it has to be naming the artifact the
// registry actually holds; a tag is not an immutable name and Ci returning
// only an error left callers with no other way to say which bytes shipped.
func (t *Tests) GoAppCiReturnsThePushedDigest(ctx context.Context) error {
	const tag = "v2.0.0"
	src, err := gitFixture(ctx, helloDir(), "main", []string{tag})
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
	const host = registryAlias
	digest, err := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           "^refs/tags/v.+",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
		Insecure:            true,
	}).Ci(ctx)
	if err != nil {
		return fmt.Errorf("Ci: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("expected Ci to return a sha256 digest, got %q", digest)
	}
	stored, err := curlManifestDigest(ctx, svc, host, "ci", pwdHex, "hello", tag)
	if err != nil {
		return fmt.Errorf("read stored digest: %v", err)
	}
	if stored != digest {
		return fmt.Errorf("Ci reported digest %s, the registry holds %s for tag %s", digest, stored, tag)
	}
	return nil
}

// GoAppCiRefusesPlaintextRegistryUnlessInsecure asserts TLS verification is
// not inferred from registryService being set.
//
// The publish path used to disable verification whenever a service was
// present, so a caller who supplied one for their own reasons — a private
// registry that happens to be a Dagger service — silently published over an
// unverified connection they never asked for. Verification is now the
// caller's explicit choice, and with insecure left off a plain-HTTP registry
// is refused rather than accommodated.
func (t *Tests) GoAppCiRefusesPlaintextRegistryUnlessInsecure(ctx context.Context) error {
	const tag = "v3.0.0"
	src, err := gitFixture(ctx, helloDir(), "main", []string{tag})
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
	const host = registryAlias
	_, err = dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           "^refs/tags/v.+",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
	}).Ci(ctx)
	if err == nil {
		return fmt.Errorf("expected Ci to refuse a plain-HTTP registry with insecure off, got nil")
	}
	// The refusal has to mean nothing was pushed, not that the push
	// succeeded and the report was wrong.
	code, err := curlProbeManifest(ctx, svc, host, "ci", pwdHex, "hello", tag)
	if err != nil {
		return fmt.Errorf("curl probe: %v", err)
	}
	if code == 200 {
		return fmt.Errorf("Ci reported a failure but manifest %s is present in the registry", tag)
	}
	return nil
}

// GoAppCiNormalizesRemoteOriginRefs asserts that a HEAD ref shaped as
// refs/remotes/origin/main is normalized to refs/heads/main and matches
// publishOn="^refs/heads/main$".
func (t *Tests) GoAppCiNormalizesRemoteOriginRefs(ctx context.Context) error {
	// Build a fixture where HEAD is detached but
	// refs/remotes/origin/main points at it. Branch ref "main" should
	// not exist; the only ref at HEAD is refs/remotes/origin/main.
	ctr := dag.Go().Container(helloDir()).
		WithEnvVariable("GIT_AUTHOR_NAME", "CI").
		WithEnvVariable("GIT_AUTHOR_EMAIL", "ci@example.com").
		WithEnvVariable("GIT_COMMITTER_NAME", "CI").
		WithEnvVariable("GIT_COMMITTER_EMAIL", "ci@example.com").
		WithExec([]string{"git", "init", "--initial-branch=main", "."}).
		WithExec([]string{"git", "add", "."}).
		WithExec([]string{"git", "commit", "-m", "initial"}).
		WithExec([]string{"git", "update-ref", "refs/remotes/origin/main", "HEAD"}).
		WithExec([]string{"git", "checkout", "--detach", "HEAD"}).
		WithExec([]string{"git", "branch", "-D", "main"})
	if _, err := ctr.Sync(ctx); err != nil {
		return fmt.Errorf("build detached fixture: %v", err)
	}
	src := ctr.Directory("/src")
	svc, pwdHex, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           "^refs/heads/main$",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
		Insecure:            true,
	})
	if _, err := app.Ci(ctx); err != nil {
		return fmt.Errorf("Ci: %v", err)
	}
	tags, err := listTags(ctx, svc, host, "ci", pwdHex, "hello")
	if err != nil {
		return fmt.Errorf("listTags: %v", err)
	}
	if len(tags) != 1 {
		return fmt.Errorf("expected exactly 1 tag after publish, got %v", tags)
	}
	code, err := curlProbeManifest(ctx, svc, host, "ci", pwdHex, "hello", tags[0])
	if err != nil {
		return fmt.Errorf("curl probe: %v", err)
	}
	if code != 200 {
		return fmt.Errorf("expected branch-from-origin manifest to return 200, got %d", code)
	}
	return nil
}

// GoAppCiTagBeatsBranch asserts that when both a tag and a branch ref
// match at HEAD, both are pushed under their respective image tags
// ("tag wins precedence" semantically means the tag-named manifest is
// the canonical release; the spec also requires the branch-named one
// to be pushed).
func (t *Tests) GoAppCiTagBeatsBranch(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", []string{"v1.2.3"})
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           ".*",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
		Insecure:            true,
	})
	if _, err := app.Ci(ctx); err != nil {
		return fmt.Errorf("Ci: %v", err)
	}
	tags, err := listTags(ctx, svc, host, "ci", pwdHex, "hello")
	if err != nil {
		return fmt.Errorf("listTags: %v", err)
	}
	if len(tags) < 2 {
		return fmt.Errorf("expected at least 2 tags (one branch, one tag), got %v", tags)
	}
	sawTag := false
	sawBranch := false
	for _, tg := range tags {
		if tg == "v1.2.3" {
			sawTag = true
		} else if strings.Contains(tg, "-") {
			sawBranch = true
		}
	}
	if !sawTag || !sawBranch {
		return fmt.Errorf("expected both v1.2.3 and a branch-style tag, got %v", tags)
	}
	return nil
}

// GoAppCiPublishesOnMatchingBranch asserts a matching branch ref
// triggers a publish whose image tag is <shortSha>-<isoCommitTime>, and
// that re-running Ci produces the same tag (commit-time idempotence).
func (t *Tests) GoAppCiPublishesOnMatchingBranch(ctx context.Context) error {
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           "^refs/heads/main$",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
		Insecure:            true,
	})
	if _, err := app.Ci(ctx); err != nil {
		return fmt.Errorf("first Ci: %v", err)
	}
	tags, err := listTags(ctx, svc, host, "ci", pwdHex, "hello")
	if err != nil {
		return fmt.Errorf("list tags after first publish: %v", err)
	}
	if len(tags) != 1 {
		return fmt.Errorf("expected exactly 1 tag after first publish, got %v", tags)
	}
	tag := tags[0]
	if !strings.Contains(tag, "-") {
		return fmt.Errorf("expected branch image tag in form <sha>-<iso>, got %q", tag)
	}
	code, err := curlProbeManifest(ctx, svc, host, "ci", pwdHex, "hello", tag)
	if err != nil {
		return fmt.Errorf("curl probe: %v", err)
	}
	if code != 200 {
		return fmt.Errorf("expected manifest GET for tag %q to return 200, got %d (all tags: %v)", tag, code, tags)
	}
	// Idempotence: second run produces the same tag (commit-time, not build-time).
	if _, err := app.Ci(ctx); err != nil {
		return fmt.Errorf("second Ci: %v", err)
	}
	tags2, err := listTags(ctx, svc, host, "ci", pwdHex, "hello")
	if err != nil {
		return fmt.Errorf("list tags after second publish: %v", err)
	}
	if len(tags2) != 1 || tags2[0] != tag {
		return fmt.Errorf("expected idempotent tag across runs, got %v then %v", tags, tags2)
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

// GoAppCiErrorsWhenPublishOnMatchesButCredsMissing asserts that when a
// ref matches publishOn AND registry is set but auth is nil, GoApp.Ci
// returns an explicit error rather than silently no-op'ing.
func (t *Tests) GoAppCiErrorsWhenPublishOnMatchesButCredsMissing(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	_, err = dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn: "^refs/heads/main$",
		Registry:  "registry:5000",
	}).Ci(ctx)
	if err == nil {
		return fmt.Errorf("expected GoApp.Ci to error on missing auth, got nil")
	}
	if !strings.Contains(err.Error(), "auth is required when registry is set") {
		return fmt.Errorf("expected error to contain auth-required message, got: %s", err.Error())
	}
	return nil
}

// GoAppCiSkipsPublishWhenNoRefMatches asserts GoApp.Ci returns nil
// (no publish, no error) when no HEAD ref matches publishOn, even with
// registry + auth supplied. A bogus registry URL would error if a push
// were attempted; success = no push attempt.
func (t *Tests) GoAppCiSkipsPublishWhenNoRefMatches(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "feature/x", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	auth := dag.SetSecret("z5labs-skip-publish-auth", "dummy")
	_, err = dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:    "^refs/heads/main$",
		Registry:     "registry:5000",
		Auth:         auth,
		AuthUsername: "ci",
	}).Ci(ctx)
	if err != nil {
		return fmt.Errorf("GoApp.Ci should skip publish: %v", err)
	}
	return nil
}

// GoAppCiPassesForValidSource asserts GoApp.Ci runs end-to-end against
// a git-backed source with no publish configured.
func (t *Tests) GoAppCiPassesForValidSource(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	if _, err := dag.Z5Labs().GoApp(src).Ci(ctx); err != nil {
		return fmt.Errorf("GoApp.Ci on git-backed hello: %v", err)
	}
	return nil
}

// gitFixture overlays a fresh single-commit git repo on base. branch is
// the working-branch name; tags is a slice of annotated tags created on
// the single commit.
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

// GoAppCiRejectsMissingGitDir asserts GoApp.Ci fails fast when source
// has no .git directory.
func (t *Tests) GoAppCiRejectsMissingGitDir(ctx context.Context) error {
	_, err := dag.Z5Labs().GoApp(helloDir()).Ci(ctx)
	if err == nil {
		return fmt.Errorf("expected GoApp.Ci to error on missing .git, got nil")
	}
	if !strings.Contains(err.Error(), "git working tree") {
		return fmt.Errorf("expected error to mention \"git working tree\", got: %s", err.Error())
	}
	return nil
}

// BuilderContainerProducesScratchImageWithBinary asserts that
// Builder.Container produces a scratch image whose entrypoint runs the
// embedded binary and prints "hello". The source is git-backed because
// every build derives its stamp from HEAD.
func (t *Tests) BuilderContainerProducesScratchImageWithBinary(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	ctr := dag.Z5Labs().GoApp(src).Builder().Container()
	out, err := ctr.
		WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run scratch image entrypoint: %w", err)
	}
	if out != "hello\n" {
		return fmt.Errorf("expected %q, got %q", "hello\n", out)
	}
	return nil
}

// BuilderBinaryProducesCompiledBinary asserts that Builder.Binary
// returns a non-empty file named after the go.mod module basename. The
// source is git-backed because every build derives its stamp from HEAD.
func (t *Tests) BuilderBinaryProducesCompiledBinary(ctx context.Context) error {
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	bin := dag.Z5Labs().GoApp(src).Builder().Binary()
	size, err := bin.Size(ctx)
	if err != nil {
		return fmt.Errorf("Builder.Binary.Size: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	name, err := bin.Name(ctx)
	if err != nil {
		return fmt.Errorf("Builder.Binary.Name: %w", err)
	}
	if name != "hello" {
		return fmt.Errorf("expected binary name %q, got %q", "hello", name)
	}
	return nil
}

// GoLibCiFailsForFailingTest asserts that GoLib.Ci surfaces a test
// failure as an error containing "FAIL" or "exit code: 1".
func (t *Tests) GoLibCiFailsForFailingTest(ctx context.Context) error {
	err := dag.Z5Labs().GoLib(failingLibDir()).Ci(ctx)
	if err == nil {
		return fmt.Errorf("expected GoLib.Ci on failing-lib to error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exit code: 1") && !strings.Contains(msg, "FAIL") {
		return fmt.Errorf("expected error to contain \"exit code: 1\" or \"FAIL\", got: %s", msg)
	}
	return nil
}

// helloDir returns the on-disk hello (app) fixture.
func helloDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/hello")
}

// stampedDir returns the stamped (app) fixture: a main package that
// declares the two package-level vars GoApp stamps and prints them.
func stampedDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/stamped")
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
// short SHA and version is either a docker-tag-sanitized tag name or
// "<shortSha>-<isoCommitTime>".
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

// GoAppCiStampsEveryPlatformVariant asserts a multi-platform build stamps
// every variant. Ci collapses its per-platform images into a single
// manifest list before publishing, so a stamp applied at the image or
// publish layer would land on an artifact whose variants have already been
// merged; only a stamp applied in the per-variant compile reaches them
// all. Each variant is therefore pulled back individually: the one
// matching the engine's architecture is executed, and the foreign one —
// which cannot be executed here — is searched for the stamped bytes.
func (t *Tests) GoAppCiStampsEveryPlatformVariant(ctx context.Context) error {
	const tag = "v9.9.9"
	src, err := gitFixture(ctx, stampedDir(), "main", []string{tag})
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
	const host = registryAlias
	platforms := []string{"linux/amd64", "linux/arm64"}
	_, err = dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           "^refs/tags/v.+",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
		Insecure:            true,
		Platforms:           platforms,
	}).Ci(ctx)
	if err != nil {
		return fmt.Errorf("Ci: %v", err)
	}
	sha, err := headShortSha(ctx, src)
	if err != nil {
		return err
	}
	for _, p := range platforms {
		variant := pullVariant(svc, host, "ci", pwdHex, "stamped", tag, p, "")
		if p == hostPlatform() {
			version, commit, err := stampOf(ctx, variant)
			if err != nil {
				return fmt.Errorf("%s: %v", p, err)
			}
			if version != tag || commit != sha {
				return fmt.Errorf("%s: expected version=%q commit=%q, got version=%q commit=%q", p, tag, sha, version, commit)
			}
			continue
		}
		for _, want := range []string{tag, sha} {
			found, err := binaryContains(ctx, variant.File("/app/stamped"), want)
			if err != nil {
				return fmt.Errorf("%s: scan binary: %v", p, err)
			}
			if !found {
				return fmt.Errorf("%s: expected the binary to carry %q", p, want)
			}
		}
		// Negative control: a scan that answers yes to everything
		// would make the two assertions above vacuous.
		found, err := binaryContains(ctx, variant.File("/app/stamped"), "v0.0.0-never-stamped")
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
// nonce varies the exec's cache key so the two calls really are two
// separate git invocations rather than one cached result — otherwise a
// reproducibility assertion would be comparing an artifact against itself.
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
		WithExec([]string{"git", "remote", "add", "origin", fixtureOriginURL})
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	return ctr.Directory("/src"), nil
}

// GoAppCiRebuildIsByteIdenticalPerPlatform asserts building one commit
// twice produces byte-identical binaries for every platform. The two runs
// build from independently created working trees of the same commit, so
// each is a real compile rather than a cache hit on the first — what makes
// them agree is that both the stamp and everything else in the link line
// are functions of the commit alone.
func (t *Tests) GoAppCiRebuildIsByteIdenticalPerPlatform(ctx context.Context) error {
	svc, pwdHex, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}
	const host = registryAlias
	platforms := []string{"linux/amd64", "linux/arm64"}
	runs := make([]map[string]string, 0, 2)
	imageTag := ""
	for _, nonce := range []string{"run-a", "run-b"} {
		src, err := pinnedGitFixture(ctx, stampedDir(), "main", nonce)
		if err != nil {
			return fmt.Errorf("pinnedGitFixture %s: %v", nonce, err)
		}
		_, err = dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
			PublishOn:           "^refs/heads/main$",
			Registry:            host + ":5000",
			AuthUsername:        "ci",
			Auth:                secret,
			RegistryService:     svc,
			IDTokenRequestURL:   prov.URL,
			IDTokenRequestToken: prov.RequestToken,
			IDTokenService:      prov.Service,
			SigningKey:          prov.SigningKey,
			Insecure:            true,
			Platforms:           platforms,
		}).Ci(ctx)
		if err != nil {
			return fmt.Errorf("Ci %s: %v", nonce, err)
		}
		tags, err := listTags(ctx, svc, host, "ci", pwdHex, "stamped")
		if err != nil {
			return fmt.Errorf("listTags %s: %v", nonce, err)
		}
		if len(tags) != 1 {
			return fmt.Errorf("%s: expected exactly 1 published tag, got %v", nonce, tags)
		}
		if imageTag == "" {
			imageTag = tags[0]
		} else if tags[0] != imageTag {
			return fmt.Errorf("expected both runs to publish tag %q, second run published %q", imageTag, tags[0])
		}
		// Digest eagerly: the next run overwrites this tag.
		digests := make(map[string]string, len(platforms))
		for _, p := range platforms {
			d, err := pullVariant(svc, host, "ci", pwdHex, "stamped", imageTag, p, nonce).
				File("/app/stamped").
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
			return fmt.Errorf("%s: expected byte-identical rebuild, got %q then %q", p, runs[0][p], runs[1][p])
		}
	}
	return nil
}

// GoAppCiStampedBinaryMatchesImageTagAndBuilder asserts three things about
// a binary Ci built, by pulling it back out of the registry and running
// it: it reports a version and a commit at all; its version is exactly the
// tag of the image carrying it, which is what makes the two agree by
// construction on the branch rule "<shortSha>-<isoCommitTime>"; and
// Builder produces the identical stamp, so a local build is the same
// artifact.
func (t *Tests) GoAppCiStampedBinaryMatchesImageTagAndBuilder(ctx context.Context) error {
	src, err := gitFixture(ctx, stampedDir(), "main", nil)
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:           "^refs/heads/main$",
		Registry:            host + ":5000",
		AuthUsername:        "ci",
		Auth:                secret,
		RegistryService:     svc,
		IDTokenRequestURL:   prov.URL,
		IDTokenRequestToken: prov.RequestToken,
		IDTokenService:      prov.Service,
		SigningKey:          prov.SigningKey,
		Insecure:            true,
		Platforms:           []string{hostPlatform()},
	})
	if _, err := app.Ci(ctx); err != nil {
		return fmt.Errorf("Ci: %v", err)
	}
	tags, err := listTags(ctx, svc, host, "ci", pwdHex, "stamped")
	if err != nil {
		return fmt.Errorf("listTags: %v", err)
	}
	if len(tags) != 1 {
		return fmt.Errorf("expected exactly 1 published tag, got %v", tags)
	}
	imageTag := tags[0]

	version, commit, err := stampOf(ctx, pullVariant(svc, host, "ci", pwdHex, "stamped", imageTag, hostPlatform(), ""))
	if err != nil {
		return err
	}
	sha, err := headShortSha(ctx, src)
	if err != nil {
		return err
	}
	if commit != sha {
		return fmt.Errorf("expected commit stamp %q, got %q", sha, commit)
	}
	if version != imageTag {
		return fmt.Errorf("expected stamped version to equal image tag %q, got %q", imageTag, version)
	}

	localVersion, localCommit, err := stampOf(ctx, app.Builder().Container())
	if err != nil {
		return fmt.Errorf("Builder: %v", err)
	}
	if localVersion != version || localCommit != commit {
		return fmt.Errorf(
			"expected Builder to stamp as Ci did (version=%q commit=%q), got version=%q commit=%q",
			version, commit, localVersion, localCommit,
		)
	}
	return nil
}

// GoAppStampsWhenPublishOnDoesNotMatch asserts stamping is not gated on
// publishOn: a build from a branch the publish filter rejects still
// carries a version and commit derived from HEAD.
func (t *Tests) GoAppStampsWhenPublishOnDoesNotMatch(ctx context.Context) error {
	src, err := gitFixture(ctx, stampedDir(), "feature/x", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn: "^refs/heads/main$",
	})
	version, commit, err := stampOf(ctx, app.Builder().Container())
	if err != nil {
		return err
	}
	sha, err := headShortSha(ctx, src)
	if err != nil {
		return err
	}
	if commit != sha {
		return fmt.Errorf("expected commit stamp %q, got %q", sha, commit)
	}
	// The branch rule: "<shortSha>-<isoCommitTime>". "dev" is the
	// fixture's own default, i.e. an unstamped build.
	if !strings.HasPrefix(version, sha+"-") {
		return fmt.Errorf("expected version stamp to start with %q, got %q", sha+"-", version)
	}
	return nil
}

// GoAppBuildFailsWithoutGitMetadata asserts a source with no git metadata
// at HEAD fails with a message about the stamp rather than leaking a bare
// git error. Builder is the path that reaches the build without Ci's
// working-tree precondition.
func (t *Tests) GoAppBuildFailsWithoutGitMetadata(ctx context.Context) error {
	_, err := dag.Z5Labs().GoApp(stampedDir()).Builder().Binary().Size(ctx)
	if err == nil {
		return fmt.Errorf("expected Builder.Binary to error without git metadata, got nil")
	}
	if !strings.Contains(err.Error(), "could not derive build stamp") {
		return fmt.Errorf("expected a stamp-derivation message, got: %s", err.Error())
	}
	return nil
}

// helloLibDir returns the on-disk hello-lib fixture (library variant).
func helloLibDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/hello-lib")
}

// failingLibDir returns the failing-lib fixture (test fails).
func failingLibDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/failing-lib")
}
