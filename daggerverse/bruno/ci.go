package main

import (
	"context"
	"fmt"

	"dagger/bruno/internal/dagger"
)

// reportFileName is the base name a report carries inside the directory Run
// returns. The extension comes from the format, so json, junit and html land
// beside one another as report.json, report.xml and report.html rather than
// colliding.
const reportFileName = "report"

// Ci is a chained builder for a standardized Bruno CI pipeline: a collection
// in, a gate and the reports out.
//
// Lint, run and report are three calls plus the glue that decides what fails
// the build, which is the shape every API repo ends up hand-rolling. This
// bundles them so that CI is one declarative `dagger call`.
//
// It composes Collection without adding capability of its own — every stage is
// a call the caller could make by hand. What it adds is the ordering: lint runs
// before the collection, so a {{baseUrl}} that resolves nowhere or a
// credential committed in plaintext is reported without spending a request on
// discovering it, and a collection that could never have passed does not start
// a container's worth of work to say so.
//
// The two terminals split the way Collection's own Run and Report do, and for
// the same reason. Check is the gate: it fails on a lint error, on a failing
// request, test or assertion, and on any of bru's usage errors. Run is the
// artifact: it returns the reports directory, and does not fail a run whose
// requests failed — Dagger drops a function's value when it also returns an
// error, so a gating Run could never hand back the report describing the
// failure, which is exactly when the report matters. CI pairs them.
type Ci struct {
	// +private
	Collection *Collection
	// +private
	LintEnabled bool
	// +private
	LintFailOnWarnings bool
	// +private
	Formats []string
}

// Ci returns a new pipeline builder bound to the supplied collection source.
// The input is the collection root, exactly as a bare Collection(source) call
// would take it.
func (b *Bruno) Ci(source *dagger.Directory) *Ci {
	return &Ci{Collection: b.Collection(source)}
}

// WithEnvironment selects the environment the pipeline resolves variables from,
// by the name of the file under environments/ without its extension. See
// Collection.WithEnvironment.
//
// It is also what the lint stage checks references against, so the environment
// a pipeline runs under is the one its variables are required to resolve in.
func (c *Ci) WithEnvironment(name string) *Ci {
	out := *c
	out.Collection = c.Collection.WithEnvironment(name)
	return &out
}

// WithService binds a service into the pipeline's network under alias, so the
// collection can reach it by that hostname. A collection is inert without a
// target. See Collection.WithService.
func (c *Ci) WithService(alias string, service *dagger.Service) *Ci {
	out := *c
	out.Collection = c.Collection.WithService(alias, service)
	return &out
}

// WithSecretVar makes a secret readable from the collection as
// {{process.env.NAME}}, without it ever reaching argv. See
// Collection.WithSecretVar.
//
// A pipeline takes secrets and not plain overrides on purpose: a value worth
// passing into CI by hand is usually a credential, and WithVar would put it on
// the command line. A collection that needs a non-secret override can still be
// assembled through Collection.
func (c *Ci) WithSecretVar(name string, value *dagger.Secret) *Ci {
	out := *c
	out.Collection = c.Collection.WithSecretVar(name, value)
	return &out
}

// WithCaCert verifies peers against a custom CA certificate, for a pipeline
// whose target presents a certificate signed by a private CA. See
// Collection.WithCaCert.
//
// This is the case the pipeline builder exists for as much as any: an internal
// endpoint is exactly the kind a repo hangs a CI check on, and the alternative
// is `--insecure`, which verifies nothing.
func (c *Ci) WithCaCert(cert *dagger.File) *Ci {
	out := *c
	out.Collection = c.Collection.WithCaCert(cert)
	return &out
}

// WithoutTruststore verifies peers against the WithCaCert certificate alone,
// ignoring the CAs the image ships. It means nothing without WithCaCert, and on
// its own is rejected by the terminal. See Collection.WithoutTruststore.
func (c *Ci) WithoutTruststore() *Ci {
	out := *c
	out.Collection = c.Collection.WithoutTruststore()
	return &out
}

// WithClientCert presents a client certificate to hosts matching host, for a
// pipeline that has to authenticate to an mTLS endpoint. Call it more than once
// for more than one host. See Collection.WithClientCert.
func (c *Ci) WithClientCert(
	// Hostname pattern the certificate applies to, matched against the request
	// URL: "api.internal" for one host, "*.internal" for a wildcard. bru uses
	// the first configured host that matches.
	host string,
	// PEM certificate to present.
	cert *dagger.File,
	// PEM private key for the certificate.
	key *dagger.Secret,
	// Passphrase the private key is encrypted with, if it is.
	// +optional
	passphrase *dagger.Secret,
) *Ci {
	out := *c
	out.Collection = c.Collection.WithClientCert(host, cert, key, passphrase)
	return &out
}

// WithLint adds the lint stage, which runs before the collection and fails the
// pipeline on a structural error without issuing a request. See
// Collection.Lint for the rules.
//
// It is opt-in rather than always-on because linting is an opinion about how a
// collection is written, and a pipeline should not start failing on one the day
// it adopts the builder.
func (c *Ci) WithLint(
	// Treat lint warnings as failures.
	// +default=false
	failOnWarnings bool,
) *Ci {
	out := *c
	out.LintEnabled = true
	out.LintFailOnWarnings = failOnWarnings
	return &out
}

// WithReport adds a reporter format — json, junit or html — to the set Run
// returns. Call it more than once for more than one format.
//
// Every requested format comes out of a single collection pass: bru accepts all
// of its `--reporter-*` flags at once, so asking for both JUnit and HTML costs
// one run and the two artifacts describe the same set of responses rather than
// two different ones.
//
// Like the rest of the builder it has no error return, so an unknown format is
// reported by the terminal that would have written it.
func (c *Ci) WithReport(
	// Reporter format: json, junit or html.
	format string,
) *Ci {
	out := *c
	out.Formats = append(append([]string(nil), c.Formats...), format)
	return &out
}

// WithoutHeaders omits the named headers from the reports Run returns. See
// Collection.WithoutHeaders.
func (c *Ci) WithoutHeaders(
	// Header names to omit, matched case-insensitively.
	names []string,
) *Ci {
	out := *c
	out.Collection = c.Collection.WithoutHeaders(names)
	return &out
}

// WithoutAllHeaders omits every header from the reports Run returns. See
// Collection.WithoutAllHeaders.
func (c *Ci) WithoutAllHeaders() *Ci {
	out := *c
	out.Collection = c.Collection.WithoutAllHeaders()
	return &out
}

// WithoutRequestBody omits every request body from the reports Run returns. See
// Collection.WithoutRequestBody.
func (c *Ci) WithoutRequestBody() *Ci {
	out := *c
	out.Collection = c.Collection.WithoutRequestBody()
	return &out
}

// WithoutResponseBody omits every response body from the reports Run returns.
// See Collection.WithoutResponseBody.
func (c *Ci) WithoutResponseBody() *Ci {
	out := *c
	out.Collection = c.Collection.WithoutResponseBody()
	return &out
}

// WithoutBodies omits both request and response bodies from the reports Run
// returns. See Collection.WithoutBodies.
func (c *Ci) WithoutBodies() *Ci {
	out := *c
	out.Collection = c.Collection.WithoutBodies()
	return &out
}

// WithUnredactedReport reports every header and both bodies, cancelling the
// redaction a WithSecretVar secret otherwise applies. See
// Collection.WithUnredactedReport.
//
// This is the builder where the default matters most and where cancelling it
// deserves the most thought: what Run produces is the artifact a CI system
// archives, and an archived report outlives the run that wrote it.
func (c *Ci) WithUnredactedReport() *Ci {
	out := *c
	out.Collection = c.Collection.WithUnredactedReport()
	return &out
}

// Check runs the pipeline as a gate and produces nothing, for the PR that wants
// to know whether the API is behaving.
//
// The stages are the enabled lint followed by the collection itself. Lint comes
// first so that a structural error costs no request: a collection whose
// variables resolve nowhere fails here rather than against a live service,
// naming the file instead of the response. The collection then fails on bru's
// exit 1 — a failing request, test or assertion — and reports every other
// non-zero exit as the usage error it is.
//
// No report is produced, because a gate that returns nothing can gate: see Run
// for why the terminal that hands back artifacts cannot also be the one that
// fails.
//
// +cache="never"
func (c *Ci) Check(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := c.lint(ctx); err != nil {
		return err
	}
	_, err := c.Collection.Run(ctx, true)
	return err
}

// Run executes the pipeline and returns the requested reports as a directory,
// one file per format: report.json, report.xml for junit, report.html.
//
// It does not fail on a failing request, test or assertion. That is deliberate
// and it is the same reasoning as Collection.Report: a Dagger function that
// returns an error forfeits its value, so a Run that gated would hand back
// nothing on exactly the runs whose reports a pipeline needs — the JUnit file a
// CI system turns into a test report, the HTML page somebody opens to see which
// assertion failed. Pair the two: Run for the artifacts, Check for the gate.
//
// A lint error is still an error here, because then the collection never ran
// and there is no report to return. So is a usage error, for the same reason.
//
// +cache="never"
func (c *Ci) Run(ctx context.Context) (*dagger.Directory, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	// A pipeline with no reporter has nothing for this terminal to return, and
	// running the collection to hand back an empty directory would look like a
	// pass. The caller wanted Check.
	if len(c.Formats) == 0 {
		return nil, fmt.Errorf(
			"Ci: run has no reports to return: add one with with-report (%s, %s or %s), or use check for a pipeline that only gates",
			formatJSON, formatJUnit, formatHTML)
	}
	if err := c.lint(ctx); err != nil {
		return nil, err
	}
	exec, err := c.Collection.exec(ctx, true, c.Formats)
	if err != nil {
		return nil, err
	}
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return nil, err
	}
	if code != 0 && code != exitCollectionFailed {
		return nil, usageError(code, combinedOutput(ctx, exec))
	}
	out := dag.Directory()
	for _, format := range c.Formats {
		path, err := reportPath(format)
		if err != nil {
			return nil, err
		}
		ext, err := reportExtension(format)
		if err != nil {
			return nil, err
		}
		out = out.WithFile(reportFileName+"."+ext, exec.File(path))
	}
	return out, nil
}

// validate reports the deferred builder checks, which live here because a
// builder has no error return. The report formats are checked by both terminals
// rather than only by Run: a pipeline whose declared artifact cannot be written
// is misconfigured whichever terminal discovers it, and finding out from Check
// costs nothing.
func (c *Ci) validate() error {
	for _, format := range c.Formats {
		if _, err := reportExtension(format); err != nil {
			return fmt.Errorf("WithReport: %w", err)
		}
	}
	return nil
}

// lint runs the enabled lint stage. Nothing here is a container: every rule is
// evaluated in pure Go over the source tree, which is what makes running it
// ahead of the collection free.
func (c *Ci) lint(ctx context.Context) error {
	if !c.LintEnabled {
		return nil
	}
	return c.Collection.Lint(ctx, c.LintFailOnWarnings)
}
