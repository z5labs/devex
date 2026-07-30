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
	"regexp"
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
	jobs = jobs.WithJob("SecretVarIsRedactedFromReportsByDefault", t.SecretVarIsRedactedFromReportsByDefault)
	jobs = jobs.WithJob("ReportWithoutAllHeadersOmitsEveryHeader", t.ReportWithoutAllHeadersOmitsEveryHeader)
	jobs = jobs.WithJob("ReportWithoutNamedHeaderKeepsTheOthers", t.ReportWithoutNamedHeaderKeepsTheOthers)
	jobs = jobs.WithJob("ReportWithoutBodiesOmitsBothBodies", t.ReportWithoutBodiesOmitsBothBodies)
	jobs = jobs.WithJob("WithoutHeadersRejectsBlankName", t.WithoutHeadersRejectsBlankName)
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
	jobs = jobs.WithJob("DriftIgnoresHandWrittenTests", t.DriftIgnoresHandWrittenTests)
	jobs = jobs.WithJob("CheckDriftFailsOnAnEndpointAddedToTheSpec", t.CheckDriftFailsOnAnEndpointAddedToTheSpec)
	jobs = jobs.WithJob("CheckDriftFailsOnBothSidesOfTheRequestSet", t.CheckDriftFailsOnBothSidesOfTheRequestSet)
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
	jobs = jobs.WithJob("CiRedactsSecretVarFromItsReports", t.CiRedactsSecretVarFromItsReports)

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

// SecretVarIsRedactedFromReportsByDefault checks the module's answer to the
// question the redaction story poses: what a report contains when the caller
// has said nothing about redaction and the collection was handed a secret.
//
// The first pass is the control, and it is what makes the second one mean
// something. bru masks header values by name — Authorization comes back as
// "Bearer ********" without anyone having asked — so a default asserted on its
// own could be passing on the strength of upstream's list rather than on
// anything this module did. The control pins how far that list reaches: the
// same secret in a header the list has never heard of, in the request body, and
// echoed back in the response body is written out verbatim.
//
// That is why the default is all headers and both bodies rather than something
// narrower. The module knows the value is sensitive; it does not know where the
// collection interpolated it, and a partial redaction would be a promise it
// cannot keep.
func (t *Tests) SecretVarIsRedactedFromReportsByDefault(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	value, err := dag.Random().Sha256(ctx)
	if err != nil {
		return fmt.Errorf("mint the test token: %w", err)
	}
	collection := dag.Bruno().
		Collection(fixture("redact")).
		WithEnvironment("local").
		WithSecretVar("API_TOKEN", dag.SetSecret("bruno-redact-default-token", value)).
		WithService(responderAlias, rsp.Svc)

	loud, err := exportContents(ctx,
		collection.WithUnredactedReport().Report("json"), "redact-unredacted.json")
	if err != nil {
		return fmt.Errorf("unredacted report: %w", err)
	}
	control, err := oneResult(loud)
	if err != nil {
		return fmt.Errorf("unredacted report: %w", err)
	}
	masked, ok := header(control.Request.Headers, "authorization")
	if !ok {
		return fmt.Errorf("expected an unredacted report to carry the Authorization header, got %v",
			control.Request.Headers)
	}
	if strings.Contains(masked, value) {
		return fmt.Errorf("expected bru to mask the Authorization header on its own, got %q", masked)
	}
	// Every place the secret survives upstream's masking. If one of these ever
	// stops holding, bru has widened its own redaction and this module's
	// default has less work to do than its documentation claims.
	custom, _ := header(control.Request.Headers, "x-custom")
	for _, leak := range []struct {
		where string
		got   string
	}{
		{"the X-Custom request header", custom},
		{"the request body", string(control.Request.Data)},
		{"the response body", string(control.Response.Data)},
	} {
		if !strings.Contains(leak.got, value) {
			return fmt.Errorf(
				"expected an unredacted report to carry the secret in %s, got %q", leak.where, leak.got)
		}
	}

	quiet, err := exportContents(ctx, collection.Report("json"), "redact-default.json")
	if err != nil {
		return fmt.Errorf("default report: %w", err)
	}
	if strings.Contains(quiet, value) {
		return fmt.Errorf("the secret is in the report the default produced:\n%s", head(quiet))
	}
	// Asserted against a report of a run that happened: a request bru never
	// issued carries no secret either, and would satisfy the line above.
	got, err := oneResult(quiet)
	if err != nil {
		return fmt.Errorf("default report: %w", err)
	}
	if len(got.Request.Headers) != 0 {
		return fmt.Errorf("expected no request headers under the default, got %v", got.Request.Headers)
	}
	if len(got.Response.Headers) != 0 {
		return fmt.Errorf("expected no response headers under the default, got %v", got.Response.Headers)
	}
	if got.Request.Data != nil || got.Response.Data != nil {
		return fmt.Errorf("expected neither body under the default, got request %q and response %q",
			got.Request.Data, got.Response.Data)
	}
	if got.Response.Status != 200 {
		return fmt.Errorf("expected the redacted report to describe a request that was served, got status %d",
			got.Response.Status)
	}
	return nil
}

// ReportWithoutAllHeadersOmitsEveryHeader checks the blunt control: nothing
// that was sent or came back as a header survives into the report, on either
// side of the exchange.
//
// Every redaction test here sets WithUnredactedReport first. The collection
// carries a secret — that is what makes it worth redacting — and a secret is
// enough to redact the report on its own, so without the opt-out these tests
// would pass whether or not the flag under test was rendered at all.
func (t *Tests) ReportWithoutAllHeadersOmitsEveryHeader(ctx context.Context) error {
	got, err := redactedReport(ctx, "all-headers", func(c *dagger.BrunoCollection) *dagger.BrunoCollection {
		return c.WithoutAllHeaders()
	})
	if err != nil {
		return err
	}
	if len(got.Request.Headers) != 0 {
		return fmt.Errorf("expected no request headers, got %v", got.Request.Headers)
	}
	if len(got.Response.Headers) != 0 {
		return fmt.Errorf("expected no response headers, got %v", got.Response.Headers)
	}
	// The bodies are the control: this flag is about headers, and one that
	// took the whole exchange with it would satisfy the two checks above.
	if got.Request.Data == nil || got.Response.Data == nil {
		return fmt.Errorf("expected both bodies to survive a header-only redaction, got request %q and response %q",
			got.Request.Data, got.Response.Data)
	}
	return nil
}

// ReportWithoutNamedHeaderKeepsTheOthers checks the narrow control, which is
// the one worth having: the credential goes and the report is still a report.
//
// The name is given in lower case against a header the collection spells
// Authorization, because bru matches case-insensitively and a caller should not
// have to know how the collection capitalised it.
func (t *Tests) ReportWithoutNamedHeaderKeepsTheOthers(ctx context.Context) error {
	got, err := redactedReport(ctx, "named-header", func(c *dagger.BrunoCollection) *dagger.BrunoCollection {
		return c.WithoutHeaders([]string{"authorization"})
	})
	if err != nil {
		return err
	}
	if value, ok := header(got.Request.Headers, "authorization"); ok {
		return fmt.Errorf("expected the Authorization header to be omitted, got %q", value)
	}
	for _, name := range []string{"X-Trace-Id", "X-Custom"} {
		if _, ok := header(got.Request.Headers, name); !ok {
			return fmt.Errorf("expected the %s header to survive, got %v", name, got.Request.Headers)
		}
	}
	if len(got.Response.Headers) == 0 {
		return fmt.Errorf("expected the response headers to survive, got none")
	}
	return nil
}

// ReportWithoutBodiesOmitsBothBodies checks the three body controls together,
// because what each one has to establish is which bodies it left alone.
//
// A flag that dropped both would satisfy the request-only case on its own, so
// the assertion in every pass is the pair: what went, and what stayed.
func (t *Tests) ReportWithoutBodiesOmitsBothBodies(ctx context.Context) error {
	for _, tc := range []struct {
		label   string
		apply   func(*dagger.BrunoCollection) *dagger.BrunoCollection
		request bool
		response bool
	}{
		{
			label:   "request-body",
			apply:   func(c *dagger.BrunoCollection) *dagger.BrunoCollection { return c.WithoutRequestBody() },
			request: false, response: true,
		},
		{
			label:   "response-body",
			apply:   func(c *dagger.BrunoCollection) *dagger.BrunoCollection { return c.WithoutResponseBody() },
			request: true, response: false,
		},
		{
			label:   "bodies",
			apply:   func(c *dagger.BrunoCollection) *dagger.BrunoCollection { return c.WithoutBodies() },
			request: false, response: false,
		},
	} {
		got, err := redactedReport(ctx, tc.label, tc.apply)
		if err != nil {
			return err
		}
		if (got.Request.Data != nil) != tc.request {
			return fmt.Errorf("%s: expected request body present=%t, got %q", tc.label, tc.request, got.Request.Data)
		}
		if (got.Response.Data != nil) != tc.response {
			return fmt.Errorf("%s: expected response body present=%t, got %q", tc.label, tc.response, got.Response.Data)
		}
		// Headers are the control here for the same reason the bodies were one
		// above: a flag that emptied the whole exchange would pass otherwise.
		if len(got.Request.Headers) == 0 {
			return fmt.Errorf("%s: expected the request headers to survive a body redaction, got none", tc.label)
		}
	}
	return nil
}

// WithoutHeadersRejectsBlankName checks that a name bru could not act on is
// refused. The builders have no error return, so the finding belongs to the
// run — and a blank name renders a flag that consumes whatever follows it,
// which is a redaction that silently applies to nothing.
func (t *Tests) WithoutHeadersRejectsBlankName(ctx context.Context) error {
	// Like every other deferred finding, it surfaces on the read: Report is
	// lazy, so its error travels with the file rather than with the call.
	_, err := dag.Bruno().
		Collection(fixture("redact")).
		WithEnvironment("local").
		WithoutHeaders([]string{"authorization", " "}).
		Report("json").
		Contents(ctx)
	if err == nil {
		return fmt.Errorf("expected a blank header name to be rejected")
	}
	if !strings.Contains(err.Error(), "WithoutHeaders: header name is required") {
		return fmt.Errorf("expected the error to name the builder, got: %v", err)
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

// DriftIgnoresHandWrittenTests checks the scoping the comparison exists for. A
// collection generated from the spec matches it — and still matches once one of
// its requests carries a test the document never described.
//
// The second half is the whole reason the comparison is not a tree diff: a
// generated request is where anyone would put the assertions that make the
// collection worth running, and a check that called that drift would be a check
// nobody could leave switched on.
func (t *Tests) DriftIgnoresHandWrittenTests(ctx context.Context) error {
	spec := fixtureFile("petstore.yaml")
	generated, err := pin(ctx, dag.Bruno().Generate(spec, dagger.BrunoGenerateOpts{Name: "petstore"}))
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	// The test is appended to a request the spec does describe, so the request
	// set is untouched and only the file's bytes move.
	const request = "pets/List every pet.bru"
	body, err := generated.File(request).Contents(ctx)
	if err != nil {
		return fmt.Errorf("read %q from the generated collection: %w", request, err)
	}
	edited := generated.WithNewFile(request, body+`
tests {
  test("responds", function() {
    expect(res.getStatus()).to.equal(200);
  });
}
`)

	for _, tc := range []struct {
		name       string
		collection *dagger.Directory
	}{
		{"as generated", generated},
		{"with a hand-written test", edited},
	} {
		collection := dag.Bruno().Collection(tc.collection)

		report, err := collection.Drift(ctx, spec)
		if err != nil {
			return fmt.Errorf("drift (%s): %w", tc.name, err)
		}
		if !strings.Contains(report, "matches") {
			return fmt.Errorf("expected drift (%s) to report a match, got:\n%s", tc.name, head(report))
		}
		// The count is the claim that the comparison read the requests at all:
		// a report of zero requests would "match" any spec.
		if !strings.Contains(report, fmt.Sprintf("%d requests", specOperations)) {
			return fmt.Errorf("expected drift (%s) to account for %d requests, got:\n%s",
				tc.name, specOperations, head(report))
		}
		if err := collection.CheckDrift(ctx, spec); err != nil {
			return fmt.Errorf("expected the collection (%s) to gate clean against its own spec: %w", tc.name, err)
		}
	}
	return nil
}

// CheckDriftFailsOnAnEndpointAddedToTheSpec checks the case the whole story
// exists for: the document grew an operation and nobody added a request for it,
// so the endpoint is untested and nothing says so.
//
// The failure has to name the missing request rather than only report that
// something differs — an endpoint nobody noticed is not made findable by being
// told a count changed.
func (t *Tests) CheckDriftFailsOnAnEndpointAddedToTheSpec(ctx context.Context) error {
	collection := dag.Bruno().Collection(
		dag.Bruno().Generate(fixtureFile("petstore.yaml"), dagger.BrunoGenerateOpts{Name: "petstore"}))
	extended := fixtureFile("petstore-extended.yaml")

	err := collection.CheckDrift(ctx, extended)
	if err == nil {
		return fmt.Errorf("expected an endpoint added to the spec to fail the drift check")
	}
	// The route is spelled with Bruno's `:petId`, not the document's {petId}:
	// the two are the same segment, and the report is read beside the
	// collection.
	if !hasReportLine(err.Error(), "+GET /pets/:petId/toys") {
		return fmt.Errorf("expected the error to name the missing request, got:\n%s", head(err.Error()))
	}
	// The requests that did not change must not be reported as differences, or
	// the report buries the one endpoint that needs adding.
	for _, unchanged := range []string{"+GET /health", "+GET /pets", "+POST /pets", "+GET /pets/:petId", "-GET /pets"} {
		if hasReportLine(err.Error(), unchanged) {
			return fmt.Errorf("expected only the added endpoint to be reported, but the report carries %q:\n%s",
				unchanged, head(err.Error()))
		}
	}

	// Drift describes the same difference without failing on it, so the report
	// is reachable from a step that is not the gate.
	report, err := collection.Drift(ctx, extended)
	if err != nil {
		return fmt.Errorf("drift: %w", err)
	}
	if !strings.Contains(report, "+GET /pets/:petId/toys") {
		return fmt.Errorf("expected drift to report the missing request without failing, got:\n%s", head(report))
	}
	return nil
}

// CheckDriftFailsOnBothSidesOfTheRequestSet checks the other direction, and
// that the two are distinguished.
//
// A request deleted from the collection leaves an endpoint the document declares
// with nothing testing it. A request the document has no operation for is the
// opposite problem — an endpoint that was renamed or retired upstream, still
// being exercised — and it has to read as its own thing rather than as the same
// finding, because the fix is the opposite one.
func (t *Tests) CheckDriftFailsOnBothSidesOfTheRequestSet(ctx context.Context) error {
	spec := fixtureFile("petstore.yaml")
	generated, err := pin(ctx, dag.Bruno().Generate(spec, dagger.BrunoGenerateOpts{Name: "petstore"}))
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	edited := generated.
		WithoutFile("pets/Fetch one pet.bru").
		WithNewFile("pets/Retire a pet.bru", `meta {
  name: Retire a pet
  type: http
  seq: 4
}

delete {
  url: {{baseUrl}}/pets/:petId/retire
  body: none
  auth: inherit
}
`)

	err = dag.Bruno().Collection(edited).CheckDrift(ctx, spec)
	if err == nil {
		return fmt.Errorf("expected a collection missing one request and carrying an undeclared one to fail the drift check")
	}
	for _, want := range []string{
		// The document declares it; the collection no longer has it.
		"+GET /pets/:petId",
		// The collection has it; the document declares no such operation.
		"-DELETE /pets/:petId/retire",
	} {
		if !hasReportLine(err.Error(), want) {
			return fmt.Errorf("expected the error to carry %q, got:\n%s", want, head(err.Error()))
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

// CiRedactsSecretVarFromItsReports checks the redaction where it matters most.
// Run is the terminal that writes the file a CI system archives, and an
// archived report outlives the run that produced it.
//
// Both directions are exercised, because a builder that dropped the delegation
// entirely would look identical to one that redacts: a report with no headers
// is what the default produces and also what a pipeline that never passed the
// controls along would produce if the collection redacted on its own account.
// The unredacted pass is what tells the two apart — the secret has to come back
// when the pipeline asks for it.
func (t *Tests) CiRedactsSecretVarFromItsReports(ctx context.Context) error {
	rsp, err := newResponder(ctx)
	if err != nil {
		return err
	}
	defer rsp.stop(ctx)

	value, err := dag.Random().Sha256(ctx)
	if err != nil {
		return fmt.Errorf("mint the test token: %w", err)
	}
	ci := dag.Bruno().Ci(fixture("redact")).
		WithEnvironment("local").
		WithSecretVar("API_TOKEN", dag.SetSecret("bruno-ci-redact-token", value)).
		WithService(responderAlias, rsp.Svc).
		WithReport("json")

	for _, tc := range []struct {
		label     string
		pipeline  *dagger.BrunoCi
		wantValue bool
	}{
		{"default", ci, false},
		{"unredacted", ci.WithUnredactedReport(), true},
	} {
		// Ci.Run is +cache="never", so the directory is resolved once before
		// anything is selected out of it. Two selections would be two runs.
		reports, err := pin(ctx, tc.pipeline.Run())
		if err != nil {
			return fmt.Errorf("%s run: %w", tc.label, err)
		}
		contents, err := exportContents(ctx,
			reports.File("report.json"), "ci-redact-"+tc.label+".json")
		if err != nil {
			return fmt.Errorf("%s report: %w", tc.label, err)
		}
		if strings.Contains(contents, value) != tc.wantValue {
			return fmt.Errorf("%s: expected the secret in the report=%t:\n%s",
				tc.label, tc.wantValue, head(contents))
		}
		got, err := oneResult(contents)
		if err != nil {
			return fmt.Errorf("%s report: %w", tc.label, err)
		}
		if got.Response.Status != 200 {
			return fmt.Errorf("%s: expected a report of a request that was served, got status %d",
				tc.label, got.Response.Status)
		}
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

// redactedReport runs the redact fixture under one explicit redaction control
// and hands back the single exchange its JSON report describes.
//
// The collection is handed a secret, because a report worth redacting is one of
// a run that carried something — and then WithUnredactedReport turns the
// default off, so that what the report holds is the doing of the control under
// test and not of the secret.
func redactedReport(
	ctx context.Context,
	label string,
	apply func(*dagger.BrunoCollection) *dagger.BrunoCollection,
) (reportResult, error) {
	rsp, err := newResponder(ctx)
	if err != nil {
		return reportResult{}, err
	}
	defer rsp.stop(ctx)

	value, err := dag.Random().Sha256(ctx)
	if err != nil {
		return reportResult{}, fmt.Errorf("mint the test token: %w", err)
	}
	collection := dag.Bruno().
		Collection(fixture("redact")).
		WithEnvironment("local").
		WithSecretVar("API_TOKEN", dag.SetSecret("bruno-redact-"+label+"-token", value)).
		WithService(responderAlias, rsp.Svc).
		WithUnredactedReport()

	contents, err := exportContents(ctx, apply(collection).Report("json"), "redact-"+label+".json")
	if err != nil {
		return reportResult{}, fmt.Errorf("%s report: %w", label, err)
	}
	got, err := oneResult(contents)
	if err != nil {
		return reportResult{}, fmt.Errorf("%s report: %w", label, err)
	}
	return got, nil
}

// header looks a reported header up case-insensitively. bru writes each name
// back the way the collection spelled it, so an exact-key lookup would be
// asserting the fixture's capitalisation rather than what was reported.
func header(headers map[string]string, name string) (string, bool) {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}

// reportResult is the part of one entry in bru's JSON report that the
// redaction tests read: what the request carried and what came back.
type reportResult struct {
	Request  reportMessage `json:"request"`
	Response reportMessage `json:"response"`
}

// reportMessage is one side of a reported exchange.
//
// Data is held raw because the two sides do not agree on its type — a request
// body is reported as the string that was sent, a response body as the parsed
// document — and because what the body flags do is remove the field rather than
// blank it, so nil is the assertion those tests make.
type reportMessage struct {
	Headers map[string]string `json:"headers"`
	Data    json.RawMessage   `json:"data"`
	Status  int               `json:"status"`
}

// oneResult reads the single reported exchange out of a JSON report.
//
// The document is an array of iterations, each holding its own results, because
// bru reports a data-driven run as one iteration per row. Every collection here
// makes one request, so anything else means the run was not the one the test
// set up.
func oneResult(contents string) (reportResult, error) {
	var iterations []struct {
		Results []reportResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(contents), &iterations); err != nil {
		return reportResult{}, fmt.Errorf("parse the json report: %w\n%s", err, head(contents))
	}
	var results []reportResult
	for _, iteration := range iterations {
		results = append(results, iteration.Results...)
	}
	if len(results) != 1 {
		return reportResult{}, fmt.Errorf("expected 1 reported request, got %d:\n%s", len(results), head(contents))
	}
	return results[0], nil
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

// hasReportLine reports whether a drift report carries a line exactly.
//
// The comparison is per line rather than a substring match because every line of
// the patch is prefixed with a space, a + or a -, and one route is a prefix of
// another: "+GET /pets/:petId" would otherwise be satisfied by
// "+GET /pets/:petId/toys", which is the difference between the check naming the
// endpoint that drifted and naming a longer one.
//
// The traceparent comes off first. Dagger appends one to the end of a module
// error's message, so the report's last line — which for a patch is a route —
// arrives carrying a span id that is nothing to do with what drifted.
func hasReportLine(report string, line string) bool {
	report = traceparentSuffix.ReplaceAllString(report, "")
	return slices.Contains(strings.Split(report, "\n"), line)
}

// traceparentSuffix matches the span id Dagger appends to a module error's
// message.
var traceparentSuffix = regexp.MustCompile(`\s*\[traceparent:[0-9a-f-]+\]\s*$`)

// head trims a long output down to something readable in a failure message.
func head(s string) string {
	const limit = 2048
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n..."
}
