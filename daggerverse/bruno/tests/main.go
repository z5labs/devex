// Package main implements the test module for the bruno Dagger module. Each
// test is exposed as a standalone dagger function so it can be invoked
// individually during TDD; All wires them up for parallel execution under
// `dagger call all`.
package main

import (
	"context"
	"fmt"
	"strings"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

const (
	// pinnedVersion is the Bruno CLI release New defaults to. The version
	// tests assert the image actually ships it, so a bumped default that
	// resolves to a different release fails loudly rather than silently
	// running some other CLI.
	pinnedVersion = "3.4.2"
)

type Tests struct{}

// All runs every bruno-module test in parallel.
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

	jobs = jobs.WithJob("VersionReportsPinnedRelease", t.VersionReportsPinnedRelease)
	jobs = jobs.WithJob("RunPassesAgainstBoundService", t.RunPassesAgainstBoundService)
	jobs = jobs.WithJob("RunFailsOnAssertionFailure", t.RunFailsOnAssertionFailure)
	jobs = jobs.WithJob("UnknownEnvironmentIsRejected", t.UnknownEnvironmentIsRejected)
	jobs = jobs.WithJob("RunShouldNotBeCached", t.RunShouldNotBeCached)
	jobs = jobs.WithJob("RecursiveDefaultReachesSubfolders", t.RecursiveDefaultReachesSubfolders)
	jobs = jobs.WithJob("ReportEmitsJunitForFailingRun", t.ReportEmitsJunitForFailingRun)
	jobs = jobs.WithJob("ReportRejectsUnknownFormat", t.ReportRejectsUnknownFormat)
	jobs = jobs.WithJob("ReportShouldNotBeCached", t.ReportShouldNotBeCached)
	jobs = jobs.WithJob("WithVarOverridesEnvironmentValue", t.WithVarOverridesEnvironmentValue)
	jobs = jobs.WithJob("SecretVarIsNotOnArgv", t.SecretVarIsNotOnArgv)
	jobs = jobs.WithJob("PassThroughFlagsAreAccepted", t.PassThroughFlagsAreAccepted)
	jobs = jobs.WithJob("JsonEnvFileKeepsItsExtension", t.JsonEnvFileKeepsItsExtension)

	return jobs.Run(ctx)
}

// VersionReportsPinnedRelease checks that both image variants report the
// release the module pins. The Debian variant is not cosmetic — it is the
// escape hatch for the Alpine image's musl/OpenSSL TLS failures — so it has
// to be a tag that exists and ships the same CLI.
func (t *Tests) VersionReportsPinnedRelease(ctx context.Context) error {
	for _, debian := range []bool{false, true} {
		got, err := dag.Bruno(dagger.BrunoOpts{Debian: debian}).Version(ctx)
		if err != nil {
			return fmt.Errorf("version (debian=%t): %w", debian, err)
		}
		if strings.TrimSpace(got) != pinnedVersion {
			return fmt.Errorf("expected version %q (debian=%t), got %q", pinnedVersion, debian, got)
		}
	}
	return nil
}

// RunPassesAgainstBoundService checks the whole point of the module: a
// collection whose environment names a bound service reaches it and passes.
func (t *Tests) RunPassesAgainstBoundService(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	out, err := dag.Bruno().
		Collection(fixture("api")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		Run(ctx)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if !strings.Contains(out, "health") {
		return fmt.Errorf("expected the run output to name the health request, got:\n%s", head(out))
	}

	got, err := rsp.stats(ctx, "after-run")
	if err != nil {
		return err
	}
	if got.Count == 0 {
		return fmt.Errorf("expected the collection to reach the bound service, but it served no requests")
	}
	return nil
}

// RunFailsOnAssertionFailure checks that exit 1 — a failing request, test or
// assertion — is an error, and that the error carries bru's own account of
// what failed rather than just the exit code.
func (t *Tests) RunFailsOnAssertionFailure(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	_, err = dag.Bruno().
		Collection(fixture("failing")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		Run(ctx)
	if err == nil {
		return fmt.Errorf("expected a failing assertion to be an error")
	}
	if !strings.Contains(err.Error(), "expected 200 to equal 418") {
		return fmt.Errorf("expected the error to carry bru's failure output, got:\n%s", head(err.Error()))
	}
	// A failing collection is a result, not a fault: it must not be dressed
	// up as one of bru's usage errors.
	if strings.Contains(err.Error(), "exit ") {
		return fmt.Errorf("expected a collection failure to read as a failure, not a usage error, got:\n%s", head(err.Error()))
	}
	return nil
}

// UnknownEnvironmentIsRejected checks that a usage error stays a usage error.
// bru exits 6 when the named environment does not exist; conflating that with
// exit 1 would make "you typo'd the environment name" read as "your API is
// broken".
func (t *Tests) UnknownEnvironmentIsRejected(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	_, err = dag.Bruno().
		Collection(fixture("single")).
		WithEnvironment("nope").
		WithService(responderAlias, rsp.Svc).
		Run(ctx)
	if err == nil {
		return fmt.Errorf("expected an unknown environment to be an error")
	}
	for _, want := range []string{"exit 6", "environment was not found"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}
	return nil
}

// RunShouldNotBeCached checks that two identical Runs each reach the service.
// A collection run hits a live API, so a cached pass would report a
// now-broken API as green.
func (t *Tests) RunShouldNotBeCached(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	collection := dag.Bruno().
		Collection(fixture("single")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc)

	for i := 1; i <= 2; i++ {
		if _, err := collection.Run(ctx); err != nil {
			return fmt.Errorf("run %d: %w", i, err)
		}
		got, err := rsp.stats(ctx, fmt.Sprintf("after-run-%d", i))
		if err != nil {
			return err
		}
		if got.Count != i {
			return fmt.Errorf("expected %d request(s) at the service after %d run(s), got %d", i, i, got.Count)
		}
	}
	return nil
}

// RecursiveDefaultReachesSubfolders checks that the recursive default executes
// a request nested one folder deep — the api fixture keeps one request at the
// collection root and one in nested/, so a run that only reached the root
// would serve one request instead of two.
//
// The non-recursive case is not exercised from here: a `+default=true` bool
// cannot be set false through the Go SDK — querybuilder drops the zero value
// and the default wins — so `Recursive: false` would silently assert the
// default all over again. It is reachable from the CLI as
// `run --recursive=false`.
func (t *Tests) RecursiveDefaultReachesSubfolders(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	out, err := dag.Bruno().
		Collection(fixture("api")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		Run(ctx)
	if err != nil {
		return fmt.Errorf("recursive run: %w", err)
	}
	if !strings.Contains(out, "deep") {
		return fmt.Errorf("expected the run to name the nested request, got:\n%s", head(out))
	}
	got, err := rsp.stats(ctx, "after-recursive")
	if err != nil {
		return err
	}
	if got.Count != 2 {
		return fmt.Errorf("expected both the root and the nested request to be served (2 total), got %d", got.Count)
	}
	return nil
}

// ReportEmitsJunitForFailingRun checks the half of the Run/Report split that
// makes Report worth having: a failing collection still hands back its
// artifact, and the artifact says what failed. Run would have raised an error
// here, and an error forfeits the value — which is why the two are separate
// functions rather than one.
func (t *Tests) ReportEmitsJunitForFailingRun(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	report := dag.Bruno().
		Collection(fixture("failing")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		Report("junit")
	contents, err := exportContents(ctx, report, "junit-failing.xml")
	if err != nil {
		return fmt.Errorf("expected a failing collection to still return its report: %w", err)
	}
	for _, want := range []string{"<testsuites>", `status="fail"`, "expected 200 to equal 418"} {
		if !strings.Contains(contents, want) {
			return fmt.Errorf("expected the junit report to contain %q, got:\n%s", want, head(contents))
		}
	}
	return nil
}

// ReportRejectsUnknownFormat checks that a format bru does not write is
// refused by name, before a live collection has been run against a live
// service to find out.
func (t *Tests) ReportRejectsUnknownFormat(ctx context.Context) error {
	// The rejection surfaces on the read rather than on the call: a
	// File-returning function is lazy, so its error travels with the file.
	_, err := dag.Bruno().
		Collection(fixture("single")).
		WithEnvironment("local").
		Report("yaml").
		Contents(ctx)
	if err == nil {
		return fmt.Errorf("expected an unknown reporter format to be an error")
	}
	for _, want := range []string{"yaml", "json", "junit", "html"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %s", want, err.Error())
		}
	}
	return nil
}

// ReportShouldNotBeCached checks that two identical Reports each reach the
// service. A report of a run that never happened describes an API nobody
// asked about.
func (t *Tests) ReportShouldNotBeCached(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	collection := dag.Bruno().
		Collection(fixture("single")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc)

	for i := 1; i <= 2; i++ {
		// The artifact is exported rather than left unread: Report returns a
		// lazy *dagger.File, and a file nobody reads is a run nobody made.
		contents, err := exportContents(ctx, collection.Report("junit"), fmt.Sprintf("junit-uncached-%d.xml", i))
		if err != nil {
			return fmt.Errorf("report %d: %w", i, err)
		}
		if !strings.Contains(contents, "<testsuites>") {
			return fmt.Errorf("report %d is not a junit document:\n%s", i, head(contents))
		}
		got, err := rsp.stats(ctx, fmt.Sprintf("after-report-%d", i))
		if err != nil {
			return err
		}
		if got.Count != i {
			return fmt.Errorf("expected %d request(s) at the service after %d report(s), got %d", i, i, got.Count)
		}
	}
	return nil
}

// WithVarOverridesEnvironmentValue checks that an override beats the value the
// selected environment declares. The fixture puts the variable in the request
// path, so the responder records which of the two won rather than the module
// having to trust bru's summary.
func (t *Tests) WithVarOverridesEnvironmentValue(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	const override = "from-with-var"
	if _, err := dag.Bruno().
		Collection(fixture("vars")).
		WithEnvironment("local").
		WithVar("greeting", override).
		WithService(responderAlias, rsp.Svc).
		Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	got, err := rsp.stats(ctx, "after-override")
	if err != nil {
		return err
	}
	if want := "/echo/" + override; got.Path != want {
		return fmt.Errorf("expected the request path %q, got %q", want, got.Path)
	}
	return nil
}

// SecretVarIsNotOnArgv checks the reason WithSecretVar exists at all.
//
// `--env-var name=value` puts its value on bru's command line, so a secret
// passed that way is readable by anything that can see the process table and
// lands in any diagnostic that echoes the invocation. WithSecretVar binds it
// as an environment variable instead, reachable from the collection as
// {{process.env.NAME}}.
//
// The fixture's pre-request script reports bru's own argv back to the
// responder, which is the only vantage point from which "it never reached the
// command line" can actually be checked. That script needs the developer
// sandbox — the safe one has no process — which is what makes WithSandbox
// part of this test.
func (t *Tests) SecretVarIsNotOnArgv(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	// No credential is ever a literal in this repo, not even a throwaway one
	// for a service that exists for four seconds.
	value, err := dag.Random().Sha256(ctx)
	if err != nil {
		return fmt.Errorf("mint the test token: %w", err)
	}
	secret := dag.SetSecret("bruno-api-token", value)

	out, err := dag.Bruno().
		Collection(fixture("secret")).
		WithEnvironment("local").
		WithSecretVar("API_TOKEN", secret).
		WithSandbox("developer").
		WithService(responderAlias, rsp.Svc).
		Run(ctx)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	got, err := rsp.stats(ctx, "after-secret")
	if err != nil {
		return err
	}
	if got.Token != value {
		return fmt.Errorf("expected the secret to reach the request as its X-Token header, got %q", got.Token)
	}
	if got.Argv == "" {
		return fmt.Errorf("the fixture reported no argv, so this test proves nothing; check the pre-request script and the sandbox mode")
	}
	if strings.Contains(got.Argv, value) {
		return fmt.Errorf("the secret is on bru's command line: %s", got.Argv)
	}
	if strings.Contains(out, value) {
		return fmt.Errorf("the secret is in bru's output:\n%s", head(out))
	}
	return nil
}

// PassThroughFlagsAreAccepted covers the modifiers whose behaviour is bru's
// rather than this module's: the module's share of them is rendering the right
// flag, and a wrong flag name is a usage error rather than a wrong result. So
// they are set together on one run, which passes only if bru accepted every
// one of them.
//
// The api fixture tags its root request and leaves the nested one untagged, so
// the tag filters also have to have selected the right request rather than
// merely been tolerated.
func (t *Tests) PassThroughFlagsAreAccepted(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	if _, err := dag.Bruno().
		Collection(fixture("api")).
		WithEnvFile(fixtureFile("env/api.bru")).
		WithTags([]string{"smoke"}).
		WithoutTags([]string{"wip"}).
		WithTestsOnly().
		WithBail().
		WithDelay(10).
		WithInsecure().
		WithService(responderAlias, rsp.Svc).
		Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	got, err := rsp.stats(ctx, "after-flags")
	if err != nil {
		return err
	}
	if got.Count != 1 {
		return fmt.Errorf("expected the tag filter to leave 1 request, got %d", got.Count)
	}
	if got.Path != "/health" {
		return fmt.Errorf("expected the tagged request to be the one that ran, got %q", got.Path)
	}
	return nil
}

// JsonEnvFileKeepsItsExtension checks that an environment file is mounted
// under the extension it arrived with. bru picks its environment parser from
// that extension and not from the contents, so a JSON environment staged as
// .bru dies inside the Bruno grammar — with a Node stack trace, several layers
// away from anything the caller did.
func (t *Tests) JsonEnvFileKeepsItsExtension(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	if _, err := dag.Bruno().
		Collection(fixture("single")).
		WithEnvFile(fixtureFile("env/api.json")).
		WithService(responderAlias, rsp.Svc).
		Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	got, err := rsp.stats(ctx, "after-json-env-file")
	if err != nil {
		return err
	}
	if got.Count != 1 {
		return fmt.Errorf("expected the JSON environment to resolve baseUrl and reach the service, served %d requests", got.Count)
	}
	return nil
}

// ------------------------------------------------------------------ helpers

// fixture returns the named hand-authored Bruno collection under fixtures/.
func fixture(name string) *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/" + name)
}

// exportContents round-trips an artifact through the module's own workdir and
// returns what it holds. A reporter artifact is only evidence once something
// has actually read it: the file Report returns is lazy, and reading it is
// what forces the run behind it.
func exportContents(ctx context.Context, f *dagger.File, name string) (string, error) {
	if _, err := f.Export(ctx, name); err != nil {
		return "", fmt.Errorf("export %s: %w", name, err)
	}
	contents, err := dag.CurrentModule().WorkdirFile(name).Contents(ctx)
	if err != nil {
		return "", fmt.Errorf("read exported %s: %w", name, err)
	}
	return contents, nil
}

// fixtureFile returns a single file under fixtures/, for the inputs — an
// environment file — that a function takes on its own rather than as part of
// a collection.
func fixtureFile(path string) *dagger.File {
	return dag.CurrentModule().Source().File("fixtures/" + path)
}

// head trims a long output down to something readable in a failure message.
func head(s string) string {
	const limit = 2048
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n..."
}
