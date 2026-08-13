package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"

	"dagger/tests/internal/dagger"
)

// The annotations cosign reads a keyless signature's identity out of.
//
// Spelled here rather than imported from the module, for the same reason
// signatureTagSuffix is: they are a contract with every consumer's cosign,
// so a rename in the module has to break this test rather than agree with
// itself. The signature key's `cosignproject` spelling beside the others'
// `sigstore` one is upstream's inconsistency and copying it exactly is the
// point.
const (
	signatureAnnotation   = "dev.cosignproject.cosign/signature"
	certificateAnnotation = "dev.sigstore.cosign/certificate"
	chainAnnotation       = "dev.sigstore.cosign/chain"
	bundleAnnotation      = "dev.sigstore.cosign/bundle"
)

// fulcioIssuerOID is the X.509 extension a Fulcio certificate carries its
// OIDC issuer in, and the one `cosign verify --certificate-oidc-issuer`
// matches against.
const fulcioIssuerOID = "1.3.6.1.4.1.57264.1.1"

// sigstoreHarness is a certificate authority and a transparency log
// standing inside this session, in place of the public sigstore.
//
// It is the same move the provenance harness makes one layer down: rather
// than relax the requirement into the shape of the tests, stand up a real
// thing that behaves like the one production talks to. A CA is exactly as
// standable-up as an OIDC issuer — a key, a certificate, and an endpoint
// that signs — and standing one up is what makes every line of the keyless
// path executable.
type sigstoreHarness struct {
	// Fulcio and Rekor are the two services, which are handed to the
	// module together. They are separate services rather than one process
	// with two routes so that asking the wrong one is a 404 rather than an
	// answer.
	Fulcio *dagger.Service
	Rekor  *dagger.Service
	// Root and Intermediate are the CA the leaf will chain to, kept so a
	// test can assert which certificate landed in which annotation rather
	// than merely that both are populated.
	Root            *x509.Certificate
	RootPEM         []byte
	Intermediate    *x509.Certificate
	IntermediatePEM []byte
	// LogID is what the log reports for every entry. It is random per run
	// and asserted against the bundle annotation, which is what makes "the
	// module carried the log's answer through" a check rather than a hope.
	LogID string
}

// newSigstoreHarness generates the CA and starts both services.
func newSigstoreHarness(ctx context.Context) (*sigstoreHarness, error) {
	ca, err := newTestCertificateAuthority()
	if err != nil {
		return nil, err
	}
	logID, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (log id): %v", err)
	}
	// A separate random from the log id, and a plain environment variable:
	// services are content-addressed, so without a value that varies per
	// session the engine would hand a later run an earlier run's CA — which
	// would then be issuing against a root this run's cosign was never
	// given. The same trap localRegistry's NONCE exists for.
	nonce, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (nonce): %v", err)
	}

	// Built from source rather than run by an interpreter because issuing an
	// X.509 certificate is what this has to do, and Go's standard library
	// does it with no dependencies to install at test time.
	source := dag.CurrentModule().Source().Directory("fixtures/sigstore")
	base := dag.Go().Container(source).
		WithEnvVariable("CGO_ENABLED", "0").
		WithExec([]string{"go", "build", "-o", "/bin/fake-sigstore", "."}).
		WithEnvVariable("NONCE", nonce).
		WithEnvVariable("ROOT_CERT_PEM", string(ca.RootPEM)).
		WithEnvVariable("INTERMEDIATE_CERT_PEM", string(ca.IntermediatePEM)).
		WithEnvVariable("INTERMEDIATE_KEY_PEM", string(ca.IntermediateKeyPEM)).
		WithEnvVariable("LOG_ID", logID).
		WithExposedPort(8080)
	serve := func(role string) *dagger.Service {
		return base.WithEnvVariable("ROLE", role).
			AsService(dagger.ContainerAsServiceOpts{Args: []string{"/bin/fake-sigstore"}})
	}
	return &sigstoreHarness{
		Fulcio:          serve("fulcio"),
		Rekor:           serve("rekor"),
		Root:            ca.Root,
		RootPEM:         ca.RootPEM,
		Intermediate:    ca.Intermediate,
		IntermediatePEM: ca.IntermediatePEM,
		LogID:           logID,
	}, nil
}

// testCertificateAuthority is a two-level CA: a root, and an intermediate
// that does the issuing.
//
// Two levels rather than one, and that is not decoration. The chain the
// module splits has to have something to split — with a single self-signed
// CA the `dev.sigstore.cosign/chain` annotation would carry one certificate
// and "the leaf goes to certificate and the rest goes to chain" would be
// satisfied by a chain of one, which is the assertion passing by accident.
// A real Fulcio issues from an intermediate for the same structural reason.
type testCertificateAuthority struct {
	Root               *x509.Certificate
	RootPEM            []byte
	Intermediate       *x509.Certificate
	IntermediatePEM    []byte
	IntermediateKeyPEM []byte
}

// newTestCertificateAuthority generates that CA. Every key is generated at
// run time; nothing here is a constant a test could assert against itself.
func newTestCertificateAuthority() (*testCertificateAuthority, error) {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate root key: %v", err)
	}
	rootTemplate, err := caTemplate("z5labs test sigstore root", 1)
	if err != nil {
		return nil, err
	}
	root, rootPEM, err := signCertificate(rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("self-sign root: %v", err)
	}

	intermediateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate intermediate key: %v", err)
	}
	intermediateTemplate, err := caTemplate("z5labs test sigstore intermediate", 0)
	if err != nil {
		return nil, err
	}
	intermediate, intermediatePEM, err := signCertificate(intermediateTemplate, root, &intermediateKey.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("sign intermediate: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(intermediateKey)
	if err != nil {
		return nil, fmt.Errorf("encode intermediate key: %v", err)
	}
	return &testCertificateAuthority{
		Root:               root,
		RootPEM:            rootPEM,
		Intermediate:       intermediate,
		IntermediatePEM:    intermediatePEM,
		IntermediateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}),
	}, nil
}

// caTemplate is one signing certificate's template.
//
// No extended key usage on either CA, deliberately. Go's chain verification
// treats a CA's extended key usages as a constraint on everything beneath
// it, so a CA carrying the wrong one — or carrying one at all, when cosign
// verifies for code signing — rejects a leaf that is otherwise perfect, and
// does it with an error about key usage rather than about the CA.
func caTemplate(name string, pathLen int) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %v", err)
	}
	now := time.Now()
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            pathLen,
		MaxPathLenZero:        pathLen == 0,
	}, nil
}

// signCertificate signs template with parent's key and returns the parsed
// certificate beside its PEM.
func signCertificate(template, parent *x509.Certificate, public *ecdsa.PublicKey, signer *ecdsa.PrivateKey) (*x509.Certificate, []byte, error) {
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return parsed, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// publishableKeyless configures app for a keyless publish: the local
// registry, the credential, plain HTTP, the provenance machinery, and the
// session's sigstore.
//
// It is publishable's counterpart and differs from it in exactly one place:
// no signing key. That is what selects the keyless branch, and it is why
// this is a second function rather than an option on the first — a publish
// carrying both a key and a sigstore is refused by the module, on the
// grounds that it has asked for two different modes.
func publishableKeyless(
	app *dagger.Z5LabsApp,
	svc *dagger.Service,
	secret *dagger.Secret,
	prov *provenanceHarness,
	sig *sigstoreHarness,
) *dagger.Z5LabsApp {
	return app.
		WithRegistry(registryAlias+":5000", "ci", secret).
		WithRegistryService(svc).
		WithInsecure().
		WithOidc(prov.URL, prov.RequestToken).
		WithOidcService(prov.Service).
		WithSigstoreServices(sig.Fulcio, sig.Rekor)
}

// AppKeylessSignatureVerifiesAgainstALocalSigstore drives a publish through
// the keyless branch and verifies it with the command Publish's doc comment
// tells consumers to run: stock `cosign verify` by certificate identity and
// OIDC issuer, with no key anywhere.
//
// # What this is for
//
// Every line of the keyless path was unreachable from the suite before it:
// the certificate request and its proof of possession, the chain split, the
// log upload, and the three annotations that carry a certificate, a chain
// and a bundle. AppSignsEveryPublishedManifest covers the supplied-key mode,
// where none of those exist. So the identity half of the story — a
// certificate identity, an issuer, a log entry — had never been executed
// anywhere but a real release.
//
// # One platform, on purpose
//
// The recursive half — that the index and every per-platform manifest beneath
// it are each signed — is AppSignsEveryPublishedManifest's subject and is
// mode-independent, since signImage walks the same digests whichever signer
// it holds. This test's subject is the identity, so it builds for one
// platform and asserts the annotations of every signature the publish did
// write, whether that is one or two.
//
// # What the local sigstore cannot establish, stated rather than hidden
//
// Three flags below exist only because the sigstore is this session's:
//
//   - --ca-roots and --ca-intermediates, because the leaf chains to a root
//     generated in this process rather than to one in cosign's trust root.
//     This is the same kind of flag as --allow-http-registry: it tells
//     cosign where to look, not what to skip.
//   - --insecure-ignore-sct, because a certificate transparency log is a
//     third service this harness does not stand up, so there is no embedded
//     SCT to check.
//   - --insecure-ignore-tlog, because the bundle is countersigned by a log
//     key nobody trusts. The entry is still round-tripped through a log that
//     verifies the signature against the certificate before answering, and
//     the bundle it returned is asserted below — so what goes unchecked here
//     is a verifier's trust in the log, not the module's encoding of the
//     entry.
//
// None of them weakens what the test is about. The identity and the issuer
// are matched by cosign, from the certificate, exactly as a consumer's
// command would; the last assertion below is that the same command goes red
// when the identity is not the one that signed, which is what makes the
// green one evidence.
func (t *Tests) AppKeylessSignatureVerifiesAgainstALocalSigstore(ctx context.Context) error {
	const (
		version    = "v9.2.0"
		repository = "z5labs/keyless"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	headSha, err := headFullSha(ctx, src)
	if err != nil {
		return err
	}
	svc, password, secret, err := localRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, headSha)
	if err != nil {
		return err
	}
	sig, err := newSigstoreHarness(ctx)
	if err != nil {
		return err
	}
	// The identity and the issuer come from the claims the token endpoint
	// mints, so what cosign matches is what the CA was told — not a constant
	// this test and the harness agreed on separately.
	issuer, ok := prov.Claims["iss"].(string)
	if !ok || issuer == "" {
		return fmt.Errorf("the provenance harness minted no iss claim, so there is no issuer to verify against")
	}
	workflowRef, ok := prov.Claims["job_workflow_ref"].(string)
	if !ok || workflowRef == "" {
		return fmt.Errorf("the provenance harness minted no job_workflow_ref claim, so there is no identity to verify against")
	}
	identity := "https://github.com/" + workflowRef

	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{
		Platforms: []dagger.Platform{hostPlatform()},
	})
	refs, err := publishableKeyless(app, svc, secret, prov, sig).Publish(ctx, []string{repository})
	if err != nil {
		return fmt.Errorf("Publish: %v", err)
	}
	digest, err := digestOf(refs[0])
	if err != nil {
		return err
	}

	verifier, err := cosignKeylessVerifier(ctx, svc, password, sig)
	if err != nil {
		return err
	}
	reference := fmt.Sprintf("%s:5000/%s:%s", registryAlias, repository, version)
	// The regexp form, because it is the one Publish's doc comment gives a
	// consumer: an identity is a workflow file at a ref, and pinning the ref
	// would make every consumer's command break on a branch rename.
	identityPattern := "^https://github\\.com/z5labs/example/\\.github/workflows/"
	if err := verifier.mustVerifyKeyless(ctx, reference, identityPattern, issuer); err != nil {
		return err
	}
	// The same command, one field different. Without this the assertion above
	// could be satisfied by a cosign invocation that resolved nothing and
	// reported success, which is the failure AppSignatureDoesNotVerifyForAnotherKey
	// exists to close for the supplied-key mode.
	code, stderr, err := verifier.verifyKeyless(ctx, reference,
		"^https://github\\.com/somebody/else/\\.github/workflows/", issuer)
	if err != nil {
		return err
	}
	if code == 0 {
		return fmt.Errorf("cosign verified %s against an identity that did not sign it", reference)
	}
	const wantRejection = "none of the expected identities matched"
	if !strings.Contains(stderr, wantRejection) {
		return fmt.Errorf("cosign rejected %s but not for an identity mismatch: wanted %q in its output, got exit %d and: %s",
			reference, wantRejection, code, stderr)
	}

	registry := testRegistry(svc, secret)
	tags, err := signatureTagsFor(ctx, registry, repository, digest)
	if err != nil {
		return err
	}
	// Without this the loop below is vacuous, and a publish that wrote no
	// signature at all would satisfy every assertion in it by having nothing
	// to assert against.
	if len(tags) == 0 {
		return fmt.Errorf("the publish of %s left no signature tags, so there are no annotations to check", digest)
	}
	for _, tag := range tags {
		if err := sig.assertKeylessAnnotations(ctx, registry, repository, tag, identity, issuer); err != nil {
			return err
		}
	}
	return nil
}

// assertKeylessAnnotations checks the four annotations one keyless signature
// carries: the signature, the leaf certificate, the rest of the chain, and
// the transparency log bundle.
//
// The split between the certificate and the chain annotations is the part
// worth asserting rather than assuming, and it is why the harness keeps its
// own certificates: the leaf has to be the one the identity is in, and the
// chain has to be the intermediate and the root, in that order and nothing
// else. A lenient reading — "both annotations are non-empty" — is satisfied
// by a split that put the intermediate in front, which would publish an
// intermediate as the signing identity.
func (h *sigstoreHarness) assertKeylessAnnotations(
	ctx context.Context,
	registry *dagger.OciRegistry,
	repository, tag, identity, issuer string,
) error {
	signature, err := fetchManifest(ctx, registry, repository, tag)
	if err != nil {
		return err
	}
	if len(signature.Layers) != 1 {
		return fmt.Errorf("signature %s holds %d layers, want 1", tag, len(signature.Layers))
	}
	annotations := signature.Layers[0].Annotations
	if annotations[signatureAnnotation] == "" {
		return fmt.Errorf("signature %s carries no %s annotation", tag, signatureAnnotation)
	}

	leaves, err := certificatesIn(annotations[certificateAnnotation])
	if err != nil {
		return fmt.Errorf("signature %s: %s: %v", tag, certificateAnnotation, err)
	}
	if len(leaves) != 1 {
		return fmt.Errorf("signature %s: %s carries %d certificates, want only the leaf",
			tag, certificateAnnotation, len(leaves))
	}
	leaf := leaves[0]
	if got := uriNames(leaf); len(got) != 1 || got[0] != identity {
		return fmt.Errorf("signature %s: the certificate identifies %v, want [%s]", tag, got, identity)
	}
	if got := extensionValue(leaf, fulcioIssuerOID); got != issuer {
		return fmt.Errorf("signature %s: the certificate names issuer %q, want %q", tag, got, issuer)
	}
	if err := leaf.CheckSignatureFrom(h.Intermediate); err != nil {
		return fmt.Errorf("signature %s: the certificate was not issued by this session's authority: %v", tag, err)
	}

	chain, err := certificatesIn(annotations[chainAnnotation])
	if err != nil {
		return fmt.Errorf("signature %s: %s: %v", tag, chainAnnotation, err)
	}
	want := []*x509.Certificate{h.Intermediate, h.Root}
	if len(chain) != len(want) {
		return fmt.Errorf("signature %s: %s carries %d certificates, want the intermediate and the root",
			tag, chainAnnotation, len(chain))
	}
	for i := range want {
		if !bytes.Equal(chain[i].Raw, want[i].Raw) {
			return fmt.Errorf("signature %s: %s position %d is %q, want %q",
				tag, chainAnnotation, i, chain[i].Subject.CommonName, want[i].Subject.CommonName)
		}
	}

	return h.assertBundle(tag, annotations[bundleAnnotation])
}

// assertBundle checks the transparency log bundle the publish embedded.
//
// The field names are capitalized because cosign's bundle type spells them
// that way, and getting that wrong is invisible until a consumer's cosign
// reads the annotation and finds nothing — which is why the spelling is
// asserted here rather than left to the module to agree with itself about.
//
// The log id is the load-bearing assertion. It is minted per run and told
// only to the log, so a bundle carrying it is a bundle assembled out of what
// the log answered; a module that fabricated a well-formed bundle without
// ever reading the response would pass every other check here.
func (h *sigstoreHarness) assertBundle(tag, raw string) error {
	if raw == "" {
		return fmt.Errorf("signature %s carries no %s annotation", tag, bundleAnnotation)
	}
	var bundle struct {
		SignedEntryTimestamp string `json:"SignedEntryTimestamp"`
		Payload              struct {
			Body           string `json:"body"`
			IntegratedTime int64  `json:"integratedTime"`
			LogIndex       int64  `json:"logIndex"`
			LogID          string `json:"logID"`
		} `json:"Payload"`
	}
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		return fmt.Errorf("signature %s: decode %s: %v", tag, bundleAnnotation, err)
	}
	if bundle.SignedEntryTimestamp == "" {
		return fmt.Errorf("signature %s: the bundle carries no SignedEntryTimestamp, so nothing establishes the certificate was live when it signed", tag)
	}
	if bundle.Payload.LogID != h.LogID {
		return fmt.Errorf("signature %s: the bundle names log %q, want this session's %q",
			tag, bundle.Payload.LogID, h.LogID)
	}
	if bundle.Payload.LogIndex <= 0 || bundle.Payload.IntegratedTime <= 0 {
		return fmt.Errorf("signature %s: the bundle records index %d at time %d, want both from the log's answer",
			tag, bundle.Payload.LogIndex, bundle.Payload.IntegratedTime)
	}
	body, err := base64.StdEncoding.DecodeString(bundle.Payload.Body)
	if err != nil {
		return fmt.Errorf("signature %s: the bundle's body is not base64: %v", tag, err)
	}
	var entry struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(body, &entry); err != nil {
		return fmt.Errorf("signature %s: the bundle's body is not a log entry: %v", tag, err)
	}
	if entry.Kind != "hashedrekord" {
		return fmt.Errorf("signature %s: the bundle's body records a %q entry, want hashedrekord", tag, entry.Kind)
	}
	return nil
}

// certificatesIn parses a PEM bundle, refusing anything that is not a
// certificate and anything left over — the same strictness the module's own
// split applies, so a test reading it back cannot be more forgiving than the
// code that wrote it.
func certificatesIn(raw string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	remaining := []byte(raw)
	for {
		block, tail := pem.Decode(remaining)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("carries a %q PEM block where a CERTIFICATE was expected", block.Type)
		}
		parsed, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %v", err)
		}
		out = append(out, parsed)
		remaining = tail
	}
	if len(bytes.TrimSpace(remaining)) > 0 {
		return nil, fmt.Errorf("carries %d trailing bytes that are not a PEM block", len(bytes.TrimSpace(remaining)))
	}
	return out, nil
}

// uriNames is the certificate's subject alternative URI names, as strings.
func uriNames(cert *x509.Certificate) []string {
	out := make([]string, 0, len(cert.URIs))
	for _, uri := range cert.URIs {
		out = append(out, uri.String())
	}
	return out
}

// extensionValue reads one X.509 extension's raw value, which is how Fulcio
// stores the OIDC issuer and how cosign reads it back.
func extensionValue(cert *x509.Certificate, oid string) string {
	for _, ext := range cert.Extensions {
		if ext.Id.String() == oid {
			return string(ext.Value)
		}
	}
	return ""
}
