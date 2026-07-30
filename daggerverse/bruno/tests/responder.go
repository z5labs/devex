package main

import (
	"context"
	"encoding/json"
	"fmt"

	"dagger/tests/internal/dagger"
)

const (
	// responderAlias is the hostname the fixtures' baseUrl resolves to. A
	// binding alias is scoped to the container that declares it, not to the
	// session, so every test can bind its own responder under the same name
	// and still run in parallel — unlike a WithHostname, which is global.
	responderAlias = "api"

	// responderPort is what the fixtures' baseUrl points at.
	responderPort = 8080

	// statsPath is the responder's own endpoint, excluded from the request
	// count so that asking what happened is not itself something that
	// happened.
	statsPath = "/_stats"
)

// responderScript builds a request-recording HTTP service. Every path but
// statsPath answers 200 with a small JSON body, bumps a counter and remembers
// what the request carried; statsPath hands that record back.
//
// That body echoes the request's X-Custom header back, which is what puts a
// caller's own value in a *response* body. The redaction tests need one there:
// a login route answering with the token it was given is the case
// WithoutResponseBody exists for, and a responder that only ever said "ok"
// could not stand in for it.
//
// It runs on the Bruno CLI image itself rather than a second image: that image
// is node:22-alpine, so `node -e` needs nothing pulled that the module under
// test has not already pulled.
//
// id is baked into the script rather than derived at boot because Dagger
// content-addresses services: two responders assembled from identical args are
// the same service, and a suite that counts requests would then be counting
// somebody else's — including a previous session's, which is how a run that
// really did happen twice reads as one. A per-test id makes each instance
// private.
func responderScript(id string) string {
	return fmt.Sprintf(`
const http = require('http');
const boot = %q;
let count = 0;
let last = { path: '', token: '', argv: '' };
http.createServer((req, res) => {
  if (req.url === %q) {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify(Object.assign({ count, boot }, last)));
    return;
  }
  count++;
  last = {
    path: req.url,
    token: req.headers['x-token'] || '',
    argv: req.headers['x-argv'] || '',
  };
  res.writeHead(200, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ status: 'ok', count, echo: req.headers['x-custom'] || '' }));
}).listen(%d, '0.0.0.0');
`, id, statsPath, responderPort)
}

// stats is what the responder recorded: how many requests it served, and the
// path, X-Token header and X-Argv header of the most recent one.
//
// Peer and Collection are only ever set by the TLS responder: the Common Name of
// the client certificate the request arrived under, and the X-Collection header a
// fixture reports the mounted collection's contents in.
type stats struct {
	Count      int    `json:"count"`
	Path       string `json:"path"`
	Token      string `json:"token"`
	Argv       string `json:"argv"`
	Peer       string `json:"peer"`
	Collection string `json:"collection"`
	// Boot identifies the service instance that answered. Two reads that
	// disagree about it were talking to two different responders, which is
	// what a request count that refuses to add up usually means.
	Boot string `json:"boot"`
}

// responder is a started request-recording service plus the handle the tests
// query it through.
type responder struct {
	Svc *dagger.Service
	// ID is what makes this instance private; see responderScript.
	ID string
}

// newResponder starts a private recording service.
//
// It is started here rather than left to the first WithServiceBinding because
// the record has to survive across calls: a binding holds the service only for
// the exec that declares it, and a restart between two Runs would reset the
// counter the caching tests read.
func newResponder(ctx context.Context) (*responder, error) {
	id, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("name the responder instance: %w", err)
	}
	svc := dag.Bruno().Container().
		WithExposedPort(responderPort).
		// A server never exits, so it belongs in AsService's args rather than
		// a WithExec, which would wait for it forever.
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{"node", "-e", responderScript(id)},
		})
	if _, err := svc.Start(ctx); err != nil {
		return nil, fmt.Errorf("start the recording responder: %w", err)
	}
	return &responder{Svc: svc, ID: id}, nil
}

// stats reads what the responder has recorded so far. label distinguishes one
// read from another: two identical execs share a cache entry, so a second read
// with the same inputs would hand back the first read's counter no matter what
// happened at the service in between.
func (rsp *responder) stats(ctx context.Context, label string) (stats, error) {
	out, err := dag.Bruno().Container().
		WithServiceBinding(responderAlias, rsp.Svc).
		WithEnvVariable("STATS_READ", rsp.ID+"-"+label).
		WithExec([]string{"node", "-e", fmt.Sprintf(
			`fetch('http://%s:%d%s').then(r => r.text()).then(t => process.stdout.write(t))`,
			responderAlias, responderPort, statsPath)}).
		Stdout(ctx)
	if err != nil {
		return stats{}, fmt.Errorf("read responder stats: %w", err)
	}
	var s stats
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return stats{}, fmt.Errorf("parse responder stats %q: %w", out, err)
	}
	if s.Boot != rsp.ID {
		return stats{}, fmt.Errorf("read stats from responder %q, expected %q", s.Boot, rsp.ID)
	}
	return s, nil
}

// stop tears the responder down. Under `all` several are alive at once, and
// each is useless the moment its test returns.
func (rsp *responder) stop(ctx context.Context) {
	_, _ = rsp.Svc.Stop(ctx)
}
