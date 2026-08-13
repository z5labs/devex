// Command sigstore is a certificate authority and a transparency log that
// stand in for the public sigstore inside one Dagger session.
//
// It exists so the module's keyless signing path can be executed rather than
// only described. The public sigstore is not reachable from a test session in
// any useful way — it would issue certificates for this suite's fake workload
// identity, which is exactly what a CA must not do — so the choice is between
// a local authority and a keyless path whose first execution is a real
// release.
//
// It is deliberately fussy, for the same reason the fake OIDC token endpoint
// is. A CA that issued for any request would leave the proof of possession,
// the audience and the encoding of the request untested while the test went
// green, which is the failure being guarded against rather than a stricter
// version of it. So:
//
//   - the identity token has to be present, decodable and minted for the
//     sigstore audience, exactly as a real CA requires;
//   - the proof of possession has to verify against the public key in the
//     same request, which is the check that stops a holder of someone else's
//     token binding their own key to that identity;
//   - the log entry's signature has to verify against the certificate the
//     entry names, over the hash the entry names.
//
// Every refusal answers with a message saying which of those failed, because
// the module reports a rejected sigstore response by status and detail and a
// bare 400 is a publish failure nobody can act on.
//
// What it is not: it is not a transparency log. Nothing here is appended to
// anything, the countersignature is made with a key nobody trusts, and there
// is no inclusion proof. A verifier therefore still has to be told to ignore
// the log; what the round trip proves is that the entry the module builds is
// well formed and internally consistent, and that it carries the log's answer
// back into the bundle annotation rather than inventing one.
package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// oidOIDCIssuer is the Fulcio X.509 extension the OIDC issuer goes in, and
// it is what `cosign verify --certificate-oidc-issuer` reads.
//
// This is the original spelling (1.3.6.1.4.1.57264.1.1), whose value is the
// issuer string raw. Fulcio also emits 1.3.6.1.4.1.57264.1.8, which is the
// same string DER-encoded as a UTF8String, and cosign prefers it where it is
// present. Only the original is emitted here: a verifier that knows the newer
// one falls back to this, so emitting one is compatible with both readers,
// while emitting a v2 extension whose encoding a given cosign reads raw would
// make the issuer compare against tag and length bytes.
var oidOIDCIssuer = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}

// certificateLifetime is how long an issued certificate is valid for.
//
// Fulcio issues for ten minutes, which is short because the certificate is
// meant to be worthless the moment the log entry exists. This is longer for
// one reason and it is not a relaxation of anything the module does: a
// verification that deliberately ignores the log checks the certificate
// against the wall clock, so the window only has to outlive the session that
// created it. Nothing is issued here that outlives the container.
const certificateLifetime = time.Hour

// backdate absorbs clock skew between the engine and this container.
const backdate = 5 * time.Minute

var (
	rootPEM         string
	intermediatePEM string
	intermediate    *x509.Certificate
	intermediateKey *ecdsa.PrivateKey
	// logID is minted by the harness and asserted against the bundle
	// annotation, which is what makes "the module carried the log's answer
	// through" checkable rather than assumed.
	logID string
	// logKey countersigns entries. It is generated here and told to nobody:
	// the signed entry timestamp has to be present and non-empty, and
	// nothing in this session is entitled to believe it.
	logKey   *ecdsa.PrivateKey
	logIndex atomic.Int64
)

func main() {
	var err error
	rootPEM = mustEnv("ROOT_CERT_PEM")
	intermediatePEM = mustEnv("INTERMEDIATE_CERT_PEM")
	logID = mustEnv("LOG_ID")

	intermediate, err = parseCertificatePEM([]byte(intermediatePEM))
	if err != nil {
		log.Fatalf("parse intermediate certificate: %v", err)
	}
	intermediateKey, err = parseECKeyPEM([]byte(mustEnv("INTERMEDIATE_KEY_PEM")))
	if err != nil {
		log.Fatalf("parse intermediate key: %v", err)
	}
	logKey, err = ecdsa.GenerateKey(intermediate.PublicKey.(*ecdsa.PublicKey).Curve, rand.Reader)
	if err != nil {
		log.Fatalf("generate log key: %v", err)
	}

	// One binary, two roles, and each instance serves only its own endpoint.
	//
	// Serving both from one process would have been less code and would have
	// hidden something worth knowing: the module resolves a CA endpoint and a
	// log endpoint separately, and if it ever asked one for the other's work
	// a server answering both would issue the certificate anyway. Split, that
	// mistake is a 404 naming the role that was asked.
	mux := http.NewServeMux()
	switch role := mustEnv("ROLE"); role {
	case "fulcio":
		mux.HandleFunc("/api/v2/signingCert", signingCert)
	case "rekor":
		mux.HandleFunc("/api/v1/log/entries", logEntries)
	default:
		log.Fatalf("ROLE is %q, want fulcio or rekor", role)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "this session's %s serves no %s", os.Getenv("ROLE"), r.URL.Path)
	})
	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}

func mustEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

// fail answers with a status and a reason. The reason is the whole point:
// the module appends a bounded snippet of this body to the error it reports,
// so a refusal here names the part of the request that was wrong.
func fail(w http.ResponseWriter, code int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), code)
}

// signingCert is Fulcio's certificate endpoint.
func signingCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "signingCert takes POST, got %s", r.Method)
		return
	}
	var request struct {
		Credentials struct {
			OIDCIdentityToken string `json:"oidcIdentityToken"`
		} `json:"credentials"`
		PublicKeyRequest struct {
			PublicKey struct {
				Algorithm string `json:"algorithm"`
				Content   string `json:"content"`
			} `json:"publicKey"`
			ProofOfPossession string `json:"proofOfPossession"`
		} `json:"publicKeyRequest"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		fail(w, http.StatusBadRequest, "decode certificate request: %v", err)
		return
	}
	if request.Credentials.OIDCIdentityToken == "" {
		fail(w, http.StatusBadRequest, "certificate request carried no credentials.oidcIdentityToken")
		return
	}
	if request.PublicKeyRequest.PublicKey.Algorithm != "ECDSA" {
		fail(w, http.StatusBadRequest, "publicKeyRequest.publicKey.algorithm is %q, want ECDSA",
			request.PublicKeyRequest.PublicKey.Algorithm)
		return
	}

	claims, err := claimsOf(request.Credentials.OIDCIdentityToken)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !audienceIsSigstore(claims) {
		fail(w, http.StatusBadRequest, "identity token was not minted for the sigstore audience, aud is %v", claims["aud"])
		return
	}
	issuer, _ := claims["iss"].(string)
	subject, _ := claims["sub"].(string)
	if issuer == "" || subject == "" {
		fail(w, http.StatusBadRequest, "identity token carries no iss/sub claim, so it identifies nobody")
		return
	}

	public, err := parsePublicKeyPEM([]byte(request.PublicKeyRequest.PublicKey.Content))
	if err != nil {
		fail(w, http.StatusBadRequest, "publicKeyRequest.publicKey.content: %v", err)
		return
	}
	proof, err := base64.StdEncoding.DecodeString(request.PublicKeyRequest.ProofOfPossession)
	if err != nil {
		fail(w, http.StatusBadRequest, "publicKeyRequest.proofOfPossession is not base64: %v", err)
		return
	}
	// The proof is a signature over the token's subject claim. Checking it is
	// what makes this a CA rather than a certificate vending machine, and it
	// is the assertion that the module signed the right bytes with the key it
	// is asking to have certified.
	digest := sha256.Sum256([]byte(subject))
	if !ecdsa.VerifyASN1(public, digest[:], proof) {
		fail(w, http.StatusBadRequest,
			"proof of possession does not verify: it must be an ASN.1 ECDSA signature over the SHA-256 of the token's sub claim, made with the key being certified")
		return
	}

	identity, err := identityURI(claims, subject)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	leaf, err := issueCertificate(public, identity, issuer)
	if err != nil {
		fail(w, http.StatusInternalServerError, "issue certificate: %v", err)
		return
	}

	// The chain is leaf, intermediate, root, which is the order Fulcio
	// answers in and the order splitCertificateChain is asserted against:
	// the first block is the identity and everything after it is how a
	// verifier walks to a root.
	respond(w, http.StatusCreated, map[string]any{
		"signedCertificateEmbeddedSct": map[string]any{
			"chain": map[string]any{
				"certificates": []string{string(leaf), intermediatePEM, rootPEM},
			},
		},
	})
}

// identityURI is what goes in the certificate's subject alternative name,
// and is what `cosign verify --certificate-identity` matches.
//
// Fulcio maps a GitHub Actions token's job_workflow_ref claim to
// https://github.com/<that>, which is why the identity a consumer is told to
// match is a workflow URL rather than the raw claim. That mapping is copied
// here so the command this suite runs is the command in Publish's doc
// comment; any other provider's token falls back to its subject, which has to
// parse as a URI because a SAN URI is not free text.
func identityURI(claims map[string]any, subject string) (string, error) {
	identity := subject
	if ref, ok := claims["job_workflow_ref"].(string); ok && ref != "" {
		identity = "https://github.com/" + strings.TrimPrefix(ref, "/")
	}
	parsed, err := url.Parse(identity)
	if err != nil || parsed.Scheme == "" {
		return "", fmt.Errorf("identity %q is not a URI, so it cannot go in a subject alternative name", identity)
	}
	return identity, nil
}

// issueCertificate signs a leaf for the given key, identity and issuer.
//
// The three things cosign requires of it, none of which is optional: the
// identity is a SAN URI and the subject is empty, so the SAN is critical; the
// key usage is digital signature and the extended key usage is code signing,
// because that is what the chain is verified for; and the issuer lives in the
// Fulcio extension rather than anywhere a normal certificate would put it.
func issueCertificate(public *ecdsa.PublicKey, identity, issuer string) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %v", err)
	}
	uri, err := url.Parse(identity)
	if err != nil {
		return nil, fmt.Errorf("parse identity: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(certificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{uri},
		ExtraExtensions: []pkix.Extension{{
			Id:    oidOIDCIssuer,
			Value: []byte(issuer),
		}},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, intermediate, public, intermediateKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// logEntries is Rekor's entry endpoint.
func logEntries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "log entries takes POST, got %s", r.Method)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, "read log entry: %v", err)
		return
	}
	var entry struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Spec       struct {
			Data struct {
				Hash struct {
					Algorithm string `json:"algorithm"`
					Value     string `json:"value"`
				} `json:"hash"`
			} `json:"data"`
			Signature struct {
				Content   string `json:"content"`
				PublicKey struct {
					Content string `json:"content"`
				} `json:"publicKey"`
			} `json:"signature"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		fail(w, http.StatusBadRequest, "decode log entry: %v", err)
		return
	}
	if entry.Kind != "hashedrekord" {
		fail(w, http.StatusBadRequest, "log entry kind is %q, want hashedrekord", entry.Kind)
		return
	}
	if entry.APIVersion != "0.0.1" {
		fail(w, http.StatusBadRequest, "log entry apiVersion is %q, want 0.0.1", entry.APIVersion)
		return
	}
	if entry.Spec.Data.Hash.Algorithm != "sha256" {
		fail(w, http.StatusBadRequest, "log entry hash algorithm is %q, want sha256", entry.Spec.Data.Hash.Algorithm)
		return
	}
	hashed, err := hex.DecodeString(entry.Spec.Data.Hash.Value)
	if err != nil || len(hashed) != sha256.Size {
		fail(w, http.StatusBadRequest, "log entry hash value %q is not a hex SHA-256", entry.Spec.Data.Hash.Value)
		return
	}
	signature, err := base64.StdEncoding.DecodeString(entry.Spec.Signature.Content)
	if err != nil {
		fail(w, http.StatusBadRequest, "log entry signature content is not base64: %v", err)
		return
	}
	// The public key of a hashedrekord entry is the signing certificate,
	// base64 of its PEM. A real log takes either a key or a certificate here;
	// this module publishes a certificate, so that is what is required, and
	// requiring it is what pins the encoding rekorBundle writes.
	certPEM, err := base64.StdEncoding.DecodeString(entry.Spec.Signature.PublicKey.Content)
	if err != nil {
		fail(w, http.StatusBadRequest, "log entry public key content is not base64: %v", err)
		return
	}
	leaf, err := parseCertificatePEM(certPEM)
	if err != nil {
		fail(w, http.StatusBadRequest, "log entry public key is not a PEM certificate: %v", err)
		return
	}
	public, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		fail(w, http.StatusBadRequest, "log entry certificate carries a %T, want an ECDSA key", leaf.PublicKey)
		return
	}
	if err := leaf.CheckSignatureFrom(intermediate); err != nil {
		fail(w, http.StatusBadRequest, "log entry certificate was not issued by this authority: %v", err)
		return
	}
	// The entry has to be internally consistent: the signature it carries has
	// to be over the hash it carries, by the certificate it names. Without
	// this the round trip would prove only that three fields were populated.
	if !ecdsa.VerifyASN1(public, hashed, signature) {
		fail(w, http.StatusBadRequest,
			"log entry signature does not verify against the certificate it names, over the hash it names")
		return
	}

	body := base64.StdEncoding.EncodeToString(raw)
	setDigest := sha256.Sum256([]byte(body))
	set, err := ecdsa.SignASN1(rand.Reader, logKey, setDigest[:])
	if err != nil {
		fail(w, http.StatusInternalServerError, "countersign log entry: %v", err)
		return
	}
	uuid := sha256.Sum256(raw)
	respond(w, http.StatusCreated, map[string]any{
		hex.EncodeToString(uuid[:]): map[string]any{
			"body":           body,
			"integratedTime": time.Now().Unix(),
			"logID":          logID,
			"logIndex":       logIndex.Add(1),
			"verification": map[string]any{
				"signedEntryTimestamp": base64.StdEncoding.EncodeToString(set),
			},
		},
	})
}

func respond(w http.ResponseWriter, code int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		fail(w, http.StatusInternalServerError, "encode response: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// claimsOf decodes a JWT's claim set without verifying anything. The token
// this receives is minted by the suite's own endpoint with an empty
// signature, exactly as the module's own claimsOf expects.
func claimsOf(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("identity token is not a JWT: expected 3 segments, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode identity token claims: %v", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("decode identity token claims: %v", err)
	}
	return claims, nil
}

func audienceIsSigstore(claims map[string]any) bool {
	switch aud := claims["aud"].(type) {
	case string:
		return aud == "sigstore"
	case []any:
		for _, entry := range aud {
			if s, ok := entry.(string); ok && s == "sigstore" {
				return true
			}
		}
	}
	return false
}

func parseCertificatePEM(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("expected a PEM CERTIFICATE block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseECKeyPEM(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("expected a PEM block")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func parsePublicKeyPEM(raw []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("expected a PEM block, got %q", truncate(string(raw)))
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("expected a PEM PUBLIC KEY block, got %q", block.Type)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %v", err)
	}
	public, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("expected an ECDSA public key, got %T", parsed)
	}
	return public, nil
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}
