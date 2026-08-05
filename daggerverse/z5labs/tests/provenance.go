package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"
)

// pythonImage is what the fake OIDC token endpoint runs on. ":latest" is
// a moving target, so the tag is pinned.
const pythonImage = "python:3.13-alpine"

// tokenEndpointScript is the fake CI provider's OIDC token endpoint.
//
// It is deliberately fussy about the two things the module is supposed
// to get right: the bearer token has to be presented, and the audience
// has to be the one a sigstore CA would accept. A stub that answered any
// request would pass whether or not the module sent either, which would
// leave the exchange it is here to exercise untested.
const tokenEndpointScript = `
import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs

TOKEN = os.environ["ID_TOKEN"]
BEARER = os.environ["REQUEST_TOKEN"]

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def fail(self, code, why):
        body = why.encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.headers.get("Authorization") != "bearer " + BEARER:
            return self.fail(401, "missing or wrong bearer token")
        query = parse_qs(urlparse(self.path).query)
        if query.get("audience") != ["sigstore"]:
            return self.fail(400, "audience must be sigstore, got %r" % query.get("audience"))
        body = json.dumps({"value": TOKEN}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
`

// provenanceHarness is everything a publishing test needs to satisfy
// GoApp's provenance requirement: a token endpoint that behaves like a
// CI provider's, and a signing key standing in for the public sigstore
// CA this session cannot reach.
type provenanceHarness struct {
	// URL is what a caller would read out of
	// ACTIONS_ID_TOKEN_REQUEST_URL. Its host is a placeholder: the
	// endpoint's real address is assigned by the engine, and Service is
	// how the module under test learns it.
	URL string
	// Service is the token endpoint itself, handed to the module so it
	// resolves the address inside its own runtime. A hostname resolved
	// here would not be reachable from there.
	Service *dagger.Service
	// RequestToken is the bearer for that endpoint.
	RequestToken *dagger.Secret
	// SigningKey is the PEM EC private key GoApp signs with.
	SigningKey *dagger.Secret
	// Public is the matching public key, kept so a test can verify a
	// signature the module produced rather than merely observing that
	// one is present.
	Public *ecdsa.PublicKey
	// Claims are the claims the endpoint mints into the token, so a test
	// can assert the predicate reports them and not something else.
	Claims map[string]any
}

// newProvenanceHarness mints a workload identity token, stands the token
// endpoint up, and generates the signing key.
//
// Every secret here is generated at run time. Nothing about the identity
// is a constant a test could accidentally assert against itself.
func newProvenanceHarness(ctx context.Context, commit string) (*provenanceHarness, error) {
	nonce, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (nonce): %v", err)
	}
	bearer, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (bearer): %v", err)
	}

	claims := map[string]any{
		"iss":              "https://token.example/" + nonce[:8],
		"sub":              "repo:z5labs/example:ref:refs/heads/main",
		"aud":              "sigstore",
		"repository":       "z5labs/example",
		"job_workflow_ref": "z5labs/example/.github/workflows/ci.yml@refs/heads/main",
		"run_id":           nonce[:12],
		"exp":              time.Now().Add(time.Hour).Unix(),
	}
	if commit != "" {
		claims["sha"] = commit
	}
	token, err := unsignedJWT(claims)
	if err != nil {
		return nil, err
	}

	svc := dag.Container().From(pythonImage).
		WithEnvVariable("NONCE", nonce).
		WithEnvVariable("ID_TOKEN", token).
		WithEnvVariable("REQUEST_TOKEN", bearer).
		WithExposedPort(8080).
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{"python", "-u", "-c", tokenEndpointScript},
		})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode signing key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})

	return &provenanceHarness{
		URL:          "http://id-token-endpoint.invalid/token",
		Service:      svc,
		RequestToken: dag.SetSecret("z5labs-id-token-request-"+nonce[:16], bearer),
		SigningKey:   dag.SetSecret("z5labs-signing-key-"+nonce[16:32], string(keyPEM)),
		Public:       &key.PublicKey,
		Claims:       claims,
	}, nil
}

// opts folds the harness into the GoApp options every publishing test
// now has to pass. Publishing without provenance is refused, so this is
// not optional decoration — it is what makes a publish possible at all.
func (h *provenanceHarness) opts(base dagger.Z5LabsGoAppOpts) dagger.Z5LabsGoAppOpts {
	base.IDTokenRequestURL = h.URL
	base.IDTokenRequestToken = h.RequestToken
	base.IDTokenService = h.Service
	base.SigningKey = h.SigningKey
	return base
}

// unsignedJWT renders claims as a JWT with an empty signature segment.
//
// The signature is empty because nothing verifies it: the module takes
// the token straight from the issuer's endpoint over the session
// network, and the claims are re-verified by the certificate authority
// in the keyless path. A test that signed it would be asserting against
// a check that does not exist.
func unsignedJWT(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(header) + "." + enc.EncodeToString(payload) + ".", nil
}
