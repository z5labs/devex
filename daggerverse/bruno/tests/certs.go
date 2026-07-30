package main

import (
	"context"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"
)

// Certificate helpers for the TLS round-trips. Every key, password and serial is
// minted at test time through the crypto, random and certificate-management
// modules: no PEM literal enters git, not even for a service that exists for the
// length of one test.
//
// Ported from daggerverse/skill-gen/tests/certs.go, which took them from
// daggerverse/postgres/tests.

// tlsAssets is one test's certificate material: the CA the collection verifies
// the responder against, the leaf the responder presents, and the leaf the
// collection presents back under mTLS.
type tlsAssets struct {
	CaCert     *dagger.File
	ServerCert *dagger.File
	ServerKey  *dagger.Secret
	ClientCert *dagger.File
	ClientKey  *dagger.Secret
}

// newTlsAssets mints a per-test CA and issues both leaves off it. host is the
// name the collection dials, and therefore the SAN the server certificate needs:
// Node checks it against the requested hostname once a CA is in play.
//
// clientCn is the Common Name the client leaf carries. The responder reports it
// back, which is what lets the mTLS test assert the certificate was actually
// presented rather than that the handshake merely succeeded.
func newTlsAssets(ctx context.Context, label, host, clientCn string) (*tlsAssets, error) {
	ca, err := freshCa(ctx, label)
	if err != nil {
		return nil, err
	}
	serverCert, serverKey, err := issueServerCert(ctx, ca, host, label)
	if err != nil {
		return nil, err
	}
	clientCert, clientKey, err := issueClientCert(ctx, ca, clientCn, label)
	if err != nil {
		return nil, err
	}
	return &tlsAssets{
		CaCert:     ca.CertPemFile(),
		ServerCert: serverCert,
		ServerKey:  serverKey,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	}, nil
}

// encryptedKey re-exports a PEM private key encrypted under passphrase, so the
// optional half of WithClientCert has a key that actually needs one.
//
// The certificate-management issuers hand back an unencrypted key and the crypto
// module generates one, so neither can produce this. Node's own crypto can, and
// it is already in the image — no second image, and no openssl, which this one
// does not ship.
//
// The result travels back as a file read rather than on stdout: a container's
// output is in the trace, and an encrypted key is still key material.
func encryptedKey(ctx context.Context, label string, key, passphrase *dagger.Secret) (*dagger.Secret, error) {
	const (
		plainPath = "/keys/plain.pem"
		encPath   = "/tmp/encrypted.pem"
	)
	script := fmt.Sprintf(`
const crypto = require('crypto');
const fs = require('fs');
fs.writeFileSync(%q, crypto.createPrivateKey(fs.readFileSync(%q)).export({
  type: 'pkcs8',
  format: 'pem',
  cipher: 'aes-256-cbc',
  passphrase: process.env.KEY_PASSPHRASE,
}));
`, encPath, plainPath)
	pem, err := dag.Bruno().Container().
		WithMountedSecret(plainPath, key, dagger.ContainerWithMountedSecretOpts{
			Owner: responderUser,
			Mode:  0o400,
		}).
		WithSecretVariable("KEY_PASSPHRASE", passphrase).
		WithExec([]string{"node", "-e", script}).
		File(encPath).
		Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("encrypt the %s client key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(label+"-encrypted-key-"+suffix, pem), nil
}

// randHex is a short random suffix, for the secret names that have to be unique.
func randHex(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return h[:12], nil
}

// randNamedSecret mints a uniquely-named secret holding fresh random bytes, for
// the throwaway PKCS#12 passwords the certificate-management issuers require.
// This suite consumes the PEM certificate and key directly and never the
// archive, so the value is irrelevant — only that it is not a literal.
func randNamedSecret(ctx context.Context, label string) (*dagger.Secret, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 32})
	if err != nil {
		return nil, err
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(label+"-"+suffix, h), nil
}

// freshCa self-signs a per-test root CA over a runtime-generated RSA key.
func freshCa(ctx context.Context, label string) (*dagger.CertificateManagementCertificateAuthority, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	key := dag.SetSecret(label+"-ca-key-"+suffix, keyPem)
	pwd, err := randNamedSecret(ctx, label+"-ca-pwd")
	if err != nil {
		return nil, fmt.Errorf("generate %s ca password: %w", label, err)
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	return dag.CertificateManagement().CreateCertificateAuthority(nb, serial, pwd, key,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   "bruno test ca " + label,
			ValidityDays: 30,
		}), nil
}

// leafKey mints a fresh RSA private key for a leaf certificate, PEM PKCS#8 in a
// uniquely-named secret, as the issuers expect.
func leafKey(ctx context.Context, label string) (*dagger.Secret, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s leaf key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(label+"-leaf-key-"+suffix, keyPem), nil
}

// issueServerCert signs a server leaf carrying host as its SAN.
func issueServerCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, host, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label+"-server")
	if err != nil {
		return nil, nil, err
	}
	pwd, err := randNamedSecret(ctx, label+"-server-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s server serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	issued := ca.IssueServerCertificate(host, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans:      []string{host, "localhost"},
			IPSans:       []string{"127.0.0.1"},
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// issueClientCert signs a client leaf whose Common Name is cn.
func issueClientCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, cn, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label+"-client")
	if err != nil {
		return nil, nil, err
	}
	pwd, err := randNamedSecret(ctx, label+"-client-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s client serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	issued := ca.IssueClientCertificate(cn, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueClientCertificateOpts{
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}
