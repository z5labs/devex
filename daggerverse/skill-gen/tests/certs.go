package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"
)

// -----------------------------------------------------------------------------
// TLS / mTLS certificate helpers. Ported from daggerverse/postgres/tests so the
// skill-gen TLS round-trip tests can mint per-test CAs and leaf certs at runtime
// (no PEM literals enter git). Every key, password, and serial is random.
// -----------------------------------------------------------------------------

// clusterHost reproduces Postgres.Cluster's hostname derivation
// (`postgres-` + the first 12 hex chars of sha256(name)). Tests need it to mint
// a server certificate whose SAN matches the hostname clients dial —
// sslmode=verify-full checks the SAN against the dialed host.
func clusterHost(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "postgres-" + hex.EncodeToString(sum[:6])
}

// randNamedSecret mints a uniquely-named *dagger.Secret holding fresh random
// bytes. Used for the throwaway PKCS#12 passwords the certificate-management
// leaf issuers require (we consume the PEM cert / key directly, never the
// PKCS#12 archive, so the value is irrelevant).
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

// freshCa mints a fresh per-test root CA via the certificate-management module
// from a runtime-random RSA key, password, and serial.
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
			CommonName:   "pg test ca " + label,
			ValidityDays: 30,
		}), nil
}

// leafKey mints a fresh RSA private key for a leaf certificate, wrapped in a
// uniquely-named *dagger.Secret (PEM PKCS#8, as the issuer expects).
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

// issueServerCert signs a server leaf certificate carrying host (and localhost /
// 127.0.0.1) as SANs, returning the PEM cert file and PEM key secret to hand to
// TlsServerSecurity / MtlsServerSecurity.
func issueServerCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, host, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label)
	if err != nil {
		return nil, nil, err
	}
	pwd, err := randNamedSecret(ctx, label+"-leaf-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s serial: %w", label, err)
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

// issueClientCert signs a client leaf certificate, returning the PEM cert file
// and PEM key secret to hand to MtlsClientSecurity. The certificate's Common
// Name is set to user because the primary's pg_hba.conf uses
// clientcert=verify-full, which additionally requires the client cert CN to
// match the PostgreSQL role being authenticated.
func issueClientCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, user, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label)
	if err != nil {
		return nil, nil, err
	}
	pwd, err := randNamedSecret(ctx, label+"-leaf-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	issued := ca.IssueClientCertificate(user, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueClientCertificateOpts{
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}
