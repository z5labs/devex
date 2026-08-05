package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"time"

	"dagger/z-5-labs/internal/dagger"
)

// defaultFulcioURL is the public sigstore certificate authority. Keyless
// signing means "no key this pipeline has to manage", not "no key at
// all": an ephemeral key is generated per publish, bound to the workload
// identity by a short-lived certificate from this CA, and thrown away.
// Nothing is stored, so nothing can leak or expire unnoticed.
const defaultFulcioURL = "https://fulcio.sigstore.dev"

// fulcioTimeout bounds the certificate request for the same reason
// idTokenTimeout bounds the token exchange.
const fulcioTimeout = 60 * time.Second

// dssePayloadType is the payload type of an in-toto statement inside a
// DSSE envelope. It is part of the signed pre-authentication encoding,
// so a verifier that expects an attestation cannot be handed a different
// document under the same signature.
const dssePayloadType = "application/vnd.in-toto+json"

// signer holds the ephemeral key a publish signs its attestations with,
// together with the certificate chain that says whose key it is.
type signer struct {
	key *ecdsa.PrivateKey
	// chain is the PEM certificate chain binding key to an identity.
	// Empty for a caller-supplied signing key, where the caller owns the
	// question of how a verifier learns the public key.
	chain []byte
	// identity is what the workload identity token said, and is what
	// ends up in the provenance predicate.
	identity *workloadIdentity
}

// newSigner produces the signer for one publish.
//
// There are two modes and the choice between them is explicit, never
// inferred from the shape of the rest of the call. That matters more
// than it looks: the tempting shortcut is to relax signing whenever the
// registry is a local service, which carves the relaxation into exactly
// the shape of the test suite and leaves the production path as the only
// unexercised one.
//
//   - Keyless (signingKey nil). An ephemeral P-256 key is bound to the
//     workload identity by a short-lived sigstore certificate. This is
//     what a CI publish uses.
//   - Supplied key (signingKey set). The caller's PEM-encoded EC private
//     key signs instead, and no certificate is fetched. This is for a
//     build that cannot reach a public CA — an air-gapped release, and
//     the test suite, which has a real OIDC issuer of its own but no
//     sigstore.
//
// The identity exchange happens in both modes. The predicate's contents
// therefore come from a real token in both, and the only thing the
// supplied-key mode changes is who vouches for the public key.
func newSigner(ctx context.Context, idTokenRequestURL string, idTokenRequestToken, signingKey *dagger.Secret, idTokenService *dagger.Service) (*signer, error) {
	rawToken, identity, err := exchangeIDToken(ctx, idTokenRequestURL, idTokenRequestToken, idTokenService)
	if err != nil {
		return nil, err
	}
	if signingKey != nil {
		key, err := parseSigningKey(ctx, signingKey)
		if err != nil {
			return nil, err
		}
		return &signer{key: key, identity: identity}, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral signing key: %v", err)
	}
	chain, err := fulcioCertificate(ctx, key, rawToken, identity.Subject)
	if err != nil {
		return nil, err
	}
	return &signer{key: key, chain: chain, identity: identity}, nil
}

// parseSigningKey reads a PEM-encoded EC private key out of a secret.
// Both the SEC 1 ("EC PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") shapes are
// accepted, because which one a tool emits is not a decision the caller
// usually gets to make.
func parseSigningKey(ctx context.Context, secret *dagger.Secret) (*ecdsa.PrivateKey, error) {
	text, err := secret.Plaintext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %v", err)
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, fmt.Errorf("signing key is not PEM-encoded")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %v", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key must be an EC key, got %T", parsed)
	}
	return key, nil
}

// fulcioCertificate exchanges the workload identity token and an
// ephemeral public key for a short-lived signing certificate.
//
// The proof of possession is a signature over the token's subject claim,
// which is what stops a holder of someone else's token from binding
// their own key to that identity.
//
// This is the one part of the publish path the test suite cannot
// exercise: it needs a live sigstore CA, and the tests run against a
// registry with no issuer beside it. Everything upstream of it — the
// token exchange, the claim mapping, the statement, the envelope, the
// attach and the retrieval — runs identically in both modes, so what is
// untested here is the HTTP shape of one request and not the mechanism.
func fulcioCertificate(ctx context.Context, key *ecdsa.PrivateKey, rawToken, subject string) ([]byte, error) {
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("encode ephemeral public key: %v", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})

	digest := sha256.Sum256([]byte(subject))
	proof, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign proof of possession: %v", err)
	}

	body, err := json.Marshal(map[string]any{
		"credentials": map[string]any{"oidcIdentityToken": rawToken},
		"publicKeyRequest": map[string]any{
			"publicKey": map[string]any{
				"algorithm": "ECDSA",
				"content":   string(publicPEM),
			},
			"proofOfPossession": base64.StdEncoding.EncodeToString(proof),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode certificate request: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, fulcioTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		defaultFulcioURL+"/api/v2/signingCert", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build certificate request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request signing certificate from %s: %v", defaultFulcioURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read signing certificate response: %v", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("signing certificate request to %s returned %s", defaultFulcioURL, resp.Status)
	}

	var payload struct {
		Embedded struct {
			Chain struct {
				Certificates []string `json:"certificates"`
			} `json:"chain"`
		} `json:"signedCertificateEmbeddedSct"`
		Detached struct {
			Chain struct {
				Certificates []string `json:"certificates"`
			} `json:"chain"`
		} `json:"signedCertificateDetachedSct"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode signing certificate response: %v", err)
	}
	certs := payload.Embedded.Chain.Certificates
	if len(certs) == 0 {
		certs = payload.Detached.Chain.Certificates
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("signing certificate response from %s carried no chain", defaultFulcioURL)
	}
	var chain bytes.Buffer
	for _, cert := range certs {
		chain.WriteString(cert)
		if !bytes.HasSuffix(chain.Bytes(), []byte("\n")) {
			chain.WriteByte('\n')
		}
	}
	return chain.Bytes(), nil
}

// dsseEnvelope wraps and signs a statement.
//
// DSSE rather than a detached signature over the JSON: the payload type
// is signed alongside the payload, so a document cannot be replayed as a
// different kind of attestation, and the encoding is stable in a way raw
// JSON is not. cert and chain are cosign's extension fields, carried so
// a verifier can recover the signing identity from the envelope alone
// rather than needing a separate bundle.
func (s *signer) dsseEnvelope(statement []byte) ([]byte, error) {
	pae := preAuthenticationEncoding(dssePayloadType, statement)
	digest := sha256.Sum256(pae)
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign attestation: %v", err)
	}
	signature := map[string]string{
		"sig":   base64.StdEncoding.EncodeToString(sig),
		"keyid": s.keyID(),
	}
	if len(s.chain) > 0 {
		signature["cert"] = string(s.chain)
	} else {
		publicDER, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("encode signing public key: %v", err)
		}
		signature["publicKey"] = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	}
	envelope := map[string]any{
		"payloadType": dssePayloadType,
		"payload":     base64.StdEncoding.EncodeToString(statement),
		"signatures":  []map[string]string{signature},
	}
	out, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode attestation envelope: %v", err)
	}
	return append(out, '\n'), nil
}

// keyID is the SHA-256 of the DER public key, which is how a verifier
// tells which key in a bundle produced a signature.
func (s *signer) keyID() string {
	der, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return fmt.Sprintf("%x", sum)
}

// preAuthenticationEncoding builds DSSE's PAE:
//
//	"DSSEv1" SP len(type) SP type SP len(body) SP body
//
// Length-prefixing both fields is what makes the encoding unambiguous —
// no choice of payload can imitate a different type/payload pair.
func preAuthenticationEncoding(payloadType string, body []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "DSSEv1 %d %s %d ", len(payloadType), payloadType, len(body))
	buf.Write(body)
	return buf.Bytes()
}
