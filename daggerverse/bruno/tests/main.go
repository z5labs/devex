// Package main implements the test module for the bruno Dagger module. Each
// test is exposed as a standalone dagger function so it can be invoked
// individually during TDD; All wires them up for parallel execution under
// `dagger call all`.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
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

	// specOperations is how many operations fixtures/petstore.yaml declares,
	// and therefore how many requests a collection generated from it makes.
	specOperations = 4
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
	jobs = jobs.WithJob("TlsControlsAreValidated", t.TlsControlsAreValidated)
	jobs = jobs.WithJob("CaCertReachesPrivateCaService", t.CaCertReachesPrivateCaService)
	jobs = jobs.WithJob("ClientCertAuthenticatesToMtlsService", t.ClientCertAuthenticatesToMtlsService)
	jobs = jobs.WithJob("ClientCertPassphraseUnlocksTheKey", t.ClientCertPassphraseUnlocksTheKey)
	jobs = jobs.WithJob("ClientCertMaterialStaysOutOfTheCollection", t.ClientCertMaterialStaysOutOfTheCollection)
	jobs = jobs.WithJob("CiReachesMtlsServiceBehindPrivateCa", t.CiReachesMtlsServiceBehindPrivateCa)
	jobs = jobs.WithJob("JsonEnvFileKeepsItsExtension", t.JsonEnvFileKeepsItsExtension)
	jobs = jobs.WithJob("GenerateProducesRunnableCollection", t.GenerateProducesRunnableCollection)
	jobs = jobs.WithJob("GenerateHonoursOpenCollectionFormat", t.GenerateHonoursOpenCollectionFormat)
	jobs = jobs.WithJob("GenerateRejectsUnknownFormat", t.GenerateRejectsUnknownFormat)
	jobs = jobs.WithJob("LintAcceptsValidCollection", t.LintAcceptsValidCollection)
	jobs = jobs.WithJob("LintRejectsMissingBrunoJson", t.LintRejectsMissingBrunoJson)
	jobs = jobs.WithJob("LintRejectsUnknownEnvironment", t.LintRejectsUnknownEnvironment)
	jobs = jobs.WithJob("LintRejectsUnresolvedVariable", t.LintRejectsUnresolvedVariable)
	jobs = jobs.WithJob("LintRejectsPlaintextSecret", t.LintRejectsPlaintextSecret)
	jobs = jobs.WithJob("LintRejectsDuplicateSequence", t.LintRejectsDuplicateSequence)
	jobs = jobs.WithJob("LintWarnsOnRequestWithoutTests", t.LintWarnsOnRequestWithoutTests)
	jobs = jobs.WithJob("CiRejectsUnknownReportFormat", t.CiRejectsUnknownReportFormat)
	jobs = jobs.WithJob("CiLintFailsBeforeAnyRequest", t.CiLintFailsBeforeAnyRequest)
	jobs = jobs.WithJob("CiCheckGatesOnFailingAssertion", t.CiCheckGatesOnFailingAssertion)
	jobs = jobs.WithJob("CiRunProducesEveryRequestedReport", t.CiRunProducesEveryRequestedReport)
	jobs = jobs.WithJob("CiRunStillReportsWhenTheCollectionFails", t.CiRunStillReportsWhenTheCollectionFails)
	jobs = jobs.WithJob("CiShouldNotBeCached", t.CiShouldNotBeCached)
	jobs = jobs.WithJob("CiSecretVarReachesTheCollection", t.CiSecretVarReachesTheCollection)

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

// GenerateProducesRunnableCollection checks the claim the default format is
// chosen for: what Generate hands back is a collection this module can run,
// with nothing rearranged in between.
//
// Structure alone would not establish that — a tree can hold a bruno.json and
// still fail at exit 4 — so the generated directory is fed straight into
// Collection and run against the recording responder, which is the host the
// fixture's `servers:` entry names and therefore the baseUrl `bru import`
// writes into the generated environment.
//
// The environment's name is read off the generated tree rather than hardcoded:
// bru derives it from the server's description, which is the spec's business
// and not this module's.
func (t *Tests) GenerateProducesRunnableCollection(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	const name = "petstore"
	generated := dag.Bruno().Generate(fixtureFile("petstore.yaml"), dagger.BrunoGenerateOpts{
		Name: name,
	})

	entries, err := generated.Entries(ctx)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	if !slices.Contains(entries, "bruno.json") {
		return fmt.Errorf("expected bruno.json at the generated collection root, got %v", entries)
	}

	manifest, err := generated.File("bruno.json").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read the generated bruno.json: %w", err)
	}
	if !strings.Contains(manifest, `"name": "`+name+`"`) {
		return fmt.Errorf("expected the collection name %q in bruno.json, got:\n%s", name, head(manifest))
	}

	requests, err := generatedRequests(ctx, generated)
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		return fmt.Errorf("expected at least one .bru request in the generated collection, got %v", entries)
	}

	environments, err := generated.Entries(ctx, dagger.DirectoryEntriesOpts{Path: "environments"})
	if err != nil {
		return fmt.Errorf("read the generated environments: %w", err)
	}
	if len(environments) != 1 {
		return fmt.Errorf("expected the spec's one server to become one environment, got %v", environments)
	}
	environment := strings.TrimSuffix(environments[0], ".bru")

	out, err := dag.Bruno().
		Collection(generated).
		WithEnvironment(environment).
		WithService(responderAlias, rsp.Svc).
		Run(ctx)
	if err != nil {
		return fmt.Errorf("run the generated collection: %w", err)
	}
	if !strings.Contains(out, "List every pet") {
		return fmt.Errorf("expected the run output to name a generated request, got:\n%s", head(out))
	}

	got, err := rsp.stats(ctx, "after-generated")
	if err != nil {
		return err
	}
	// The spec declares four operations, and `bru import` groups them by tag —
	// so this also says the run reached requests a folder deep, which is where
	// every generated request lives.
	if got.Count != specOperations {
		return fmt.Errorf("expected the generated collection to serve %d requests, got %d",
			specOperations, got.Count)
	}
	return nil
}

// GenerateHonoursOpenCollectionFormat checks that the format argument reaches
// bru at all: every other assertion in this suite would pass just as well
// against a Generate that had "bru" hardcoded.
//
// It also pins the reason the default diverges from upstream's. The
// opencollection shape carries no bruno.json, so it is not something Collection
// could run — which is why this module defaults to the other one.
func (t *Tests) GenerateHonoursOpenCollectionFormat(ctx context.Context) error {
	entries, err := dag.Bruno().
		Generate(fixtureFile("petstore.yaml"), dagger.BrunoGenerateOpts{Format: "opencollection"}).
		Entries(ctx)
	if err != nil {
		return fmt.Errorf("generate opencollection: %w", err)
	}
	if !slices.Contains(entries, "opencollection.yml") {
		return fmt.Errorf("expected opencollection.yml at the root, got %v", entries)
	}
	if slices.Contains(entries, "bruno.json") {
		return fmt.Errorf("expected no bruno.json in the opencollection shape, got %v", entries)
	}
	return nil
}

// GenerateRejectsUnknownFormat checks that a shape `bru import` does not write
// is refused by name. bru refuses one too, but by printing its whole help text
// with the actual complaint on the last line — and only after a container has
// been started to find out.
func (t *Tests) GenerateRejectsUnknownFormat(ctx context.Context) error {
	// As with Report, the rejection surfaces on the read rather than on the
	// call: a Directory-returning function is lazy, so its error travels with
	// the directory.
	_, err := dag.Bruno().
		Generate(fixtureFile("petstore.yaml"), dagger.BrunoGenerateOpts{Format: "postman"}).
		Entries(ctx)
	if err == nil {
		return fmt.Errorf("expected an unknown collection format to be an error")
	}
	for _, want := range []string{"postman", "bru", "opencollection"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got: %s", want, err.Error())
		}
	}
	return nil
}

// LintAcceptsValidCollection checks that a collection this suite already runs
// against a live service is also one the linter is happy with. The api fixture
// is deliberately reused rather than a purpose-built clean one: a linter that
// only accepts collections written for it is a linter nobody can adopt.
//
// It is checked under failOnWarnings=true as well, so the fixture has to be
// free of warnings and not merely free of errors.
func (t *Tests) LintAcceptsValidCollection(ctx context.Context) error {
	collection := dag.Bruno().Collection(fixture("api")).WithEnvironment("local")
	for _, strict := range []bool{false, true} {
		if err := collection.Lint(ctx, dagger.BrunoCollectionLintOpts{FailOnWarnings: strict}); err != nil {
			return fmt.Errorf("expected a clean collection to lint (failOnWarnings=%t): %w", strict, err)
		}
	}
	// The same collection with no environment selected still resolves
	// {{baseUrl}}: with nothing selected every file under environments/ counts,
	// so linting does not force a choice the caller has not made.
	if err := dag.Bruno().Collection(fixture("api")).Lint(ctx); err != nil {
		return fmt.Errorf("expected a clean collection to lint with no environment selected: %w", err)
	}
	return nil
}

// LintRejectsMissingBrunoJson checks the finding that stands in for bru's exit
// 4. bru reports that one as "not a collection root", from inside a container,
// without naming the file it wanted.
func (t *Tests) LintRejectsMissingBrunoJson(ctx context.Context) error {
	err := dag.Bruno().Collection(fixture("lint/missing-manifest")).WithEnvironment("local").Lint(ctx)
	if err == nil {
		return fmt.Errorf("expected a collection with no bruno.json to be an error")
	}
	for _, want := range []string{"bruno.json", "collection root"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}
	return nil
}

// LintRejectsUnknownEnvironment checks that a mistyped --env name is caught
// here rather than as bru's exit 6, and that the finding lists the names the
// collection does ship.
func (t *Tests) LintRejectsUnknownEnvironment(ctx context.Context) error {
	err := dag.Bruno().Collection(fixture("api")).WithEnvironment("nope").Lint(ctx)
	if err == nil {
		return fmt.Errorf("expected an unknown environment to be an error")
	}
	for _, want := range []string{`"nope"`, "environments/", `"local"`} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}
	return nil
}

// LintRejectsUnresolvedVariable checks the finding the whole function exists
// for: a {{var}} that resolves nowhere, named along with the file it appears
// in, before a request has been issued to discover it.
//
// The fixture also references two undeclared variables from its docs block,
// which bru never interpolates — so this pins that prose is not linted as
// though it were a request.
func (t *Tests) LintRejectsUnresolvedVariable(ctx context.Context) error {
	err := dag.Bruno().Collection(fixture("lint/unresolved")).WithEnvironment("local").Lint(ctx)
	if err == nil {
		return fmt.Errorf("expected an unresolved variable to be an error")
	}
	for _, want := range []string{"tenantId", "tenant.bru"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to name %q, got:\n%s", want, head(err.Error()))
		}
	}
	// baseUrl is declared by the selected environment, and the docs block's
	// references are prose. Either one showing up would mean the rule fires on
	// things that resolve perfectly well.
	for _, unwanted := range []string{"baseUrl", "alsoUndeclared"} {
		if strings.Contains(err.Error(), unwanted) {
			return fmt.Errorf("expected %q not to be reported, got:\n%s", unwanted, head(err.Error()))
		}
	}
	return nil
}

// LintRejectsPlaintextSecret checks the rule that catches a leak rather than a
// breakage: a credential-shaped variable carrying a literal in a file that is
// committed.
//
// The fixture holds two controls beside the violation — a token whose value is
// a {{process.env.*}} interpolation, and a name declared in a vars:secret
// block — so this also pins that the rule accepts the shape it is asking for.
func (t *Tests) LintRejectsPlaintextSecret(ctx context.Context) error {
	err := dag.Bruno().Collection(fixture("lint/plaintext-secret")).WithEnvironment("local").Lint(ctx)
	if err == nil {
		return fmt.Errorf("expected a plaintext credential to be an error")
	}
	for _, want := range []string{"apiKey", "environments/local.bru", "vars:secret"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}
	if strings.Contains(err.Error(), "sessionToken") {
		return fmt.Errorf("expected a {{process.env}} value not to be reported as a committed literal, got:\n%s",
			head(err.Error()))
	}
	if strings.Contains(err.Error(), "refreshToken") {
		return fmt.Errorf("expected a vars:secret name to satisfy the rule rather than trip it, got:\n%s",
			head(err.Error()))
	}
	return nil
}

// LintRejectsDuplicateSequence checks that two requests claiming the same seq
// in one folder is a finding, and that the same seq in a different folder is
// not — seq orders a folder's requests, so it is only ambiguous within one.
func (t *Tests) LintRejectsDuplicateSequence(ctx context.Context) error {
	err := dag.Bruno().Collection(fixture("lint/duplicate-seq")).WithEnvironment("local").Lint(ctx)
	if err == nil {
		return fmt.Errorf("expected a duplicated seq to be an error")
	}
	for _, want := range []string{"second.bru", "first.bru", "seq 1"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}
	if strings.Contains(err.Error(), "also-first.bru") {
		return fmt.Errorf("expected a seq reused in a different folder to be fine, got:\n%s", head(err.Error()))
	}
	return nil
}

// LintWarnsOnRequestWithoutTests checks the one finding that is a warning: a
// request that checks nothing passes whatever the API returns, which is worth
// saying and not worth failing a pipeline over by default.
//
// The fixture's only assertion is disabled, which is the same as not having
// written it — so this also pins that a commented-out assert does not count.
func (t *Tests) LintWarnsOnRequestWithoutTests(ctx context.Context) error {
	collection := dag.Bruno().Collection(fixture("lint/no-tests")).WithEnvironment("local")
	if err := collection.Lint(ctx); err != nil {
		return fmt.Errorf("expected a warning not to fail by default: %w", err)
	}
	err := collection.Lint(ctx, dagger.BrunoCollectionLintOpts{FailOnWarnings: true})
	if err == nil {
		return fmt.Errorf("expected failOnWarnings to turn the warning into an error")
	}
	for _, want := range []string{"warning", "ping.bru", "tests"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}
	return nil
}

// CiRejectsUnknownReportFormat checks that a reporter format bru has no writer
// for is caught before anything is started. WithReport has no error return, so
// the finding belongs to the terminal — and it belongs to Check as much as to
// Run, because a pipeline whose declared artifact cannot be produced is broken
// whichever terminal is invoked first.
func (t *Tests) CiRejectsUnknownReportFormat(ctx context.Context) error {
	ci := dag.Bruno().Ci(fixture("single")).WithEnvironment("local").WithReport("tap")
	err := ci.Check(ctx)
	if err == nil {
		return fmt.Errorf("expected an unknown reporter format to be an error")
	}
	for _, want := range []string{`"tap"`, "json", "junit", "html"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}
	if _, err := ci.Run().Entries(ctx); err == nil {
		return fmt.Errorf("expected Run to reject the format as well")
	}

	// The other unusable report set is the empty one. Run would otherwise run
	// the collection and hand back an empty directory, which reads as a pass;
	// the caller who wants that wants Check.
	_, err = dag.Bruno().Ci(fixture("single")).WithEnvironment("local").Run().Entries(ctx)
	if err == nil {
		return fmt.Errorf("expected Run with no requested report to be an error")
	}
	for _, want := range []string{"with-report", "check"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to point at %q, got:\n%s", want, head(err.Error()))
		}
	}
	return nil
}

// CiLintFailsBeforeAnyRequest checks the ordering that is the builder's own
// contribution: the lint stage runs ahead of the collection, so a structural
// error is reported without spending a request on discovering it.
//
// The fixture is the one whose {{tenantId}} resolves nowhere — and which bru
// runs perfectly happily, interpolating the literal string and getting a 200
// back. That is what makes the assertion mean something: the un-linted pipeline
// is checked afterwards and passes, so a request count of zero on the first
// pass is the lint stage short-circuiting rather than the collection being
// unrunnable.
func (t *Tests) CiLintFailsBeforeAnyRequest(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	ci := dag.Bruno().Ci(fixture("lint/unresolved")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc)

	err = ci.WithLint().Check(ctx)
	if err == nil {
		return fmt.Errorf("expected a lint error to fail the pipeline")
	}
	for _, want := range []string{"bru lint found", "tenantId", "tenant.bru"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to carry the lint finding %q, got:\n%s", want, head(err.Error()))
		}
	}
	got, err := rsp.stats(ctx, "after-lint-failure")
	if err != nil {
		return err
	}
	if got.Count != 0 {
		return fmt.Errorf("expected the lint stage to fail before a request was issued, but the service served %d",
			got.Count)
	}

	// The control: the same collection with no lint stage runs, so the zero
	// above was the ordering and not an unrunnable fixture.
	if err := ci.Check(ctx); err != nil {
		return fmt.Errorf("expected the un-linted pipeline to pass: %w", err)
	}
	got, err = rsp.stats(ctx, "after-unlinted-check")
	if err != nil {
		return err
	}
	if got.Count != 1 {
		return fmt.Errorf("expected the un-linted pipeline to issue 1 request, got %d", got.Count)
	}
	return nil
}

// CiCheckGatesOnFailingAssertion checks the other half of the gate: a
// collection that lints clean and then fails a request, test or assertion fails
// the pipeline, carrying bru's own account of what failed.
//
// The lint stage is enabled deliberately, so the failure has to be the
// assertion and not the linter having an opinion about the fixture.
func (t *Tests) CiCheckGatesOnFailingAssertion(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	err = dag.Bruno().Ci(fixture("failing")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		WithLint().
		Check(ctx)
	if err == nil {
		return fmt.Errorf("expected a failing assertion to fail the pipeline")
	}
	if !strings.Contains(err.Error(), "expected 200 to equal 418") {
		return fmt.Errorf("expected the error to carry bru's failure output, got:\n%s", head(err.Error()))
	}
	if strings.Contains(err.Error(), "bru lint found") {
		return fmt.Errorf("expected the gate to fail on the assertion, not on lint, got:\n%s", head(err.Error()))
	}
	got, err := rsp.stats(ctx, "after-failing-check")
	if err != nil {
		return err
	}
	if got.Count != 1 {
		return fmt.Errorf("expected the collection to reach the service once, got %d", got.Count)
	}
	return nil
}

// CiRunProducesEveryRequestedReport checks the artifact terminal: each format
// WithReport asked for comes back as its own file in the returned directory.
//
// It also pins that all of them come out of one collection pass. The api
// fixture makes two requests, so a service that served exactly two after a
// two-format Run is a pipeline that ran the collection once — and therefore two
// reports describing the same set of responses, rather than two runs of the same
// collection reporting on different ones.
func (t *Tests) CiRunProducesEveryRequestedReport(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	reports, err := pin(ctx, dag.Bruno().Ci(fixture("api")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		WithLint().
		WithReport("json").
		WithReport("junit").
		Run())
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	entries, err := reports.Entries(ctx)
	if err != nil {
		return fmt.Errorf("list the reports directory: %w", err)
	}
	slices.Sort(entries)
	if want := []string{"report.json", "report.xml"}; !slices.Equal(entries, want) {
		return fmt.Errorf("expected the reports directory to hold %v, got %v", want, entries)
	}

	// The junit artifact carries the .xml extension its reporter writes, so the
	// name has to be checked against what it is rather than what it was asked
	// for.
	jsonReport, err := exportContents(ctx, reports.File("report.json"), "ci-report.json")
	if err != nil {
		return err
	}
	var results []any
	if err := json.Unmarshal([]byte(jsonReport), &results); err != nil {
		return fmt.Errorf("expected the json report to be a JSON array, got:\n%s", head(jsonReport))
	}
	if !strings.Contains(jsonReport, "health") {
		return fmt.Errorf("expected the json report to name the health request, got:\n%s", head(jsonReport))
	}
	junitReport, err := exportContents(ctx, reports.File("report.xml"), "ci-report.xml")
	if err != nil {
		return err
	}
	for _, want := range []string{"<testsuite", "health"} {
		if !strings.Contains(junitReport, want) {
			return fmt.Errorf("expected the junit report to contain %q, got:\n%s", want, head(junitReport))
		}
	}

	got, err := rsp.stats(ctx, "after-report-run")
	if err != nil {
		return err
	}
	if got.Count != 2 {
		return fmt.Errorf("expected two formats to come out of one pass over the 2-request fixture, but the service served %d requests",
			got.Count)
	}
	return nil
}

// CiRunStillReportsWhenTheCollectionFails pins the split between the two terminals: Run
// hands back the artifact for a run whose assertions failed, and does not fail
// itself.
//
// This is the whole reason the gate is Check's job. Dagger drops a function's
// value when it also returns an error, so a Run that gated would return nothing
// on exactly the runs whose report a pipeline needs — the JUnit file a CI system
// turns into a test report names which assertion failed, and it is worthless if
// it only arrives when nothing did.
func (t *Tests) CiRunStillReportsWhenTheCollectionFails(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	ci := dag.Bruno().Ci(fixture("failing")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		WithReport("junit")

	reports, err := pin(ctx, ci.Run())
	if err != nil {
		return fmt.Errorf("expected a failing collection still to produce its report: %w", err)
	}
	report, err := exportContents(ctx, reports.File("report.xml"), "ci-failing-report.xml")
	if err != nil {
		return err
	}
	for _, want := range []string{"teapot", "<failure"} {
		if !strings.Contains(report, want) {
			return fmt.Errorf("expected the junit report to record the failure with %q, got:\n%s", want, head(report))
		}
	}

	// The same pipeline through the gate does fail, so Run's silence is the
	// split between the terminals and not a failure nobody notices.
	if err := ci.Check(ctx); err == nil {
		return fmt.Errorf("expected Check to gate the same pipeline Run reported on")
	}
	return nil
}

// CiShouldNotBeCached checks that both terminals re-run within one session.
// A pipeline hits a live API, so a cached pass would report a now-broken API as
// green — and it is the second call in a session, not the first, that a CI
// system makes when somebody re-runs a failed job.
//
// Counted at the service rather than read out of bru's summary: a replayed run
// prints a perfectly convincing "1 request, 1 passed".
func (t *Tests) CiShouldNotBeCached(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	ci := dag.Bruno().Ci(fixture("single")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		WithReport("json")

	served := 0
	for i := 1; i <= 2; i++ {
		if err := ci.Check(ctx); err != nil {
			return fmt.Errorf("check %d: %w", i, err)
		}
		served++
		got, err := rsp.stats(ctx, fmt.Sprintf("after-check-%d", i))
		if err != nil {
			return err
		}
		if got.Count != served {
			return fmt.Errorf("expected %d request(s) after %d check(s), got %d", served, i, got.Count)
		}
	}
	for i := 1; i <= 2; i++ {
		if _, err := pin(ctx, ci.Run()); err != nil {
			return fmt.Errorf("run %d: %w", i, err)
		}
		served++
		got, err := rsp.stats(ctx, fmt.Sprintf("after-run-%d", i))
		if err != nil {
			return err
		}
		if got.Count != served {
			return fmt.Errorf("expected %d request(s) after %d run(s), got %d", served, i, got.Count)
		}
	}
	return nil
}

// CiSecretVarReachesTheCollection checks that the pipeline's one secret-passing
// method actually delegates: a WithSecretVar secret is readable from the
// collection as {{process.env.NAME}} and arrives at the service.
//
// A builder that silently dropped the secret would look identical from the
// outside — the collection would send an empty header and the responder would
// answer 200 either way — so the value is read back off the request rather than
// inferred from the run passing.
//
// It runs against its own fixture rather than the one SecretVarIsNotOnArgv
// uses, because that one records bru's command line from a pre-request script
// and reading process.argv needs the developer sandbox. The pipeline builder
// wraps no sandbox switch — a collection needing one is assembled through
// Collection — so it gets the same request without the script.
func (t *Tests) CiSecretVarReachesTheCollection(ctx context.Context) error {
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

	err = dag.Bruno().Ci(fixture("ci-secret")).
		WithEnvironment("local").
		WithSecretVar("API_TOKEN", dag.SetSecret("bruno-ci-api-token", value)).
		WithService(responderAlias, rsp.Svc).
		WithLint().
		Check(ctx)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	got, err := rsp.stats(ctx, "after-ci-secret")
	if err != nil {
		return err
	}
	if got.Token != value {
		return fmt.Errorf("expected the secret to reach the request as its X-Token header, got %q", got.Token)
	}
	return nil
}

// TlsControlsAreValidated checks the two TLS combinations that cannot mean what
// they say, both of which bru accepts and then quietly does something else with.
//
// `--cacert` alongside `--insecure` is dropped by bru with a message on stderr,
// leaving a run that verifies nothing when the caller asked for verification
// against a named CA. `--ignore-truststore` is evaluated in combination with
// `--cacert` only, so on its own it is a flag that does nothing. Neither is a
// non-zero exit, which is why they are the module's to catch.
func (t *Tests) TlsControlsAreValidated(ctx context.Context) error {
	collection := dag.Bruno().Collection(fixture("single")).WithEnvironment("local")

	_, err := collection.WithInsecure().WithCaCert(fixtureFile("env/api.json")).Run(ctx)
	if err == nil {
		return fmt.Errorf("expected WithCaCert with WithInsecure to be an error")
	}
	for _, want := range []string{"WithCaCert", "WithInsecure"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}

	_, err = collection.WithoutTruststore().Run(ctx)
	if err == nil {
		return fmt.Errorf("expected WithoutTruststore without WithCaCert to be an error")
	}
	for _, want := range []string{"WithoutTruststore", "WithCaCert"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected the error to mention %q, got:\n%s", want, head(err.Error()))
		}
	}

	// A host pattern is what binds a client certificate to a request: bru skips
	// an entry with no domain rather than presenting it to everything, so an
	// empty one is a certificate that is never used.
	_, err = collection.
		WithClientCert(" ", fixtureFile("env/api.json"), dag.SetSecret("bruno-blank-host-key", "unused")).
		Run(ctx)
	if err == nil {
		return fmt.Errorf("expected a blank client certificate host to be an error")
	}
	if !strings.Contains(err.Error(), "WithClientCert") {
		return fmt.Errorf("expected the error to mention WithClientCert, got:\n%s", head(err.Error()))
	}
	return nil
}

// CaCertReachesPrivateCaService checks what WithCaCert is for: a collection whose
// target presents a certificate signed by a CA the image has never heard of
// reaches it, and verifies while doing so.
//
// The negative case is the whole point. Verification against a private CA is
// indistinguishable from no verification at all unless the run without the CA
// fails, so the same collection is run first without WithCaCert — where bru has
// to reject the peer — and then with it.
//
// The third pass adds WithoutTruststore, which narrows verification to that CA
// alone. It has to still pass: the flag is a no-op that is easy to render into a
// run that was going to pass anyway, and easy to get wrong in a way that severs
// the connection.
func (t *Tests) CaCertReachesPrivateCaService(ctx context.Context) error {
	assets, err := newTlsAssets(ctx, "cacert", responderAlias, "bruno-client")
	if err != nil {
		return err
	}
	rsp, err := newTlsResponder(ctx, assets, false)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	collection := dag.Bruno().
		Collection(fixture("tls")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc)

	_, err = collection.Run(ctx)
	if err == nil {
		return fmt.Errorf("expected a private-CA certificate to be rejected without WithCaCert")
	}
	// bru reports the rejection as a failing request — exit 1 — carrying Node's
	// own account of it.
	if !strings.Contains(err.Error(), "certificate") {
		return fmt.Errorf("expected the error to name the certificate as the problem, got:\n%s", head(err.Error()))
	}
	got, err := rsp.stats(ctx, "after-unverified")
	if err != nil {
		return err
	}
	if got.Count != 0 {
		return fmt.Errorf("expected a rejected peer to serve no request, got %d", got.Count)
	}

	if _, err := collection.WithCaCert(assets.CaCert).Run(ctx); err != nil {
		return fmt.Errorf("expected WithCaCert to verify the private-CA certificate: %w", err)
	}
	got, err = rsp.stats(ctx, "after-cacert")
	if err != nil {
		return err
	}
	if got.Count != 1 {
		return fmt.Errorf("expected the verified run to serve 1 request, got %d", got.Count)
	}

	if _, err := collection.WithCaCert(assets.CaCert).WithoutTruststore().Run(ctx); err != nil {
		return fmt.Errorf("expected WithoutTruststore to leave the private CA sufficient: %w", err)
	}
	got, err = rsp.stats(ctx, "after-ignore-truststore")
	if err != nil {
		return err
	}
	if got.Count != 2 {
		return fmt.Errorf("expected the truststore-free run to serve a 2nd request, got %d", got.Count)
	}
	return nil
}

// ClientCertAuthenticatesToMtlsService checks that WithClientCert authenticates
// the run: the responder demands a certificate signed by the test's CA, and the
// collection presents one.
//
// What is asserted is the certificate and not the handshake. The responder
// records the peer certificate's Common Name, so a run that somehow completed a
// TLS handshake without presenting anything would fail here rather than pass on
// the strength of having connected.
//
// The first pass is the control: the same collection, verifying the same server,
// with no client certificate configured. It has to fail, or the second pass says
// nothing about the certificate having been needed.
func (t *Tests) ClientCertAuthenticatesToMtlsService(ctx context.Context) error {
	const clientCn = "bruno-mtls-client"

	assets, err := newTlsAssets(ctx, "mtls", responderAlias, clientCn)
	if err != nil {
		return err
	}
	rsp, err := newTlsResponder(ctx, assets, true)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	collection := dag.Bruno().
		Collection(fixture("tls")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		WithCaCert(assets.CaCert)

	if _, err := collection.Run(ctx); err == nil {
		return fmt.Errorf("expected an mTLS endpoint to reject a run with no client certificate")
	}
	got, err := rsp.stats(ctx, "after-no-client-cert")
	if err != nil {
		return err
	}
	if got.Count != 0 {
		return fmt.Errorf("expected a rejected client to serve no request, got %d", got.Count)
	}

	if _, err := collection.
		WithClientCert(responderAlias, assets.ClientCert, assets.ClientKey).
		Run(ctx); err != nil {
		return fmt.Errorf("expected WithClientCert to authenticate to the mTLS endpoint: %w", err)
	}
	got, err = rsp.stats(ctx, "after-client-cert")
	if err != nil {
		return err
	}
	if got.Count != 1 {
		return fmt.Errorf("expected the authenticated run to serve 1 request, got %d", got.Count)
	}
	if got.Peer != clientCn {
		return fmt.Errorf("expected the request to arrive under the client certificate %q, got %q", clientCn, got.Peer)
	}

	// A second entry, for a host this collection never reaches, added ahead of
	// the one it does. Each entry is staged under its own path, so this is where
	// two of them writing over each other would show up — and the decoy carries
	// the responder's own leaf, whose Common Name is the bound alias rather than
	// the client's, so presenting the wrong one is a wrong answer and not a
	// failed handshake.
	if _, err := collection.
		WithClientCert("other.internal", assets.ServerCert, assets.ServerKey).
		WithClientCert(responderAlias, assets.ClientCert, assets.ClientKey).
		Run(ctx); err != nil {
		return fmt.Errorf("expected a second client certificate not to disturb the matching one: %w", err)
	}
	got, err = rsp.stats(ctx, "after-two-client-certs")
	if err != nil {
		return err
	}
	if got.Count != 2 {
		return fmt.Errorf("expected the two-certificate run to serve a 2nd request, got %d", got.Count)
	}
	if got.Peer != clientCn {
		return fmt.Errorf("expected the entry matching %q to be the one presented, got the certificate for %q",
			responderAlias, got.Peer)
	}
	return nil
}

// ClientCertMaterialStaysOutOfTheCollection checks the two properties the
// rendered config exists to keep: the key never reaches bru's command line, and
// nothing this module writes lands in the collection the caller handed over.
//
// Both are checked from inside the run, which is the only vantage point they are
// visible from. Nothing outside the container can read a finished process's
// command line, and the collection bru sees is the mount rather than the
// caller's directory — so the fixture's pre-request script reports argv and the
// contents of the working directory back to the responder. That script needs the
// developer sandbox, the safe one having no process and no require.
//
// The responder demands the certificate, so this is not a run that skipped the
// key: it reports the peer's Common Name back, and the key was necessary to
// produce it.
func (t *Tests) ClientCertMaterialStaysOutOfTheCollection(ctx context.Context) error {
	const clientCn = "bruno-argv-client"

	assets, err := newTlsAssets(ctx, "argv", responderAlias, clientCn)
	if err != nil {
		return err
	}
	rsp, err := newTlsResponder(ctx, assets, true)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	// The needle is one base64 line of the key itself rather than the whole PEM:
	// a multi-line document is never going to appear verbatim in a
	// space-joined command line, so searching for it would pass whatever
	// happened.
	needle, err := keyNeedle(ctx, assets.ClientKey)
	if err != nil {
		return err
	}

	out, err := dag.Bruno().
		Collection(fixture("tls-argv")).
		WithEnvironment("local").
		WithSandbox("developer").
		WithService(responderAlias, rsp.Svc).
		WithCaCert(assets.CaCert).
		WithClientCert(responderAlias, assets.ClientCert, assets.ClientKey).
		Run(ctx)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	got, err := rsp.stats(ctx, "after-argv")
	if err != nil {
		return err
	}
	if got.Peer != clientCn {
		return fmt.Errorf("expected the request to arrive under the client certificate %q, got %q", clientCn, got.Peer)
	}
	if got.Argv == "" {
		return fmt.Errorf("the fixture reported no argv, so this test proves nothing; check the pre-request script and the sandbox mode")
	}
	// The flag has to be there, or "the key is not on argv" would be satisfied by
	// a client certificate that was never configured at all.
	if !strings.Contains(got.Argv, "--client-cert-config") {
		return fmt.Errorf("expected the run to have been given a client certificate config, got argv: %s", got.Argv)
	}
	for _, unwanted := range []string{needle, "PRIVATE KEY"} {
		if strings.Contains(got.Argv, unwanted) {
			return fmt.Errorf("the key is on bru's command line: %s", got.Argv)
		}
		if strings.Contains(out, unwanted) {
			return fmt.Errorf("the key is in bru's output:\n%s", head(out))
		}
	}

	// The collection is the tree a caller lints and commits. The config, the
	// certificate and the key are this module's, and are mounted outside it.
	if want := "bruno.json,environments,health.bru"; got.Collection != want {
		return fmt.Errorf("expected the mounted collection to hold exactly %q, got %q", want, got.Collection)
	}
	return nil
}

// ClientCertPassphraseUnlocksTheKey checks WithClientCert's optional argument
// against a key that genuinely needs it.
//
// The passphrase is the one field of the rendered config that is not a path, and
// it is the reason the document travels as a secret rather than a file. It is
// also the easiest thing in the module to leave out and not notice: an
// unencrypted key ignores it, and every other test here uses one.
//
// So the same encrypted key is run twice. Without the passphrase the key cannot
// be loaded and the run fails; with it, the run authenticates to the mTLS
// endpoint and the responder reports the certificate back.
func (t *Tests) ClientCertPassphraseUnlocksTheKey(ctx context.Context) error {
	const clientCn = "bruno-passphrase-client"

	assets, err := newTlsAssets(ctx, "passphrase", responderAlias, clientCn)
	if err != nil {
		return err
	}
	rsp, err := newTlsResponder(ctx, assets, true)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	passphrase, err := randNamedSecret(ctx, "bruno-client-key-passphrase")
	if err != nil {
		return fmt.Errorf("mint the key passphrase: %w", err)
	}
	locked, err := encryptedKey(ctx, "passphrase", assets.ClientKey, passphrase)
	if err != nil {
		return err
	}

	collection := dag.Bruno().
		Collection(fixture("tls")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		WithCaCert(assets.CaCert)

	if _, err := collection.
		WithClientCert(responderAlias, assets.ClientCert, locked).
		Run(ctx); err == nil {
		return fmt.Errorf("expected an encrypted key with no passphrase to be an error")
	}
	got, err := rsp.stats(ctx, "after-locked-key")
	if err != nil {
		return err
	}
	if got.Count != 0 {
		return fmt.Errorf("expected a client that could not load its key to serve no request, got %d", got.Count)
	}

	if _, err := collection.
		WithClientCert(responderAlias, assets.ClientCert, locked,
			dagger.BrunoCollectionWithClientCertOpts{Passphrase: passphrase}).
		Run(ctx); err != nil {
		return fmt.Errorf("expected the passphrase to unlock the client key: %w", err)
	}
	got, err = rsp.stats(ctx, "after-unlocked-key")
	if err != nil {
		return err
	}
	if got.Count != 1 {
		return fmt.Errorf("expected the unlocked run to serve 1 request, got %d", got.Count)
	}
	if got.Peer != clientCn {
		return fmt.Errorf("expected the request to arrive under the client certificate %q, got %q", clientCn, got.Peer)
	}
	return nil
}

// CiReachesMtlsServiceBehindPrivateCa checks that the pipeline builder's TLS
// controls delegate, against the case they exist for: an internal endpoint
// behind a private CA that wants a client certificate, which is exactly the kind
// of target a repo hangs a CI check on.
//
// A pipeline that reaches it at all is a pipeline that delegated both. bru
// rejects a private-CA peer without the CA, and the responder rejects a client
// without the certificate — so the gate passing is the assertion, and the
// reported Common Name says it was the certificate rather than the handshake
// alone.
//
// Both terminals are exercised, since a report of a run that never connected is
// the failure this would otherwise hide, and the lint stage is enabled so the
// TLS material has to survive being validated ahead of the run.
func (t *Tests) CiReachesMtlsServiceBehindPrivateCa(ctx context.Context) error {
	const clientCn = "bruno-ci-client"

	assets, err := newTlsAssets(ctx, "ci", responderAlias, clientCn)
	if err != nil {
		return err
	}
	rsp, err := newTlsResponder(ctx, assets, true)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	ci := dag.Bruno().Ci(fixture("tls")).
		WithEnvironment("local").
		WithService(responderAlias, rsp.Svc).
		WithCaCert(assets.CaCert).
		WithoutTruststore().
		WithClientCert(responderAlias, assets.ClientCert, assets.ClientKey).
		WithLint().
		WithReport("junit")

	if err := ci.Check(ctx); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	got, err := rsp.stats(ctx, "after-ci-check")
	if err != nil {
		return err
	}
	if got.Count != 1 {
		return fmt.Errorf("expected the gate to issue 1 request, got %d", got.Count)
	}
	if got.Peer != clientCn {
		return fmt.Errorf("expected the request to arrive under the client certificate %q, got %q", clientCn, got.Peer)
	}

	reports, err := pin(ctx, ci.Run())
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	report, err := exportContents(ctx, reports.File("report.xml"), "ci-mtls-report.xml")
	if err != nil {
		return err
	}
	for _, want := range []string{"<testsuite", "health"} {
		if !strings.Contains(report, want) {
			return fmt.Errorf("expected the junit report to contain %q, got:\n%s", want, head(report))
		}
	}
	got, err = rsp.stats(ctx, "after-ci-run")
	if err != nil {
		return err
	}
	if got.Count != 2 {
		return fmt.Errorf("expected the artifact terminal to issue a 2nd request, got %d", got.Count)
	}
	if got.Peer != clientCn {
		return fmt.Errorf("expected the reported run to have authenticated as %q, got %q", clientCn, got.Peer)
	}
	return nil
}

// ------------------------------------------------------------------ helpers

// keyNeedle returns one base64 line out of a PEM key, as a fingerprint to search
// argv and output for. The whole document would never appear verbatim in a
// space-joined command line, so a search for it would pass no matter what
// happened.
func keyNeedle(ctx context.Context, key *dagger.Secret) (string, error) {
	pem, err := key.Plaintext(ctx)
	if err != nil {
		return "", fmt.Errorf("read the client key: %w", err)
	}
	for _, line := range strings.Split(pem, "\n") {
		if len(line) >= 40 && !strings.Contains(line, "-----") {
			return line, nil
		}
	}
	return "", fmt.Errorf("found no base64 line in the client key to fingerprint it by")
}

// fixture returns the named hand-authored Bruno collection under fixtures/.
func fixture(name string) *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/" + name)
}

// pin resolves a lazily-returned directory once and hands back a handle to that
// exact result.
//
// Ci.Run is +cache="never", so every selection off the directory it returns
// would otherwise re-invoke it — reading two reports out of one Run would be two
// runs of the collection, each reporting on its own set of responses. Resolving
// to an ID first pins the result.
func pin(ctx context.Context, dir *dagger.Directory) (*dagger.Directory, error) {
	id, err := dir.ID(ctx)
	if err != nil {
		return nil, err
	}
	return dag.LoadDirectoryFromID(dagger.DirectoryID(id)), nil
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

// generatedRequests lists the request files in a generated collection.
//
// Not every .bru file is a request: `bru import` writes the collection's own
// settings to collection.bru, one folder.bru per tag group, and the
// environments under environments/. Counting those as requests would let a
// collection with no requests at all satisfy "at least one .bru request".
func generatedRequests(ctx context.Context, collection *dagger.Directory) ([]string, error) {
	paths, err := collection.Glob(ctx, "**/*.bru")
	if err != nil {
		return nil, fmt.Errorf("glob the generated collection: %w", err)
	}
	var requests []string
	for _, p := range paths {
		if strings.HasPrefix(p, "environments/") {
			continue
		}
		switch path.Base(p) {
		case "collection.bru", "folder.bru":
			continue
		}
		requests = append(requests, p)
	}
	return requests, nil
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
