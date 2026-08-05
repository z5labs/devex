package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// tlsMaterial resolves the caller's TLS material — a private CA to verify the
// registry against, and a client certificate to authenticate with — into the
// tls.Config every request on this connection will use.
//
// It returns nil when none of it was supplied, which leaves both client
// libraries on their own defaults: the system trust store, and no client
// certificate. That is the shape every existing caller has.
//
// The second return value is every plaintext read on the way, so scrub can
// keep it out of an error leaving this module. The client key is the only
// secret of the three — a CA certificate and a client certificate are public
// by construction, which is why they cross as files and the key does not — and
// it is returned even when the call then fails, because a key read before an
// unrelated failure is still a key that must not reach a log.
func (reg *Registry) tlsMaterial(ctx context.Context) (*tls.Config, []string, error) {
	// The halves of a client certificate are checked before anything is read.
	// A caller who supplied one half believed they were authenticating; the
	// alternative to refusing here is falling back to anonymous TLS and
	// letting them discover otherwise from a 401 much later, somewhere that
	// says nothing about the certificate they thought they were presenting.
	switch {
	case reg.ClientCert != nil && reg.ClientKey == nil:
		return nil, nil, errors.New(
			"registry: clientCert was supplied without clientKey; a client certificate needs both halves")
	case reg.ClientKey != nil && reg.ClientCert == nil:
		return nil, nil, errors.New(
			"registry: clientKey was supplied without clientCert; a client certificate needs both halves")
	case reg.CaCert == nil && reg.ClientCert == nil:
		return nil, nil, nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if reg.CaCert != nil {
		pemBytes, err := reg.CaCert.Contents(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("read registry CA certificate: %v", err)
		}
		// The CA is added to the system trust store rather than replacing it.
		// A caller naming a private CA is supplying a trust anchor for one
		// registry, not declaring that every public CA has become
		// untrustworthy — and Copy reads a source on another host through the
		// same connection when that host is this registry, so silently
		// narrowing the pool would break unrelated verification for a reason
		// nothing in the call said.
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(pemBytes)) {
			return nil, nil, errors.New("registry: caCert holds no PEM-encoded certificate")
		}
		cfg.RootCAs = pool
	}

	var redact []string
	if reg.ClientCert != nil {
		certPem, err := reg.ClientCert.Contents(ctx)
		if err != nil {
			return nil, redact, fmt.Errorf("read registry client certificate: %v", err)
		}
		keyPem, err := reg.ClientKey.Plaintext(ctx)
		if err != nil {
			return nil, redact, fmt.Errorf("read registry client key: %v", err)
		}
		redact = append(redact, keyPem)

		pair, err := tls.X509KeyPair([]byte(certPem), []byte(keyPem))
		if err != nil {
			// crypto/tls quotes no part of the key in its own messages, but
			// the cost of being wrong once is a private key in a CI log
			// forever, so the caller gets the shape of the failure rather
			// than the library's text.
			return nil, redact, errors.New(
				"registry: clientCert and clientKey are not a matching PEM certificate and private key")
		}
		cfg.Certificates = []tls.Certificate{pair}
	}

	return cfg, redact, nil
}

// tlsClientConfig is the TLS configuration every request on this connection
// uses, or nil when the client libraries' own defaults are already right.
//
// insecure and the caller's TLS material are independent, and this is where
// that is enforced: supplying a CA does not switch verification off, and
// switching verification off does not discard a client certificate. Neither is
// inferred from the other — they are two answers to two different questions,
// and a module that conflated them would silently stop doing one of the things
// it was asked to do.
func (c *conn) tlsClientConfig() *tls.Config {
	if c.tlsConfig == nil && !c.insecure {
		return nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.tlsConfig != nil {
		cfg = c.tlsConfig.Clone()
	}
	if c.insecure {
		cfg.InsecureSkipVerify = true //nolint:gosec // opt-in via insecure
	}
	return cfg
}

// tlsTransport is go-containerregistry's default transport with cfg applied.
//
// It is a clone of that default rather than a bare http.Transport because the
// default carries the connection limits and timeouts go-containerregistry
// tuned for registry traffic, and a caller who supplies a CA has not asked to
// give those up.
func tlsTransport(cfg *tls.Config) http.RoundTripper {
	tr, ok := remote.DefaultTransport.(*http.Transport)
	if !ok {
		return remote.DefaultTransport
	}
	clone := tr.Clone()
	clone.TLSClientConfig = cfg
	return clone
}
