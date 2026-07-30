package main

import (
	"fmt"
	"strings"
)

// This file is the reporter's redaction controls: which of a run's headers and
// bodies reach the artifact CI archives.
//
// The problem is that a report is written to be read later, by whatever
// collects build artifacts, and a collection that authenticates puts its
// credential in a request. bru's JSON reporter records each exchange in full —
// request headers, request body, response headers, response body — so the
// artifact a pipeline publishes carries the credential the pipeline was given.
//
// bru masks some of that already, by header name: a value under Authorization
// is written out as "Bearer ********" with nobody having asked. The list is
// short and it is a list of names, so the same secret under a header the list
// has never heard of, in the request body, or echoed back in the response body
// is written out verbatim. That is what these controls are for, and it is why
// the default below is as broad as it is.
//
// Only the JSON reporter carries any of this. As of 3.4.2 the JUnit document
// holds test names, assertion expressions and failure messages, and the HTML
// page holds the same — neither reports a header or a body at all, whether the
// run passed or failed. So these flags change the JSON artifact and leave the
// other two as they were.
//
// What none of them reach is the request URL, which every reporter records in
// full. A secret interpolated into a path or a query string is in the report
// whatever is set here, and bru has no flag that would remove it. Put
// credentials in headers.

// WithoutHeaders omits the named headers from the reporter output
// (`--reporter-skip-headers`), on both sides of each exchange.
//
// Matching is case-insensitive and the header is dropped rather than blanked,
// so "authorization" removes an Authorization that was sent and one that came
// back. Call it once with every name, or more than once — the names accumulate.
//
// Use it to keep a report that is still worth reading: everything the run
// carried survives except the headers named here. WithoutAllHeaders is the
// blunter instrument for when the set of sensitive names is not known.
func (c *Collection) WithoutHeaders(
	// Header names to omit, matched case-insensitively.
	names []string,
) *Collection {
	out := c.clone()
	out.SkipHeaders = append(out.SkipHeaders, names...)
	return out
}

// WithoutAllHeaders omits every header from the reporter output
// (`--reporter-skip-all-headers`), on both sides of each exchange.
//
// This is the choice to make when the collection's headers are not enumerable
// from where the pipeline is written — a header set by a script, or an auth
// scheme that adds one. WithoutHeaders keeps the rest of them.
func (c *Collection) WithoutAllHeaders() *Collection {
	out := c.clone()
	out.SkipAllHeaders = true
	return out
}

// WithoutRequestBody omits every request body from the reporter output
// (`--reporter-skip-request-body`), for the collection that posts credentials
// rather than sending them in a header.
func (c *Collection) WithoutRequestBody() *Collection {
	out := c.clone()
	out.SkipRequestBody = true
	return out
}

// WithoutResponseBody omits every response body from the reporter output
// (`--reporter-skip-response-body`), for the endpoint that answers with a token
// — a login route being the obvious one, and the one whose report is most
// worth reading and least safe to keep.
func (c *Collection) WithoutResponseBody() *Collection {
	out := c.clone()
	out.SkipResponseBody = true
	return out
}

// WithoutBodies omits both request and response bodies from the reporter output
// (`--reporter-skip-body`).
func (c *Collection) WithoutBodies() *Collection {
	out := c.clone()
	out.SkipBodies = true
	return out
}

// WithUnredactedReport reports every header and both bodies, cancelling the
// redaction that a WithSecretVar secret otherwise applies.
//
// A collection that was handed a secret redacts its reports by default: see
// redactArgs for why that is the default rather than the option. This is the
// way back for a pipeline that wants the whole exchange in its artifact and has
// decided the artifact is somewhere that can hold it.
//
// It does not undo the five Without* controls above; it only cancels what the
// secret added. So a pipeline that wants the bodies in its artifact and none of
// the headers sets this alongside WithoutAllHeaders, and gets exactly that
// rather than the default's both.
func (c *Collection) WithUnredactedReport() *Collection {
	out := c.clone()
	out.Unredacted = true
	return out
}

// redactArgs renders the reporter's redaction flags, including the ones nobody
// asked for.
//
// A collection holding a WithSecretVar secret redacts all headers and both
// bodies unless WithUnredactedReport says otherwise. The reasoning is that the
// module has been told, explicitly and by the caller, that a particular value
// is sensitive — and knows nothing at all about where the collection
// interpolates it. It could be a header, it could be a field in the request
// body, and if the endpoint is a login route it comes back in the response
// body. Redacting the one place it is most often found would be a promise this
// module cannot keep, and a report that looks redacted is worse than one that
// obviously is not.
//
// So the safe choice is the one the caller gets without asking for it, which is
// the point: the caller who would have thought to ask was never the one at
// risk. The cost is a narrower artifact for a pipeline that passes a secret,
// and WithUnredactedReport buys it back.
//
// TLS material is not a trigger. WithClientCert's key and passphrase are
// secrets too, but they are consumed by the handshake and never reach a header
// or a body, so a collection that only authenticates with a certificate reports
// in full.
func (c *Collection) redactArgs() []string {
	skipAllHeaders, skipBodies := c.SkipAllHeaders, c.SkipBodies
	if len(c.SecretVarNames) > 0 && !c.Unredacted {
		skipAllHeaders, skipBodies = true, true
	}

	var args []string
	if skipAllHeaders {
		args = append(args, "--reporter-skip-all-headers")
	}
	for _, name := range c.SkipHeaders {
		// The flag takes an array, and yargs accumulates it across repeats.
		// Repeating is what this renders rather than passing the names as one
		// space-separated run, which would go on swallowing whatever followed
		// it on the command line.
		args = append(args, "--reporter-skip-headers", name)
	}
	if skipBodies {
		args = append(args, "--reporter-skip-body")
	}
	if c.SkipRequestBody {
		args = append(args, "--reporter-skip-request-body")
	}
	if c.SkipResponseBody {
		args = append(args, "--reporter-skip-response-body")
	}
	return args
}

// validateRedaction reports the deferred checks on the redaction builders.
func (c *Collection) validateRedaction() error {
	for _, name := range c.SkipHeaders {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("WithoutHeaders: header name is required")
		}
	}
	return nil
}
