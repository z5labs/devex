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
	"regexp"
	"strings"
	"time"

	"dagger/z-5-labs/internal/dagger"
)

// defaultFulcioURL is the public sigstore certificate authority. Keyless
// signing means "no key this pipeline has to manage", not "no key at
// all": an ephemeral key is generated per publish, bound to the workload
// identity by a short-lived certificate from this CA, and thrown away.
// Nothing is stored, so nothing can leak or expire unnoticed.
const defaultFulcioURL = "https://fulcio.sigstore.dev"

// sigstoreEndpoints is where one publish's keyless signing talks: the
// certificate authority that certifies the ephemeral key, and the
// transparency log whose countersignature makes that certificate still
// mean something after it expires.
//
// The pair travels together and is resolved once per publish, which is the
// point rather than a convenience. A publish certified by one authority and
// logged in a different one's log is not a weaker keyless signature, it is
// an incoherent one — the log entry a verifier checks the certificate's
// lifetime against would have been made somewhere that never saw it — so
// there is no state in which only one of these is redirected.
type sigstoreEndpoints struct {
	fulcio string
	rekor  string
}

// defaultSigstore is the public sigstore, which is what every publish that
// does not stand up its own uses.
func defaultSigstore() sigstoreEndpoints {
	return sigstoreEndpoints{fulcio: defaultFulcioURL, rekor: defaultRekorURL}
}

// serviceOrigin is the scheme-and-authority a session-hosted sigstore is
// reachable at from this module's runtime.
//
// The service is started here for the same reason boundToService starts the
// token endpoint: an engine-assigned address is only resolvable once the
// service is up, and a hostname resolved in someone else's container is not
// reachable from this one.
//
// The scheme is http and is not a parameter. A service exists only inside
// the session that created it and has no name outside it, so there is no
// certificate anyone could have issued for it — the same argument
// withAudience already makes for a session-hosted token endpoint. What
// would be gained by allowing https here is nothing; what would be lost is
// that "the address is not the caller's to write" stops being true of every
// part of the URL.
func serviceOrigin(ctx context.Context, svc *dagger.Service, what string) (string, error) {
	started, err := svc.Start(ctx)
	if err != nil {
		return "", fmt.Errorf("start the session-hosted %s: %v", what, err)
	}
	endpoint, err := started.Endpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve the session-hosted %s's endpoint: %v", what, err)
	}
	return "http://" + endpoint, nil
}

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
	// keyless says which mode produced this signer, and is not inferred
	// from any other field.
	//
	// It exists because "keyless" and "has a certificate chain" are two
	// propositions, and the code that reads them apart is the code that
	// decides whether a signature carries an identity at all. Inferring the
	// mode from an empty chain means a keyless publish that somehow lost its
	// certificate publishes a signature with no certificate and no log
	// entry — verifiable by neither documented command — and reports
	// success. That is the refusal in newSigner quietly becoming
	// conditional, which is the one thing it exists not to be.
	keyless bool
	// chain is the PEM certificate chain binding key to an identity.
	// Empty for a caller-supplied signing key, where the caller owns the
	// question of how a verifier learns the public key.
	chain []byte
	// identity is what the workload identity token said, and is what
	// ends up in the provenance predicate.
	identity *workloadIdentity
	// rekorURL is the transparency log this publish's signatures are
	// recorded in. It is carried on the signer rather than read from a
	// constant where it is used, so the log a signature is logged in is
	// the log of the sigstore that issued the certificate beside it —
	// resolved once, in one place, for the reason sigstoreEndpoints
	// states. Empty in the supplied-key mode, which logs nothing.
	rekorURL string
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
//     what a CI publish uses, and it is what sigstore names — the public
//     sigstore unless the caller stood one up in the session.
//   - Supplied key (signingKey set). The caller's PEM-encoded EC private
//     key signs instead, and no certificate is fetched. This is for a
//     build that cannot reach a CA at all — an air-gapped release.
//
// The identity exchange happens in both modes. The predicate's contents
// therefore come from a real token in both, and the only thing the
// supplied-key mode changes is who vouches for the public key.
func newSigner(ctx context.Context, idTokenRequestURL string, idTokenRequestToken, signingKey *dagger.Secret, idTokenService *dagger.Service, sigstore sigstoreEndpoints) (*signer, error) {
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
	chain, err := fulcioCertificate(ctx, key, rawToken, identity.Subject, sigstore.fulcio)
	if err != nil {
		return nil, err
	}
	return &signer{key: key, keyless: true, chain: chain, identity: identity, rekorURL: sigstore.rekor}, nil
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
// fulcioURL is the authority to ask, which is the public sigstore unless
// the caller stood one up inside the session — see App.WithSessionSigstore
// for why that seam takes services rather than a URL. The test suite drives
// this function against a CA of its own, which verifies the proof of
// possession before it issues, so what is exercised is the request this
// builds and not merely the fact that it was sent.
func fulcioCertificate(ctx context.Context, key *ecdsa.PrivateKey, rawToken, subject, fulcioURL string) ([]byte, error) {
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
		fulcioURL+"/api/v2/signingCert", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build certificate request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request signing certificate from %s: %v", fulcioURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read signing certificate response: %v", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("signing certificate request to %s returned %s%s", fulcioURL, resp.Status, responseDetail(raw))
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
		return nil, fmt.Errorf("signing certificate response from %s carried no chain", fulcioURL)
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

// responseDetail is a bounded rendering of the message a rejected sigstore
// response carried, for appending to the error that reports the status.
//
// A status code alone says a request was refused and never why, and both
// sigstore services answer a refusal with a message saying which part of the
// request they did not accept. Discarding it turns "the proof of possession
// did not verify" into "returned 400 Bad Request", which is a publish
// failure nobody can act on without a packet capture.
//
// # It renders a parsed field, never the body
//
// The body is a remote server's to choose, and the request it is refusing
// carries the raw workload identity token — a bearer credential good enough
// to mint a certificate for this build's identity, and a plain string rather
// than a dagger.Secret, so nothing in the engine scrubs it. A server that
// echoes the request it rejected is not exotic; gateways and WAFs quote the
// offending payload routinely. Splicing the body verbatim into an error
// would therefore hand any server this module talks to — including, since
// WithSessionSigstore exists, one the caller stood up — a channel into this
// build's logs and traces.
//
// So only a known error field of a known error shape is rendered, and a body
// that is not one of those shapes contributes nothing, which is exactly the
// status-only behaviour that came before. What is left is still
// attacker-chosen when the CA is, so it is scrubbed of anything JWT-shaped
// as well: the point is that no path renders a token, not that one path
// happens not to.
func responseDetail(raw []byte) string {
	// Fulcio and Rekor are both grpc-gateway services and answer a refusal
	// with `{"code":…,"message":"…"}`. The others are here because an
	// intermediary that speaks OAuth answers in its own vocabulary and is
	// still describing this request.
	var payload struct {
		Message          string `json:"message"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	detail := payload.Message
	if detail == "" {
		detail = payload.Error
	}
	if detail == "" {
		detail = payload.ErrorDescription
	}
	detail = strings.Join(strings.Fields(detail), " ")
	if detail == "" {
		return ""
	}
	detail = jwtLike.ReplaceAllString(detail, "[redacted]")
	// Bounded on runes rather than bytes: this goes into an error a caller
	// reads in a terminal, and cutting a multi-byte rune in half renders as
	// a replacement character in the middle of the one useful sentence.
	const limit = 200
	if runes := []rune(detail); len(runes) > limit {
		detail = string(runes[:limit]) + "…"
	}
	return ": " + detail
}

// jwtLike matches a JWS compact serialization — three base64url segments
// separated by dots, the last of which may be empty for an unsigned token.
//
// It is deliberately loose. Its job is not to parse a token but to make
// certain that nothing token-shaped survives into an error message, and a
// pattern that matched only well-formed tokens would pass a truncated one
// through — which is still most of a credential.
var jwtLike = regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)

// dsseEnvelope wraps and signs a statement.
//
// DSSE rather than a detached signature over the JSON: the payload type
// is signed alongside the payload, so a document cannot be replayed as a
// different kind of attestation, and the encoding is stable in a way raw
// JSON is not. cert is cosign's extension field, carried so a verifier can
// recover the signing identity from the envelope alone rather than needing a
// separate bundle.
//
// # It carries the whole chain in cert, where cosign would split it
//
// cosign's own split is the one signatureAnnotations makes for the signature
// annotations: the leaf goes in cert and the intermediates in chain. This
// envelope writes the whole PEM chain into cert and no chain field at all,
// which predates devex#419 and is left alone by it — moving certificates
// between fields would change bytes already published under a documented
// read path, which is not something to do incidentally to adding a log entry.
//
// What that costs a reader is one surprise, so it is written down here and
// in the package doc rather than left in the bytes. `cert` holds one or more
// certificates, leaf first. A single pem.Decode or `openssl x509` reads the
// leaf and is correct. Anything that requires cert to hold exactly one
// certificate is not, and the log entry below names the leaf alone — so the
// certificate in the entry is the *first* certificate in cert, never the
// whole field.
//
// # The envelope is recorded in the transparency log too
//
// A keyless envelope carries its own log entry, in the bundle field beside
// the certificate, and that is devex#419's decision. The alternative on the
// table was to leave it unlogged and call the indirect anchor sufficient —
// the envelope is a referrer of a digest whose signature *is* logged — and it
// was rejected, for two reasons.
//
// The first is that the indirect anchor does not establish the property. A
// referrer can be attached to a digest at any time by anyone who can push to
// the repository, so "this envelope hangs off a digest that was signed at
// time T" says nothing about when the envelope was signed. What the log
// establishes for a signature is that the signature existed while the
// certificate was live, and nothing but a log entry over *this* signature
// establishes that for this signature.
//
// The second is that the module already holds the opposite opinion one file
// away and cannot coherently hold both. sign.go's defaultRekorURL comment
// says keyless signing with no log entry is not a weaker signature but a
// broken one, and signatureAnnotations refuses to publish one. The envelope
// is signed by the same ephemeral key under the same ten-minute certificate;
// there is no argument that makes the signature's expiry fatal and the
// envelope's acceptable.
//
// Three things about the shape, each of which is a decision:
//
//   - The bundle is embedded rather than left to be looked up, for the reason
//     rekorBundle gives on the signature's side: a consumer inside a network
//     that cannot reach rekor.sigstore.dev still has everything the check
//     needs. It keeps the envelope verifiable on its own bytes, which is what
//     the package doc promises about it.
//   - It hangs off the signature, not off the envelope. A log entry
//     countersigns one signature, and DSSE allows an envelope to carry
//     several; a top-level field could not say which one it timestamped.
//     cert and chain are already extension fields in the same place, so this
//     is the position cosign established rather than a new one.
//   - It is the same bundle format the signature's dev.sigstore.cosign/bundle
//     annotation carries, as an object rather than as a string containing
//     JSON. One format is one thing for a consumer to learn, and a document
//     meant to be read with jq should not need a second parse to reach half
//     of itself.
//
// # What it costs
//
// One more transparency log upload, and it is stated here the way signImage
// states its own because it is the same cost: another serial round trip to a
// shared third-party service inside the publish, bounded by rekorTimeout, and
// another way for a publish to fail that has nothing to do with the artifact.
//
// The unit is one per repository published to, not one per platform and not
// one per publish. There is a single provenance statement per repository —
// its subject is that repository's digest — and Publish loops repositories,
// calling attachAttestations and signImage inside the loop. So a
// four-platform keyless release costs six round trips to one repository, and
// twelve to two.
func (s *signer) dsseEnvelope(ctx context.Context, statement []byte) ([]byte, error) {
	pae := preAuthenticationEncoding(dssePayloadType, statement)
	digest := sha256.Sum256(pae)
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign attestation: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(sig)
	signature := map[string]any{
		"sig":   encoded,
		"keyid": s.keyID(),
	}
	// The mode is read off the signer rather than off the chain, which is the
	// rule the keyless field exists to make possible and the one
	// signatureAnnotations follows. Branching on a non-empty chain would mean
	// a keyless publish that somehow lost its certificate silently produced
	// the supplied-key envelope — a bare public key, no identity, and no log
	// entry — and reported success.
	if !s.keyless {
		publicDER, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("encode signing public key: %v", err)
		}
		signature["publicKey"] = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	} else {
		bundle, err := s.logEnvelopeSignature(ctx, digest[:], encoded)
		if err != nil {
			return nil, err
		}
		signature["cert"] = string(s.chain)
		signature["bundle"] = json.RawMessage(bundle)
	}
	envelope := map[string]any{
		"payloadType": dssePayloadType,
		"payload":     base64.StdEncoding.EncodeToString(statement),
		"signatures":  []map[string]any{signature},
	}
	out, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode attestation envelope: %v", err)
	}
	return append(out, '\n'), nil
}

// logEnvelopeSignature records the envelope's signature in the transparency
// log and returns the bundle to embed beside it.
//
// The certificate the entry names is the leaf and not the whole chain, which
// is the same split signatureAnnotations makes and for a reason worth stating
// rather than inheriting: the entry says which key signed, and a log lenient
// enough to accept a chain would record an intermediate as the signer.
//
// The envelope's own cert field still carries the whole chain, which is what
// it carried before this and is dsseEnvelope's divergence from cosign rather
// than an agreement with it — see the section there that says so. The
// consequence to hold on to is that the entry's certificate and the cert
// field are deliberately not the same bytes: the entry names the first
// certificate in cert.
//
// The two refusals mirror signatureAnnotations' exactly. Neither is reachable
// today — newSigner sets the chain and the log together on every keyless
// signer, and refuses if the CA returned no chain — and they are here for the
// same reason the ones there are: what they would otherwise let out is a
// provenance envelope carrying an identity nothing vouches for, or a
// certificate that expires into unverifiability, published as though it were
// complete.
func (s *signer) logEnvelopeSignature(ctx context.Context, digest []byte, signature string) ([]byte, error) {
	if len(s.chain) == 0 {
		return nil, fmt.Errorf(
			"keyless signing produced no certificate chain, so nothing would vouch for the key that signed the " +
				"provenance envelope; refusing to publish an attestation no documented check can attribute")
	}
	if strings.TrimSpace(s.rekorURL) == "" {
		return nil, fmt.Errorf(
			"keyless signing has no transparency log to record in, so nothing would establish the signing certificate " +
				"was live when it signed the provenance envelope; refusing to publish an attestation that expires " +
				"into unverifiability")
	}
	leaf, _, err := splitCertificateChain(s.chain)
	if err != nil {
		return nil, err
	}
	bundle, err := rekorBundle(ctx, digest, signature, leaf, s.rekorURL)
	if err != nil {
		return nil, err
	}
	return bundle, nil
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
