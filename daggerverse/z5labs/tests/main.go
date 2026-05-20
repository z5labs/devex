// Package main implements the test module for daggerverse/z5labs.
// Each test is exposed as a standalone Dagger function so it can be
// invoked individually during TDD; All wires them up for parallel
// execution under `dagger call all`.
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
)

// registryAlias is the WithServiceBinding alias used wherever a test
// containerized client needs to reach the local registry:2 service.
const registryAlias = "registry"

type Tests struct{}

// All runs every z5labs test. parallel caps concurrency; defaults to 1
// (sequential).
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=1
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
	jobs = jobs.WithJob("BuilderBinaryProducesCompiledBinary", t.BuilderBinaryProducesCompiledBinary)
	jobs = jobs.WithJob("BuilderContainerProducesScratchImageWithBinary", t.BuilderContainerProducesScratchImageWithBinary)
	jobs = jobs.WithJob("GoAppCiRejectsMissingGitDir", t.GoAppCiRejectsMissingGitDir)
	jobs = jobs.WithJob("GoAppCiPassesForValidSource", t.GoAppCiPassesForValidSource)
	jobs = jobs.WithJob("GoAppCiSkipsPublishWhenNoRefMatches", t.GoAppCiSkipsPublishWhenNoRefMatches)
	jobs = jobs.WithJob("GoAppCiErrorsWhenPublishOnMatchesButCredsMissing", t.GoAppCiErrorsWhenPublishOnMatchesButCredsMissing)
	jobs = jobs.WithJob("GoAppCiPublishesOnMatchingBranch", t.GoAppCiPublishesOnMatchingBranch)
	jobs = jobs.WithJob("GoAppCiPublishesOnMatchingTag", t.GoAppCiPublishesOnMatchingTag)
	jobs = jobs.WithJob("GoAppCiPublishesToAllMatchingTags", t.GoAppCiPublishesToAllMatchingTags)
	jobs = jobs.WithJob("GoAppCiNormalizesRemoteOriginRefs", t.GoAppCiNormalizesRemoteOriginRefs)
	jobs = jobs.WithJob("GoAppCiTagBeatsBranch", t.GoAppCiTagBeatsBranch)

	return jobs.Run(ctx)
}

// localRegistry stands up a docker registry:2 service with htpasswd auth.
// Returns the service, the plaintext password (for curl probes), and the
// password as a *dagger.Secret (for GoApp.Auth). User is always "ci".
func localRegistry(ctx context.Context) (*dagger.Service, string, *dagger.Secret, error) {
	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("random sha256: %v", err)
	}
	htpasswdFile := dag.Container().From("httpd:2.4-alpine").
		WithExec([]string{"sh", "-c", "htpasswd -Bbn ci " + pwdHex + " > /tmp/htpasswd"}).
		File("/tmp/htpasswd")
	svc := dag.Container().From("registry:2").
		WithMountedFile("/auth/htpasswd", htpasswdFile).
		WithEnvVariable("REGISTRY_AUTH", "htpasswd").
		WithEnvVariable("REGISTRY_AUTH_HTPASSWD_REALM", "Registry").
		WithEnvVariable("REGISTRY_AUTH_HTPASSWD_PATH", "/auth/htpasswd").
		WithExposedPort(5000).
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true})
	secret := dag.SetSecret("z5labs-registry-pwd-"+pwdHex[:16], pwdHex)
	return svc, pwdHex, secret, nil
}

// curlProbeManifest issues a basic-auth GET against the registry's
// manifest endpoint and returns the HTTP status code. host is the
// registry hostname reachable from this session (use Service.Hostname).
func curlProbeManifest(ctx context.Context, svc *dagger.Service, host, user, pwd, image, tag string) (int, error) {
	out, err := dag.Container().From("curlimages/curl:latest").
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

// GoLibCiPassesForValidSource asserts that GoLib.Ci against a clean,
// vet-clean, gofmt-clean library fixture returns no error.
func (t *Tests) GoLibCiPassesForValidSource(ctx context.Context) error {
	if err := dag.Z5Labs().GoLib(helloLibDir()).Ci(ctx); err != nil {
		return fmt.Errorf("GoLib.Ci on hello-lib: %w", err)
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:       "^refs/tags/v.+",
		Registry:        host + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
	})
	if err := app.Ci(ctx); err != nil {
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:       "^refs/tags/v.+",
		Registry:        host + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
	})
	if err := app.Ci(ctx); err != nil {
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:       "^refs/heads/main$",
		Registry:        host + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
	})
	if err := app.Ci(ctx); err != nil {
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:       ".*",
		Registry:        host + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
	})
	if err := app.Ci(ctx); err != nil {
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
	const host = registryAlias
	app := dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
		PublishOn:       "^refs/heads/main$",
		Registry:        host + ":5000",
		AuthUsername:    "ci",
		Auth:            secret,
		RegistryService: svc,
	})
	if err := app.Ci(ctx); err != nil {
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
	if err := app.Ci(ctx); err != nil {
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
	out, err := dag.Container().From("curlimages/curl:latest").
		WithServiceBinding(host, svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`curl -fs -u %s:%s http://%s:5000/v2/%s/tags/list`,
			user, pwd, host, image,
		)}).
		Stdout(ctx)
	if err != nil {
		return nil, err
	}
	return parseTagsList(out)
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
	err = dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
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
	err = dag.Z5Labs().GoApp(src, dagger.Z5LabsGoAppOpts{
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
	if err := dag.Z5Labs().GoApp(src).Ci(ctx); err != nil {
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
		WithExec([]string{"git", "commit", "-m", "initial"})
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
	err := dag.Z5Labs().GoApp(helloDir()).Ci(ctx)
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
// embedded binary and prints "hello".
func (t *Tests) BuilderContainerProducesScratchImageWithBinary(ctx context.Context) error {
	ctr := dag.Z5Labs().GoApp(helloDir()).Builder().Container()
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
// returns a non-empty file named after the go.mod module basename.
func (t *Tests) BuilderBinaryProducesCompiledBinary(ctx context.Context) error {
	bin := dag.Z5Labs().GoApp(helloDir()).Builder().Binary()
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

// helloLibDir returns the on-disk hello-lib fixture (library variant).
func helloLibDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/hello-lib")
}

// failingLibDir returns the failing-lib fixture (test fails).
func failingLibDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/failing-lib")
}
