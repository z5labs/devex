package main

import (
	"context"
	"crypto/rand"
	rsaPkg "crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dagger/envoy/internal/dagger"

	"software.sslmate.com/src/go-pkcs12"
)

// ServerSecurity describes how an Envoy listener authenticates and
// encrypts traffic from downstream clients. Plaintext is the default;
// TLS and mTLS modes carry the PKI material needed to terminate TLS
// on the listener and (for mTLS) verify client certificates.
type ServerSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	CaKeyStore *dagger.File // PKCS#12 of the CA used to mint per-listener server leaves (TLS + MTLS)
	// +private
	CaKeyStorePassword *dagger.Secret
	// +private
	ClientTrustStore *dagger.File // PKCS#12 of CA(s) trusted to sign incoming client certs (MTLS only)
	// +private
	ClientTrustStorePassword *dagger.Secret
}

// UpstreamSecurity describes how an Envoy cluster authenticates and
// encrypts traffic to upstream endpoints. Plaintext is the default;
// TLS validates the upstream's server cert; mTLS additionally
// presents a client leaf to the upstream.
type UpstreamSecurity struct {
	// +private
	Mode string // PLAINTEXT | TLS | MTLS
	// +private
	TrustStore *dagger.File // PKCS#12 of CA(s) the cluster trusts on the upstream side (TLS + MTLS)
	// +private
	TrustStorePassword *dagger.Secret
	// +private
	KeyStore *dagger.File // PKCS#12 of Envoy's own client leaf for the upstream (MTLS only)
	// +private
	KeyStorePassword *dagger.Secret
}

// PlaintextServerSecurity returns a ServerSecurity profile configured
// for unencrypted, unauthenticated traffic.
func (e *Envoy) PlaintextServerSecurity() *ServerSecurity {
	return &ServerSecurity{Mode: "PLAINTEXT"}
}

// TlsServerSecurity returns a ServerSecurity profile that terminates
// TLS on the listener. caKeyStore is a PKCS#12 archive containing the
// CA cert + private key used to mint a per-listener leaf certificate.
func (e *Envoy) TlsServerSecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
) *ServerSecurity {
	return &ServerSecurity{
		Mode:               "TLS",
		CaKeyStore:         caKeyStore,
		CaKeyStorePassword: caKeyStorePassword,
	}
}

// MtlsServerSecurity returns a ServerSecurity profile that terminates
// mTLS on the listener. caKeyStore signs the per-listener server
// leaf; clientTrustStore holds the CA(s) the listener will accept
// incoming client certs from (asymmetric trust is allowed).
func (e *Envoy) MtlsServerSecurity(
	caKeyStore *dagger.File,
	caKeyStorePassword *dagger.Secret,
	clientTrustStore *dagger.File,
	clientTrustStorePassword *dagger.Secret,
) *ServerSecurity {
	return &ServerSecurity{
		Mode:                     "MTLS",
		CaKeyStore:               caKeyStore,
		CaKeyStorePassword:       caKeyStorePassword,
		ClientTrustStore:         clientTrustStore,
		ClientTrustStorePassword: clientTrustStorePassword,
	}
}

// PlaintextUpstreamSecurity returns an UpstreamSecurity profile
// configured for unencrypted, unauthenticated cluster traffic.
func (e *Envoy) PlaintextUpstreamSecurity() *UpstreamSecurity {
	return &UpstreamSecurity{Mode: "PLAINTEXT"}
}

// TlsUpstreamSecurity returns an UpstreamSecurity profile that opens
// a TLS connection to upstreams. trustStore is a PKCS#12 archive of
// the CA(s) used to verify the upstream's server cert.
func (e *Envoy) TlsUpstreamSecurity(
	trustStore *dagger.File,
	trustStorePassword *dagger.Secret,
) *UpstreamSecurity {
	return &UpstreamSecurity{
		Mode:               "TLS",
		TrustStore:         trustStore,
		TrustStorePassword: trustStorePassword,
	}
}

// MtlsUpstreamSecurity returns an UpstreamSecurity profile that opens
// an mTLS connection to upstreams: the upstream's server cert is
// validated against trustStore, and Envoy presents its own client
// leaf from keyStore.
func (e *Envoy) MtlsUpstreamSecurity(
	keyStore *dagger.File,
	keyStorePassword *dagger.Secret,
	trustStore *dagger.File,
	trustStorePassword *dagger.Secret,
) *UpstreamSecurity {
	return &UpstreamSecurity{
		Mode:               "MTLS",
		TrustStore:         trustStore,
		TrustStorePassword: trustStorePassword,
		KeyStore:           keyStore,
		KeyStorePassword:   keyStorePassword,
	}
}

// effectiveServerMode normalizes a possibly-nil ServerSecurity into a
// concrete mode string. nil and PLAINTEXT both render as "PLAINTEXT".
func effectiveServerMode(sec *ServerSecurity) string {
	if sec == nil {
		return "PLAINTEXT"
	}
	return sec.Mode
}

// effectiveUpstreamMode normalizes a possibly-nil UpstreamSecurity
// into a concrete mode string.
func effectiveUpstreamMode(up *UpstreamSecurity) string {
	if up == nil {
		return "PLAINTEXT"
	}
	return up.Mode
}

// envoySecretsDir is where every PKCS#12 keystore and trust PEM gets
// mounted inside the running envoy container.
const envoySecretsDir = "/etc/envoy/secrets"

func listenerCertPath(name string) string {
	return filepath.Join(envoySecretsDir, "listener-"+name+".crt")
}

func listenerKeyPath(name string) string {
	return filepath.Join(envoySecretsDir, "listener-"+name+".key")
}

func listenerTrustPath(name string) string {
	return filepath.Join(envoySecretsDir, "listener-"+name+"-trust.pem")
}

func upstreamCertPath(name string) string {
	return filepath.Join(envoySecretsDir, "upstream-"+name+".crt")
}

func upstreamKeyPath(name string) string {
	return filepath.Join(envoySecretsDir, "upstream-"+name+".key")
}

func upstreamTrustPath(name string) string {
	return filepath.Join(envoySecretsDir, "upstream-"+name+"-trust.pem")
}

// renderDownstreamTransportSocket returns the transport_socket block
// to embed under a listener's filter_chain for the given security
// mode. Returns nil for PLAINTEXT.
func renderDownstreamTransportSocket(listenerName, mode string) map[string]any {
	if mode != "TLS" && mode != "MTLS" {
		return nil
	}
	common := map[string]any{
		"tls_certificates": []any{
			map[string]any{
				"certificate_chain": map[string]any{
					"filename": listenerCertPath(listenerName),
				},
				"private_key": map[string]any{
					"filename": listenerKeyPath(listenerName),
				},
			},
		},
	}
	typed := map[string]any{
		"@type":              "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext",
		"common_tls_context": common,
	}
	if mode == "MTLS" {
		common["validation_context"] = map[string]any{
			"trusted_ca": map[string]any{
				"filename": listenerTrustPath(listenerName),
			},
		}
		typed["require_client_certificate"] = true
	}
	return map[string]any{
		"name":         "envoy.transport_sockets.tls",
		"typed_config": typed,
	}
}

// renderUpstreamTransportSocket returns the transport_socket block to
// embed under a cluster for the given upstream security mode. Returns
// nil for PLAINTEXT.
func renderUpstreamTransportSocket(clusterName, mode string) map[string]any {
	if mode != "TLS" && mode != "MTLS" {
		return nil
	}
	common := map[string]any{
		"validation_context": map[string]any{
			"trusted_ca": map[string]any{
				"filename": upstreamTrustPath(clusterName),
			},
		},
	}
	if mode == "MTLS" {
		common["tls_certificates"] = []any{
			map[string]any{
				"certificate_chain": map[string]any{
					"filename": upstreamCertPath(clusterName),
				},
				"private_key": map[string]any{
					"filename": upstreamKeyPath(clusterName),
				},
			},
		}
	}
	typed := map[string]any{
		"@type":              "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext",
		"common_tls_context": common,
	}
	return map[string]any{
		"name":         "envoy.transport_sockets.tls",
		"typed_config": typed,
	}
}

// mintListenerLeaf signs a per-listener server leaf certificate from
// the caller-supplied CA, extracts the cert + chain as a single PEM
// file and the private key as a plain PEM file (suitable for Envoy's
// `certificate_chain.filename` + `private_key.filename` data
// sources). Returns (nil, nil, nil) for PLAINTEXT mode.
func mintListenerLeaf(ctx context.Context, listenerName string, sec *ServerSecurity) (*dagger.File, *dagger.File, error) {
	if sec == nil || sec.Mode == "PLAINTEXT" {
		return nil, nil, nil
	}
	if sec.CaKeyStore == nil || sec.CaKeyStorePassword == nil {
		return nil, nil, fmt.Errorf("listener %q: %s mode requires CaKeyStore + CaKeyStorePassword", listenerName, sec.Mode)
	}

	leafKeyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listener %q: generate leaf key: %w", listenerName, err)
	}
	leafKey := dag.SetSecret("envoy-listener-leaf-key-"+randSuffix(), leafKeyPem)

	leafPwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listener %q: generate leaf password: %w", listenerName, err)
	}
	leafPwd := dag.SetSecret("envoy-listener-leaf-pwd-"+randSuffix(), leafPwdHex)

	leafSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listener %q: generate leaf serial: %w", listenerName, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)

	ca := dag.CertificateManagement().LoadCertificateAuthority(sec.CaKeyStore, sec.CaKeyStorePassword)
	issued := ca.IssueServerCertificate(listenerName, nb, leafSerial, leafPwd, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans:      []string{listenerName, "localhost", "envoy"},
			IPSans:       []string{"127.0.0.1"},
			ValidityDays: 365,
		})

	// Use cert-mgmt's PEM accessors directly: CertPemFile() returns
	// the leaf cert; PrivateKeyPem() returns the leaf's private key
	// secret (same one we just minted). Concatenate the issuer cert
	// from IssuerCertFile() for the chain.
	certPem, err := buildCertChainPem(ctx, "listener-"+listenerName, issued.CertPemFile(), issued.IssuerCertFile())
	if err != nil {
		return nil, nil, fmt.Errorf("listener %q: %w", listenerName, err)
	}
	keyPem, err := pkcs8SecretToPkcs1File(ctx, "listener-"+listenerName+"-key", issued.PrivateKeyPem())
	if err != nil {
		return nil, nil, fmt.Errorf("listener %q: %w", listenerName, err)
	}
	return certPem, keyPem, nil
}

// pkcs8SecretToPkcs1File reads a PKCS#8 PEM private key out of a
// Dagger Secret and rewrites it as a PKCS#1 ("RSA PRIVATE KEY") PEM
// file. Envoy's BoringSSL build is finicky about PKCS#8 PEM
// envelopes and reliably reads PKCS#1.
func pkcs8SecretToPkcs1File(ctx context.Context, label string, sec *dagger.Secret) (*dagger.File, error) {
	plaintext, err := sec.Plaintext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s secret: %w", label, err)
	}
	block, _ := pem.Decode([]byte(plaintext))
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block in private key secret", label)
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse pkcs8: %w", label, err)
	}
	rsa, ok := priv.(*rsaPkg.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected key type %T (expected *rsa.PrivateKey)", label, priv)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsa)})
	return writeBinaryWorkdirFile(label, "leaf.key", out)
}

// buildCertChainPem concatenates the leaf cert and issuer cert PEM
// files into a single chain file suitable for Envoy's
// `certificate_chain.filename` data source.
func buildCertChainPem(ctx context.Context, label string, leaf, issuer *dagger.File) (*dagger.File, error) {
	leafBytes, err := dagFileBytes(ctx, leaf)
	if err != nil {
		return nil, fmt.Errorf("export %s leaf cert: %w", label, err)
	}
	issuerBytes, err := dagFileBytes(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("export %s issuer cert: %w", label, err)
	}
	combined := append(append([]byte{}, leafBytes...), issuerBytes...)
	return writeBinaryWorkdirFile(label+"-cert", "leaf.crt", combined)
}

// extractUpstreamLeafPemFromPkcs12 unseals a caller-supplied PKCS#12
// client keystore and emits a PEM cert chain (leaf + chain) and a
// PKCS#1 ("RSA PRIVATE KEY") PEM private key for Envoy's upstream
// tls_certificates data sources — BoringSSL reads PKCS#1 reliably
// where PKCS#8 envelopes sometimes fail.
func extractUpstreamLeafPemFromPkcs12(ctx context.Context, label string, p12 *dagger.File, password *dagger.Secret) (*dagger.File, *dagger.File, error) {
	data, err := dagFileBytes(ctx, p12)
	if err != nil {
		return nil, nil, fmt.Errorf("export %s pkcs12: %w", label, err)
	}
	pwd, err := password.Plaintext(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s pkcs12 password: %w", label, err)
	}
	priv, leaf, chain, err := pkcs12.DecodeChain(data, pwd)
	if err != nil {
		return nil, nil, fmt.Errorf("decode %s pkcs12: %w", label, err)
	}
	var certPemBuf []byte
	certPemBuf = append(certPemBuf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})...)
	for _, c := range chain {
		certPemBuf = append(certPemBuf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	certFile, err := writeBinaryWorkdirFile(label+"-cert", "leaf.crt", certPemBuf)
	if err != nil {
		return nil, nil, fmt.Errorf("stage %s cert pem: %w", label, err)
	}
	rsaKey, ok := priv.(*rsaPkg.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("%s: unexpected key type %T (expected *rsa.PrivateKey)", label, priv)
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})
	keyFile, err := writeBinaryWorkdirFile(label+"-key", "leaf.key", keyPem)
	if err != nil {
		return nil, nil, fmt.Errorf("stage %s key pem: %w", label, err)
	}
	return certFile, keyFile, nil
}

// extractCaPemFromPkcs12 unseals the given PKCS#12 truststore and
// re-encodes its CA certificate(s) as a single PEM file, returned as
// a *dagger.File suitable for mounting at a fixed in-container path.
func extractCaPemFromPkcs12(ctx context.Context, label string, p12 *dagger.File, password *dagger.Secret) (*dagger.File, error) {
	data, err := dagFileBytes(ctx, p12)
	if err != nil {
		return nil, fmt.Errorf("export %s pkcs12: %w", label, err)
	}
	pwd, err := password.Plaintext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s pkcs12 password: %w", label, err)
	}
	rootCerts, err := pkcs12.DecodeTrustStore(data, pwd)
	if err != nil {
		return nil, fmt.Errorf("decode %s truststore: %w", label, err)
	}
	if len(rootCerts) == 0 {
		return nil, fmt.Errorf("decode %s truststore: archive contained no certificates", label)
	}
	var buf []byte
	for _, c := range rootCerts {
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return writeBinaryWorkdirFile(label, "trust.pem", buf)
}

// dagFileBytes materializes a *dagger.File via Export + ReadFile.
// Used for binary content (PKCS#12 archives) where File.Contents()
// would corrupt non-UTF-8 bytes when round-tripped through GraphQL's
// String type.
func dagFileBytes(ctx context.Context, f *dagger.File) ([]byte, error) {
	local := "envoy-tls-in-" + randSuffix()
	if _, err := f.Export(ctx, local); err != nil {
		return nil, fmt.Errorf("export file: %w", err)
	}
	defer os.Remove(local)
	return os.ReadFile(local)
}

// writeBinaryWorkdirFile writes content into a content-addressed
// subdir of the envoy module's scratch workdir and returns it as a
// *dagger.File. Identical content collapses to the same path so
// re-entry is idempotent.
func writeBinaryWorkdirFile(label, name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "envoy-" + label + "-" + hex.EncodeToString(sum[:8])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("rename to %s: %w", path, err)
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}

// randSuffix returns a fresh hex suffix for naming Dagger secrets
// uniquely across concurrent helper calls.
func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("randSuffix: %v", err))
	}
	return hex.EncodeToString(b[:])
}

