// TLS / mTLS security tests for the dgraph module. Cert material is
// minted per-test from a fresh CA via the certificate-management + crypto
// modules; the server leaf carries the cluster's derived Alpha/Zero
// hostnames as SANs so the dgo client (which verifies each Alpha's cert
// against the dialed host) and `wget` accept it.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"dagger/tests/internal/dagger"
)

// randHex returns a short random hex string suitable for a cluster name.
func randHex(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return h[:12], nil
}

// clusterHosts reproduces Dgraph.Cluster's hostname derivation for a
// single-Zero, single-Alpha, single-replica cluster built from the
// constructor's default registry/tag. Tests need it to mint a server
// certificate whose SANs match the hostnames the client dials —
// verify-full-style TLS checks the SAN against the dialed host, and dgo
// cannot override ServerName. Returns (alphaHost, zeroHost).
func clusterHosts(name string) (string, string) {
	image := "docker.io/dgraph/dgraph:v24.0.4"
	key := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d|%d|%s", name, 1, 1, 1, image))
	suffix := hex.EncodeToString(key[:6])
	return "alpha-100-" + suffix, "zero-1-" + suffix
}

// randNamedSecret mints a uniquely-named *dagger.Secret holding fresh
// random bytes. Used for the throwaway PKCS#12 passwords the
// certificate-management leaf issuers require (we consume the PEM cert /
// key directly, never the PKCS#12 archive, so the value is irrelevant).
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

// freshCa mints a fresh per-test root CA via the certificate-management
// module from a runtime-random RSA key, password, and serial.
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
			CommonName:   "dgraph test ca " + label,
			ValidityDays: 30,
		}), nil
}

// leafKey mints a fresh RSA private key for a leaf certificate, wrapped
// in a uniquely-named *dagger.Secret (PEM PKCS#8, as the issuer expects).
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

// issueServerCert signs a server leaf certificate carrying every host
// (plus localhost / 127.0.0.1) as SANs, returning the PEM cert file and
// PEM key secret to hand to TlsServerSecurity / MtlsServerSecurity.
func issueServerCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, hosts []string, label string) (*dagger.File, *dagger.Secret, error) {
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
	dnsSans := append(append([]string{}, hosts...), "localhost")
	issued := ca.IssueServerCertificate(hosts[0], nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans:      dnsSans,
			IPSans:       []string{"127.0.0.1"},
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// issueClientCert signs a client leaf certificate, returning the PEM cert
// file and PEM key secret to hand to MtlsClientSecurity.
func issueClientCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, cn, label string) (*dagger.File, *dagger.Secret, error) {
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
	issued := ca.IssueClientCertificate(cn, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueClientCertificateOpts{
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// -----------------------------------------------------------------------------
// TLS / mTLS round-trip + coupling tests
// -----------------------------------------------------------------------------

// ClusterTlsRoundTripFromClient boots a one-way-TLS cluster and proves a
// matching TLS client (pinning the server CA) can AlterSchema + Mutate +
// Query end to end.
//
// +cache="never"
func (t *Tests) ClusterTlsRoundTripFromClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	alphaHost, zeroHost := clusterHosts(name)
	ca, err := freshCa(ctx, "dgraph-tls")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, []string{alphaHost, zeroHost}, "dgraph-tls-server")
	if err != nil {
		return err
	}
	cluster := dag.Dgraph().Cluster(
		dag.Dgraph().TLSServerSecurity(cert, key),
		dagger.DgraphClusterOpts{Name: name},
	)
	clientSec := dag.Dgraph().TLSClientSecurity(ca.CertPemFile())

	val, err := randName(ctx, "v")
	if err != nil {
		return err
	}
	client := cluster.Client(clientSec)
	if err := client.AlterSchema(ctx, "name: string @index(exact) ."); err != nil {
		return fmt.Errorf("alter over TLS: %w", err)
	}
	if _, err := client.Mutate(ctx, fmt.Sprintf(`{"name":%q}`, val), true); err != nil {
		return fmt.Errorf("mutate over TLS: %w", err)
	}
	resp, err := client.RunQuery(ctx, fmt.Sprintf(`{ q(func: eq(name, %q)) { uid name } }`, val))
	if err != nil {
		return fmt.Errorf("query over TLS: %w", err)
	}
	if !strings.Contains(resp, val) {
		return fmt.Errorf("expected TLS query response to contain %q, got: %s", val, resp)
	}
	return nil
}

// ClusterMtlsRoundTripFromClient boots a mutual-TLS cluster and proves a
// matching mTLS client (presenting a client cert signed by the trusted
// CA) can AlterSchema + Mutate + Query. One CA both signs the server leaf
// and anchors the accepted client certs — the simplest symmetric mTLS
// trust setup.
//
// +cache="never"
func (t *Tests) ClusterMtlsRoundTripFromClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	alphaHost, zeroHost := clusterHosts(name)
	ca, err := freshCa(ctx, "dgraph-mtls")
	if err != nil {
		return err
	}
	serverCert, serverKey, err := issueServerCert(ctx, ca, []string{alphaHost, zeroHost}, "dgraph-mtls-server")
	if err != nil {
		return err
	}
	clientCert, clientKey, err := issueClientCert(ctx, ca, "dgraph-client", "dgraph-mtls-client")
	if err != nil {
		return err
	}
	cluster := dag.Dgraph().Cluster(
		dag.Dgraph().MtlsServerSecurity(serverCert, serverKey, ca.CertPemFile()),
		dagger.DgraphClusterOpts{Name: name},
	)
	clientSec := dag.Dgraph().MtlsClientSecurity(ca.CertPemFile(), clientCert, clientKey)

	val, err := randName(ctx, "v")
	if err != nil {
		return err
	}
	client := cluster.Client(clientSec)
	if err := client.AlterSchema(ctx, "name: string @index(exact) ."); err != nil {
		return fmt.Errorf("alter over mTLS: %w", err)
	}
	if _, err := client.Mutate(ctx, fmt.Sprintf(`{"name":%q}`, val), true); err != nil {
		return fmt.Errorf("mutate over mTLS: %w", err)
	}
	resp, err := client.RunQuery(ctx, fmt.Sprintf(`{ q(func: eq(name, %q)) { uid name } }`, val))
	if err != nil {
		return fmt.Errorf("query over mTLS: %w", err)
	}
	if !strings.Contains(resp, val) {
		return fmt.Errorf("expected mTLS query response to contain %q, got: %s", val, resp)
	}
	return nil
}

// TlsClusterRejectsPlaintextClient verifies the mode-coupling check:
// asking a TLS cluster for a plaintext client returns an error naming
// both modes, before any wire activity. The requireMode guard fires ahead
// of GrpcEndpoints, so no cluster boots.
//
// +cache="never"
func (t *Tests) TlsClusterRejectsPlaintextClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	alphaHost, zeroHost := clusterHosts(name)
	ca, err := freshCa(ctx, "dgraph-tls-reject")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, []string{alphaHost, zeroHost}, "dgraph-tls-reject-server")
	if err != nil {
		return err
	}
	cluster := dag.Dgraph().Cluster(
		dag.Dgraph().TLSServerSecurity(cert, key),
		dagger.DgraphClusterOpts{Name: name},
	)
	err = cluster.Client(dag.Dgraph().PlaintextClientSecurity()).AlterSchema(ctx, "name: string .")
	if err == nil {
		return fmt.Errorf("expected plaintext client against TLS cluster to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "plaintext") || !strings.Contains(msg, "TLS") {
		return fmt.Errorf("expected mode-mismatch error naming both modes, got: %v", err)
	}
	return nil
}

// MtlsClusterRejectsTlsOnlyClient boots an mTLS cluster and proves a
// TLS-only client (verifies the server but presents no client cert) fails
// at the gRPC handshake — the REQUIREANDVERIFY listener demands a client
// certificate. This goes through the standalone Dgraph.Client, which has
// no cluster reference and so cannot short-circuit with a mode-mismatch
// error: the failure must come from the wire.
//
// +cache="never"
func (t *Tests) MtlsClusterRejectsTlsOnlyClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	alphaHost, zeroHost := clusterHosts(name)
	ca, err := freshCa(ctx, "dgraph-mtls-reject")
	if err != nil {
		return err
	}
	serverCert, serverKey, err := issueServerCert(ctx, ca, []string{alphaHost, zeroHost}, "dgraph-mtls-reject-server")
	if err != nil {
		return err
	}
	cluster := dag.Dgraph().Cluster(
		dag.Dgraph().MtlsServerSecurity(serverCert, serverKey, ca.CertPemFile()),
		dagger.DgraphClusterOpts{Name: name},
	)
	eps, err := cluster.GrpcEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("get endpoints: %w", err)
	}
	// TLS-only: trusts the server CA but carries no client cert, so the
	// REQUIREANDVERIFY handshake aborts.
	tlsOnly := dag.Dgraph().TLSClientSecurity(ca.CertPemFile())
	err = dag.Dgraph().Client(eps, tlsOnly).AlterSchema(ctx, "name: string .")
	if err == nil {
		return fmt.Errorf("expected TLS-only client against mTLS cluster to fail at the handshake")
	}
	return nil
}

// BindAlphasResolvesFromUserContainerTls boots a one-way-TLS cluster,
// binds its Alphas into an alpine container, and proves the Alpha's HTTPS
// /health listener is reachable there: `wget --ca-certificate` (trusting
// the test CA) succeeds, while the same `wget` without the CA fails
// certificate verification.
//
// +cache="never"
func (t *Tests) BindAlphasResolvesFromUserContainerTls(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	alphaHost, zeroHost := clusterHosts(name)
	ca, err := freshCa(ctx, "dgraph-tls-bind")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, []string{alphaHost, zeroHost}, "dgraph-tls-bind-server")
	if err != nil {
		return err
	}
	cluster := dag.Dgraph().Cluster(
		dag.Dgraph().TLSServerSecurity(cert, key),
		dagger.DgraphClusterOpts{Name: name},
	)
	// AlphaHostNames is a pure accessor (no service start); the wget retry
	// loop below absorbs the Raft-leader election lag once BindAlphas
	// wires the service into the container.
	hosts, err := cluster.AlphaHostNames(ctx)
	if err != nil {
		return fmt.Errorf("alpha hostnames: %w", err)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no alpha hostnames returned")
	}
	host := hosts[0]

	ctr := dag.Container().
		From("alpine:3.20").
		WithExec([]string{"apk", "add", "--no-cache", "wget", "ca-certificates"}).
		WithFile("/ca.crt", ca.CertPemFile())
	ctr = cluster.BindAlphas(ctr)

	healthURL := fmt.Sprintf("https://%s:8080/health", host)

	// With the CA pinned, retry until the Alpha reports healthy (Raft
	// leader election lags the listener coming up by a few seconds).
	withCA := ctr.WithExec([]string{"sh", "-c", fmt.Sprintf(
		"for i in $(seq 1 60); do wget --ca-certificate=/ca.crt -q -O /dev/null %s && echo OK && exit 0; sleep 2; done; echo TIMEOUT; exit 1",
		healthURL,
	)})
	out, err := withCA.Stdout(ctx)
	if err != nil {
		return fmt.Errorf("wget --ca-certificate against %s failed: %w", healthURL, err)
	}
	if !strings.Contains(out, "OK") {
		return fmt.Errorf("expected wget --ca-certificate to succeed, got: %s", out)
	}

	// The same request without the CA must fail certificate verification
	// (the test CA is not in the system trust store).
	_, err = ctr.WithExec([]string{"wget", "-q", "-O", "/dev/null", healthURL}).Stdout(ctx)
	if err == nil {
		return fmt.Errorf("expected wget without --ca-certificate to fail certificate verification against %s", healthURL)
	}
	return nil
}
