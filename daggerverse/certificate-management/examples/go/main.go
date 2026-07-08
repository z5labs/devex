// Package main is the certificate-management-examples Dagger module: a runnable
// cookbook of certificate-management recipes. Each recipe wires the pure-signer
// certificate-management module to fresh key material (crypto) and fresh
// passwords/serials (random), and returns a PKCS#12 keystore you can export.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"dagger/certificate-management-examples/internal/dagger"
)

// CertificateManagementExamples is the module's main object: a namespace for
// the certificate-management usage recipes.
type CertificateManagementExamples struct{}

// IssueServerCertificate creates a fresh root CA and signs a TLS server leaf
// for "server.example.com" (with a DNS SAN), returning the leaf's PKCS#12
// keystore. This is the create-CA -> issue-server -> export path.
//
// +cache="never"
func (m *CertificateManagementExamples) IssueServerCertificate(ctx context.Context) (*dagger.File, error) {
	now := nowRfc3339()

	caPwd, err := newPassword(ctx, "server-ca-pwd")
	if err != nil {
		return nil, err
	}
	caKey, err := newKey(ctx, "server-ca-key")
	if err != nil {
		return nil, err
	}
	caSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, err
	}

	leafPwd, err := newPassword(ctx, "server-leaf-pwd")
	if err != nil {
		return nil, err
	}
	leafKey, err := newKey(ctx, "server-leaf-key")
	if err != nil {
		return nil, err
	}
	leafSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, err
	}

	ca := dag.CertificateManagement().CreateCertificateAuthority(now, caSerial, caPwd, caKey)
	server := ca.IssueServerCertificate("server.example.com", now, leafSerial, leafPwd, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans: []string{"server.example.com"},
		})

	return server.KeyStore().Pkcs12(), nil
}

// IssueClientCertificate creates a fresh root CA and signs a TLS client leaf
// for "client.example.com", returning the leaf's PKCS#12 keystore. Client
// certs carry no SANs, so the issue call takes no opts.
//
// +cache="never"
func (m *CertificateManagementExamples) IssueClientCertificate(ctx context.Context) (*dagger.File, error) {
	now := nowRfc3339()

	caPwd, err := newPassword(ctx, "client-ca-pwd")
	if err != nil {
		return nil, err
	}
	caKey, err := newKey(ctx, "client-ca-key")
	if err != nil {
		return nil, err
	}
	caSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, err
	}

	leafPwd, err := newPassword(ctx, "client-leaf-pwd")
	if err != nil {
		return nil, err
	}
	leafKey, err := newKey(ctx, "client-leaf-key")
	if err != nil {
		return nil, err
	}
	leafSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, err
	}

	ca := dag.CertificateManagement().CreateCertificateAuthority(now, caSerial, caPwd, caKey)
	client := ca.IssueClientCertificate("client.example.com", now, leafSerial, leafPwd, leafKey)

	return client.KeyStore().Pkcs12(), nil
}

// IssueMutualTlsCertificate creates a fresh root CA and signs a dual-EKU
// (serverAuth + clientAuth) leaf for "service.example.com", suitable for mTLS,
// returning the leaf's PKCS#12 keystore.
//
// +cache="never"
func (m *CertificateManagementExamples) IssueMutualTlsCertificate(ctx context.Context) (*dagger.File, error) {
	now := nowRfc3339()

	caPwd, err := newPassword(ctx, "mtls-ca-pwd")
	if err != nil {
		return nil, err
	}
	caKey, err := newKey(ctx, "mtls-ca-key")
	if err != nil {
		return nil, err
	}
	caSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, err
	}

	leafPwd, err := newPassword(ctx, "mtls-leaf-pwd")
	if err != nil {
		return nil, err
	}
	leafKey, err := newKey(ctx, "mtls-leaf-key")
	if err != nil {
		return nil, err
	}
	leafSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, err
	}

	ca := dag.CertificateManagement().CreateCertificateAuthority(now, caSerial, caPwd, caKey)
	// Source method is IssueMutualTlsCertificate; the generated binding is
	// IssueMutualTLSCertificate (uppercase TLS).
	mtls := ca.IssueMutualTLSCertificate("service.example.com", now, leafSerial, leafPwd, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueMutualTLSCertificateOpts{
			DNSSans: []string{"service.example.com"},
		})

	return mtls.KeyStore().Pkcs12(), nil
}

// RoundTripCaThroughPkcs12 creates a CA, exports its keystore, reloads the CA
// from that PKCS#12 with the SAME password, then issues a fresh server leaf
// from the reloaded CA and returns the leaf keystore. Demonstrates that a CA
// survives a serialize/deserialize round-trip and can still sign.
//
// +cache="never"
func (m *CertificateManagementExamples) RoundTripCaThroughPkcs12(ctx context.Context) (*dagger.File, error) {
	now := nowRfc3339()

	caPwd, err := newPassword(ctx, "rt-ca-pwd")
	if err != nil {
		return nil, err
	}
	caKey, err := newKey(ctx, "rt-ca-key")
	if err != nil {
		return nil, err
	}
	caSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, err
	}

	leafPwd, err := newPassword(ctx, "rt-leaf-pwd")
	if err != nil {
		return nil, err
	}
	leafKey, err := newKey(ctx, "rt-leaf-key")
	if err != nil {
		return nil, err
	}
	leafSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, err
	}

	cm := dag.CertificateManagement()
	ca := cm.CreateCertificateAuthority(now, caSerial, caPwd, caKey)
	caPkcs12 := ca.KeyStore().Pkcs12()

	// Reload MUST use the same password the CA keystore was bound to.
	reloaded := cm.LoadCertificateAuthority(caPkcs12, caPwd)

	server := reloaded.IssueServerCertificate("server.example.com", now, leafSerial, leafPwd, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans: []string{"server.example.com"},
		})

	return server.KeyStore().Pkcs12(), nil
}

// newPassword mints a fresh PKCS#12 password by hashing random bytes via the
// random module and wrapping the hex as a Dagger secret. The secret name gets
// an INDEPENDENT random suffix so password material never leaks into the name.
func newPassword(ctx context.Context, name string) (*dagger.Secret, error) {
	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(name+"-"+suffix, pwdHex), nil
}

// newKey mints a fresh RSA-2048 PKCS#8 PEM private key via the crypto module
// and wraps it as a *dagger.Secret. PEM is text, so File.Contents() is safe.
func newKey(ctx context.Context, name string) (*dagger.Secret, error) {
	pemFile := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem()
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

// randomHex returns 2n hex chars of crypto-random data, used only to make
// secret names unique across invocations.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random hex: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// nowRfc3339 returns the current UTC time in RFC3339 form, for the notBefore
// input to CreateCertificateAuthority / Issue*.
func nowRfc3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
