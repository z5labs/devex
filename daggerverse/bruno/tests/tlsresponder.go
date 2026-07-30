package main

import (
	"context"
	"fmt"

	"dagger/tests/internal/dagger"
)

const (
	// tlsResponderPort is what the TLS fixtures' baseUrl points at. It is a
	// different port from the plaintext responder's so that the record can be
	// read back over plain HTTP: a stats endpoint behind mTLS would need the
	// reader to present a client certificate to ask what happened.
	tlsResponderPort = 8443

	// serverCertPath, serverKeyPath and responderCaPath are where the
	// responder's own material is staged. The key is a secret mount owned by
	// the image's non-root user, because a secret mount is root-owned 0400 by
	// default and the process runs as UID 1000.
	serverCertPath  = "/certs/server.pem"
	serverKeyPath   = "/certs/server.key"
	responderCaPath = "/certs/ca.pem"

	// responderUser is the non-root user the Bruno CLI image runs as, and so the
	// one the responder's own material has to be readable by.
	responderUser = "node"
)

// tlsResponderScript builds the same request-recording service as
// responderScript, over HTTPS.
//
// The record is served by a second, plaintext listener on responderPort. That is
// not a shortcut: under mTLS the recording listener rejects any client that does
// not present a certificate, so a stats read over it would have to be given the
// very credential the test is trying to prove reached the service.
//
// requireClientCert turns the recording listener into an mTLS one. It records
// the peer certificate's Common Name, which is what makes the mTLS assertion
// about the certificate rather than about the handshake: a server that merely
// completed a TLS handshake proves nothing about who it was talking to.
//
// As with the plaintext responder, id is baked in because Dagger
// content-addresses services and a suite that counts requests would otherwise be
// counting another instance's.
func tlsResponderScript(id string, requireClientCert bool) string {
	mtls := ""
	if requireClientCert {
		mtls = fmt.Sprintf(`
options.ca = fs.readFileSync(%q);
options.requestCert = true;
options.rejectUnauthorized = true;
`, responderCaPath)
	}
	return fmt.Sprintf(`
const http = require('http');
const https = require('https');
const fs = require('fs');
const boot = %q;
let count = 0;
let last = { path: '', token: '', argv: '', collection: '', peer: '' };

http.createServer((req, res) => {
  if (req.url !== %q) {
    res.writeHead(404);
    res.end();
    return;
  }
  res.writeHead(200, { 'content-type': 'application/json' });
  res.end(JSON.stringify(Object.assign({ count, boot }, last)));
}).listen(%d, '0.0.0.0');

const options = {
  cert: fs.readFileSync(%q),
  key: fs.readFileSync(%q),
};
%s
https.createServer(options, (req, res) => {
  count++;
  const peer = req.socket.getPeerCertificate ? req.socket.getPeerCertificate() : null;
  last = {
    path: req.url,
    token: req.headers['x-token'] || '',
    argv: req.headers['x-argv'] || '',
    collection: req.headers['x-collection'] || '',
    peer: (peer && peer.subject && peer.subject.CN) || '',
  };
  res.writeHead(200, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ status: 'ok', count }));
}).listen(%d, '0.0.0.0');
`, id, statsPath, responderPort, serverCertPath, serverKeyPath, mtls, tlsResponderPort)
}

// newTlsResponder starts a private recording service that answers over HTTPS
// with assets.ServerCert, and — when requireClientCert is set — demands a
// certificate signed by assets.CaCert in return.
//
// It hands back the same handle the plaintext responder does, so stats() reads it
// the same way.
func newTlsResponder(ctx context.Context, assets *tlsAssets, requireClientCert bool) (*responder, error) {
	id, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("name the TLS responder instance: %w", err)
	}
	// Every mount is handed to the image's non-root user: a secret mount is
	// root-owned 0400 by default, and certificate-management writes its PEM
	// files 0600, so an un-owned mount is a file this process cannot open.
	ctr := dag.Bruno().Container().
		WithMountedFile(serverCertPath, assets.ServerCert, dagger.ContainerWithMountedFileOpts{Owner: responderUser}).
		WithMountedSecret(serverKeyPath, assets.ServerKey, dagger.ContainerWithMountedSecretOpts{
			Owner: responderUser,
			Mode:  0o400,
		})
	if requireClientCert {
		ctr = ctr.WithMountedFile(responderCaPath, assets.CaCert,
			dagger.ContainerWithMountedFileOpts{Owner: responderUser})
	}
	svc := ctr.
		WithExposedPort(responderPort).
		WithExposedPort(tlsResponderPort).
		// A server never exits, so it belongs in AsService's args rather than a
		// WithExec, which would wait for it forever.
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{"node", "-e", tlsResponderScript(id, requireClientCert)},
		})
	if _, err := svc.Start(ctx); err != nil {
		return nil, fmt.Errorf("start the TLS recording responder: %w", err)
	}
	return &responder{Svc: svc, ID: id}, nil
}
