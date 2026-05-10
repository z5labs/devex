// Package main is the certificate-management-tests Dagger module.
package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"dagger/certificate-management-tests/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
	"software.sslmate.com/src/go-pkcs12"
)

type Tests struct{}

// All runs every certificate-management round-trip test in parallel.
//
// +check
// +cache="session"
func (t *Tests) All(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)

	jobs = jobs.WithJob("CreateCaProducesUsableKeyStore", t.CreateCaProducesUsableKeyStore)
	jobs = jobs.WithJob("LoadCertificateAuthorityRoundTrip", t.LoadCertificateAuthorityRoundTrip)
	jobs = jobs.WithJob("IssueServerCertificateChainsToCa", t.IssueServerCertificateChainsToCa)
	jobs = jobs.WithJob("IssueClientCertificateChainsToCa", t.IssueClientCertificateChainsToCa)
	jobs = jobs.WithJob("IssueMutualTlsCertificateChainsToCa", t.IssueMutualTlsCertificateChainsToCa)
	jobs = jobs.WithJob("IssueServerCertificateWithEcdsaKey", t.IssueServerCertificateWithEcdsaKey)
	jobs = jobs.WithJob("IssueServerCertificateWithEd25519Key", t.IssueServerCertificateWithEd25519Key)
	jobs = jobs.WithJob("LoadKeyStoreFromPkcs12RoundTrip", t.LoadKeyStoreFromPkcs12RoundTrip)
	jobs = jobs.WithJob("LoadTrustStoreFromPkcs12RoundTrip", t.LoadTrustStoreFromPkcs12RoundTrip)

	return jobs.Run(ctx)
}

// CreateCaProducesUsableKeyStore checks that a freshly created CA's keystore
// decodes successfully under its bound password and yields a CA-flagged
// certificate.
func (t *Tests) CreateCaProducesUsableKeyStore(ctx context.Context) error {
	pwdSecret, pwd, err := newPassword(ctx, "ca-pwd")
	if err != nil {
		return err
	}
	caKey, err := newKey(ctx, "ca-key", "rsa")
	if err != nil {
		return err
	}
	caSerial, err := newSerial()
	if err != nil {
		return err
	}
	ca := dag.CertificateManagement().CreateCertificateAuthority(nowRfc3339(), caSerial, pwdSecret, caKey)

	data, err := readPkcs12(ctx, ca.KeyStore().Pkcs12())
	if err != nil {
		return err
	}
	_, cert, err := pkcs12.Decode(data, pwd)
	if err != nil {
		return fmt.Errorf("decode CA keystore: %w", err)
	}
	if !cert.IsCA {
		return fmt.Errorf("expected CA-flagged cert, got IsCA=false")
	}
	return nil
}

// LoadCertificateAuthorityRoundTrip creates a CA, exports its keystore as a
// file, reloads it via LoadCertificateAuthority, then issues a server cert
// from the reloaded CA and verifies it chains to the original.
func (t *Tests) LoadCertificateAuthorityRoundTrip(ctx context.Context) error {
	pwdSecret, pwd, err := newPassword(ctx, "rt-ca-pwd")
	if err != nil {
		return err
	}
	caKey, err := newKey(ctx, "rt-ca-key", "rsa")
	if err != nil {
		return err
	}
	caSerial, err := newSerial()
	if err != nil {
		return err
	}
	cm := dag.CertificateManagement()
	originalCA := cm.CreateCertificateAuthority(nowRfc3339(), caSerial, pwdSecret, caKey)
	originalKeystoreFile := originalCA.KeyStore().Pkcs12()
	reloadedCA := cm.LoadCertificateAuthority(originalKeystoreFile, pwdSecret)

	leafPwdSecret, _, err := newPassword(ctx, "rt-leaf-pwd")
	if err != nil {
		return err
	}
	leafKey, err := newKey(ctx, "rt-leaf-key", "rsa")
	if err != nil {
		return err
	}
	leafSerial, err := newSerial()
	if err != nil {
		return err
	}
	issued := reloadedCA.IssueServerCertificate("example.com", nowRfc3339(), leafSerial, leafPwdSecret, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans: []string{"example.com"},
		})

	leafCert, err := readPemCert(ctx, issued.CertPemFile())
	if err != nil {
		return err
	}

	originalKsBytes, err := readPkcs12(ctx, originalKeystoreFile)
	if err != nil {
		return err
	}
	_, originalCert, err := pkcs12.Decode(originalKsBytes, pwd)
	if err != nil {
		return fmt.Errorf("decode original CA keystore: %w", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(originalCert)
	if _, err := leafCert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("leaf does not chain to original CA: %w", err)
	}
	return nil
}

func (t *Tests) IssueServerCertificateChainsToCa(ctx context.Context) error {
	return verifyIssued(ctx, "server", "rsa",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		func(ca *dagger.CertificateManagementCertificateAuthority, leafNB, leafSerial string, leafPwd, leafKey *dagger.Secret) *dagger.CertificateManagementIssuedCertificate {
			return ca.IssueServerCertificate("server.example.com", leafNB, leafSerial, leafPwd, leafKey,
				dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
					DNSSans: []string{"server.example.com"},
				})
		})
}

func (t *Tests) IssueClientCertificateChainsToCa(ctx context.Context) error {
	return verifyIssued(ctx, "client", "rsa",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		func(ca *dagger.CertificateManagementCertificateAuthority, leafNB, leafSerial string, leafPwd, leafKey *dagger.Secret) *dagger.CertificateManagementIssuedCertificate {
			return ca.IssueClientCertificate("client", leafNB, leafSerial, leafPwd, leafKey)
		})
}

func (t *Tests) IssueMutualTlsCertificateChainsToCa(ctx context.Context) error {
	return verifyIssued(ctx, "mtls", "rsa",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		func(ca *dagger.CertificateManagementCertificateAuthority, leafNB, leafSerial string, leafPwd, leafKey *dagger.Secret) *dagger.CertificateManagementIssuedCertificate {
			return ca.IssueMutualTLSCertificate("peer.example.com", leafNB, leafSerial, leafPwd, leafKey,
				dagger.CertificateManagementCertificateAuthorityIssueMutualTLSCertificateOpts{
					DNSSans: []string{"peer.example.com"},
				})
		})
}

// IssueServerCertificateWithEcdsaKey exercises the caller-chosen-algorithm
// capability by signing the CA and leaf with ECDSA P-256 keys.
func (t *Tests) IssueServerCertificateWithEcdsaKey(ctx context.Context) error {
	return verifyIssued(ctx, "ecdsa-server", "ecdsa",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		func(ca *dagger.CertificateManagementCertificateAuthority, leafNB, leafSerial string, leafPwd, leafKey *dagger.Secret) *dagger.CertificateManagementIssuedCertificate {
			return ca.IssueServerCertificate("ecdsa.example.com", leafNB, leafSerial, leafPwd, leafKey,
				dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
					DNSSans: []string{"ecdsa.example.com"},
				})
		})
}

// IssueServerCertificateWithEd25519Key exercises the caller-chosen-algorithm
// capability by signing the CA and leaf with Ed25519 keys.
func (t *Tests) IssueServerCertificateWithEd25519Key(ctx context.Context) error {
	return verifyIssued(ctx, "ed25519-server", "ed25519",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		func(ca *dagger.CertificateManagementCertificateAuthority, leafNB, leafSerial string, leafPwd, leafKey *dagger.Secret) *dagger.CertificateManagementIssuedCertificate {
			return ca.IssueServerCertificate("ed25519.example.com", leafNB, leafSerial, leafPwd, leafKey,
				dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
					DNSSans: []string{"ed25519.example.com"},
				})
		})
}

// LoadKeyStoreFromPkcs12RoundTrip exercises LoadKeyStoreFromPkcs12 by
// re-wrapping an issued cert's keystore and asserting its PKCS#12 still
// decodes with the original password.
func (t *Tests) LoadKeyStoreFromPkcs12RoundTrip(ctx context.Context) error {
	caPwdSecret, _, err := newPassword(ctx, "lks-ca-pwd")
	if err != nil {
		return err
	}
	caKey, err := newKey(ctx, "lks-ca-key", "rsa")
	if err != nil {
		return err
	}
	caSerial, err := newSerial()
	if err != nil {
		return err
	}
	leafPwdSecret, leafPwd, err := newPassword(ctx, "lks-leaf-pwd")
	if err != nil {
		return err
	}
	leafKey, err := newKey(ctx, "lks-leaf-key", "rsa")
	if err != nil {
		return err
	}
	leafSerial, err := newSerial()
	if err != nil {
		return err
	}

	cm := dag.CertificateManagement()
	ca := cm.CreateCertificateAuthority(nowRfc3339(), caSerial, caPwdSecret, caKey)
	issued := ca.IssueServerCertificate("round.example.com", nowRfc3339(), leafSerial, leafPwdSecret, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans: []string{"round.example.com"},
		})

	wrapped := cm.LoadKeyStoreFromPkcs12(issued.KeyStore().Pkcs12(), leafPwdSecret)
	data, err := readPkcs12(ctx, wrapped.Pkcs12())
	if err != nil {
		return err
	}
	if _, _, _, err := pkcs12.DecodeChain(data, leafPwd); err != nil {
		return fmt.Errorf("decode loaded keystore: %w", err)
	}
	return nil
}

// LoadTrustStoreFromPkcs12RoundTrip exercises LoadTrustStoreFromPkcs12 by
// re-wrapping a CA's truststore.
func (t *Tests) LoadTrustStoreFromPkcs12RoundTrip(ctx context.Context) error {
	caPwdSecret, caPwd, err := newPassword(ctx, "lts-ca-pwd")
	if err != nil {
		return err
	}
	caKey, err := newKey(ctx, "lts-ca-key", "rsa")
	if err != nil {
		return err
	}
	caSerial, err := newSerial()
	if err != nil {
		return err
	}
	cm := dag.CertificateManagement()
	ca := cm.CreateCertificateAuthority(nowRfc3339(), caSerial, caPwdSecret, caKey)

	wrapped := cm.LoadTrustStoreFromPkcs12(ca.TrustStore().Pkcs12(), caPwdSecret)
	data, err := readPkcs12(ctx, wrapped.Pkcs12())
	if err != nil {
		return err
	}
	certs, err := pkcs12.DecodeTrustStore(data, caPwd)
	if err != nil {
		return fmt.Errorf("decode loaded truststore: %w", err)
	}
	if len(certs) == 0 {
		return fmt.Errorf("expected at least one trusted cert, got 0")
	}
	if !certs[0].IsCA {
		return fmt.Errorf("expected trusted cert to be CA-flagged")
	}
	return nil
}

func verifyIssued(
	ctx context.Context,
	label string,
	keyKind string,
	requireEKU []x509.ExtKeyUsage,
	issue func(*dagger.CertificateManagementCertificateAuthority, string, string, *dagger.Secret, *dagger.Secret) *dagger.CertificateManagementIssuedCertificate,
) error {
	caPwdSecret, _, err := newPassword(ctx, label+"-ca-pwd")
	if err != nil {
		return err
	}
	caKey, err := newKey(ctx, label+"-ca-key", keyKind)
	if err != nil {
		return err
	}
	caSerial, err := newSerial()
	if err != nil {
		return err
	}
	leafPwdSecret, leafPwd, err := newPassword(ctx, label+"-leaf-pwd")
	if err != nil {
		return err
	}
	leafKey, err := newKey(ctx, label+"-leaf-key", keyKind)
	if err != nil {
		return err
	}
	leafSerial, err := newSerial()
	if err != nil {
		return err
	}

	ca := dag.CertificateManagement().CreateCertificateAuthority(nowRfc3339(), caSerial, caPwdSecret, caKey)
	issued := issue(ca, nowRfc3339(), leafSerial, leafPwdSecret, leafKey)

	leafCert, err := readPemCert(ctx, issued.CertPemFile())
	if err != nil {
		return err
	}

	tsBytes, err := readPkcs12(ctx, issued.TrustStore().Pkcs12())
	if err != nil {
		return err
	}
	roots, err := pkcs12.DecodeTrustStore(tsBytes, leafPwd)
	if err != nil {
		return fmt.Errorf("decode issued truststore: %w", err)
	}
	if len(roots) == 0 {
		return fmt.Errorf("expected truststore to contain CA cert, got empty")
	}

	pool := x509.NewCertPool()
	for _, c := range roots {
		pool.AddCert(c)
	}

	for _, ku := range requireEKU {
		if _, err := leafCert.Verify(x509.VerifyOptions{
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{ku},
		}); err != nil {
			return fmt.Errorf("%s: chain validation for EKU %v failed: %w", label, ku, err)
		}
	}

	if !hasAllEKUs(leafCert.ExtKeyUsage, requireEKU) {
		return fmt.Errorf("%s: leaf EKUs %v missing required %v", label, leafCert.ExtKeyUsage, requireEKU)
	}
	return nil
}

func hasAllEKUs(have, want []x509.ExtKeyUsage) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// newPassword mints a fresh PKCS#12 password by hashing 32 random bytes via
// the random module and wrapping the resulting hex string as a Dagger secret.
// Returns the secret (for passing back into the certificate-management API)
// and its plaintext (for in-process verification). The secret name uses an
// independent random suffix so password material never appears in logs.
func newPassword(ctx context.Context, name string) (*dagger.Secret, string, error) {
	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("generate password: %w", err)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return nil, "", err
	}
	return dag.SetSecret(name+"-"+suffix, pwdHex), pwdHex, nil
}

// newKey mints a fresh PKCS#8 PEM private key via the crypto module and
// wraps it as a *dagger.Secret. The kind argument selects the algorithm
// ("rsa", "ecdsa", or "ed25519"). PEM is text, so File.Contents() is safe
// here (the binary-corruption concern only applies to PKCS#12 archives).
func newKey(ctx context.Context, name, kind string) (*dagger.Secret, error) {
	var pemFile *dagger.File
	switch kind {
	case "rsa":
		pemFile = dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem()
	case "ecdsa":
		pemFile = dag.Crypto().GenerateEcdsaP256Key().Pem()
	case "ed25519":
		pemFile = dag.Crypto().GenerateEd25519Key().Pem()
	default:
		return nil, fmt.Errorf("unknown key kind %q", kind)
	}
	contents, err := pemFile.Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read generated key: %w", err)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(name+"-"+suffix, contents), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random hex: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// nowRfc3339 returns the current UTC time in RFC3339 form, suitable for
// passing to CreateCertificateAuthority / Issue* as the notBefore input.
// Using a fresh value per call also acts as the cache-busting input that
// signs with truly deterministic templates.
func nowRfc3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// newSerial returns a fresh 128-bit hex-encoded serial number.
func newSerial() (string, error) {
	return randomHex(16)
}

// readPkcs12 round-trips a Dagger file through the module runtime container's
// scratch directory: Export materializes the bytes on local disk, then we
// read them with os.ReadFile. This is required because PKCS#12 archives are
// arbitrary binary; reading File.Contents() directly would force the bytes
// through a GraphQL String and corrupt non-UTF-8 sequences.
func readPkcs12(ctx context.Context, f *dagger.File) ([]byte, error) {
	suffix, err := randomHex(12)
	if err != nil {
		return nil, err
	}
	local := "p12-" + suffix + ".bin"
	if _, err := f.Export(ctx, local); err != nil {
		return nil, fmt.Errorf("export pkcs12 file: %w", err)
	}
	defer os.Remove(local)
	return os.ReadFile(local)
}

func readPemCert(ctx context.Context, f *dagger.File) (*x509.Certificate, error) {
	contents, err := f.Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read cert file: %w", err)
	}
	block, _ := pem.Decode([]byte(contents))
	if block == nil {
		return nil, fmt.Errorf("no PEM block in certificate file")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}
