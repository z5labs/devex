package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dagger/tests/internal/dagger"
)

// VerifiesAgainstPrivateCa asserts that a registry fronted by a private CA is
// reachable by naming that CA, with verification still on and insecure unset —
// and that the CA is what made the difference.
//
// The three handles are the whole point. Only the first is expected to work;
// the second shows verification was never off, and the third shows that
// supplying *a* CA does not switch verification off either, which is the
// failure mode a trust anchor implemented as a flag would have. Without them a
// green first handle would be equally consistent with a module that had
// quietly stopped verifying.
//
// Both client libraries are exercised: PushImage runs through
// go-containerregistry and Resolve through oras, and the two build their
// transports separately.
func (t *Tests) VerifiesAgainstPrivateCa(ctx context.Context) error {
	reg, err := newTlsRegistry(ctx, "private-ca", false)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "private-ca")
	if err != nil {
		return err
	}

	trusted := dag.Oci().Registry(reg.Host, dagger.OciRegistryOpts{
		Username: registryUser,
		Password: reg.Secret,
		Service:  reg.Service,
		CaCert:   reg.Ca.CertPem,
	})
	pushed, err := trusted.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage against a registry fronted by a private CA: %v", err)
	}
	got, err := trusted.Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve against a registry fronted by a private CA: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}

	withoutCa := dag.Oci().Registry(reg.Host, dagger.OciRegistryOpts{
		Username: registryUser,
		Password: reg.Secret,
		Service:  reg.Service,
	})
	if _, err := withoutCa.Resolve(ctx, repo, "v1"); err == nil {
		return errors.New("Resolve succeeded with no CA and verification on: the private CA is not being verified")
	} else if !looksLikeCertificateFailure(err) {
		return fmt.Errorf("Resolve without the CA failed for some other reason than verification: %v", err)
	}

	other, err := newCa(ctx, "private-ca-other")
	if err != nil {
		return err
	}
	withWrongCa := dag.Oci().Registry(reg.Host, dagger.OciRegistryOpts{
		Username: registryUser,
		Password: reg.Secret,
		Service:  reg.Service,
		CaCert:   other.CertPem,
	})
	if _, err := withWrongCa.Resolve(ctx, repo, "v1"); err == nil {
		return errors.New("Resolve succeeded with an unrelated CA: supplying a CA is switching verification off")
	} else if !looksLikeCertificateFailure(err) {
		return fmt.Errorf("Resolve with an unrelated CA failed for some other reason than verification: %v", err)
	}
	return nil
}

// AuthenticatesWithClientCertificate asserts that a client certificate and its
// key reach a registry that demands mutual TLS, and that one signed by another
// authority is refused with an error that names the failure and carries no key
// material.
//
// The registry has no password authentication at all, so nothing here can pass
// on some other credential: the certificate is the only thing separating the
// two halves of this test.
func (t *Tests) AuthenticatesWithClientCertificate(ctx context.Context) error {
	reg, err := newTlsRegistry(ctx, "mtls", true)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "mtls")
	if err != nil {
		return err
	}
	clientCert, clientKey, clientKeyPem, err := reg.Ca.issueClient(ctx, "oci-test-client", "mtls")
	if err != nil {
		return err
	}

	client := dag.Oci().Registry(reg.Host, dagger.OciRegistryOpts{
		Service:    reg.Service,
		CaCert:     reg.Ca.CertPem,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	})
	pushed, err := client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage with a client certificate: %v", err)
	}
	got, err := client.Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve with a client certificate: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}

	// No certificate at all: the registry requires one, so this is what a
	// caller who thought TLS alone was enough would see.
	anonymous := dag.Oci().Registry(reg.Host, dagger.OciRegistryOpts{
		Service: reg.Service,
		CaCert:  reg.Ca.CertPem,
	})
	if _, err := anonymous.Resolve(ctx, repo, "v1"); err == nil {
		return errors.New("Resolve succeeded with no client certificate against a registry that requires one")
	}

	// A certificate from another authority: well-formed, correctly paired,
	// and not signed by anything the registry trusts.
	//
	// The refusal was measured as `remote error: tls: certificate required`
	// rather than a bad-certificate alert, because Go will not send a
	// certificate whose issuer is absent from the CertificateRequest's
	// acceptable-CA list — so an untrusted client certificate is dropped by
	// the client before the server ever sees it. Which of the two alerts
	// comes back is the TLS stack's business; that the operation is refused,
	// and refused with the reason named, is this module's.
	other, err := newCa(ctx, "mtls-other")
	if err != nil {
		return err
	}
	impostorCert, impostorKey, impostorKeyPem, err := other.issueClient(ctx, "oci-test-impostor", "mtls-other")
	if err != nil {
		return err
	}
	refused := dag.Oci().Registry(reg.Host, dagger.OciRegistryOpts{
		Service:    reg.Service,
		CaCert:     reg.Ca.CertPem,
		ClientCert: impostorCert,
		ClientKey:  impostorKey,
	})
	_, err = refused.Resolve(ctx, repo, "v1")
	if err == nil {
		return errors.New("Resolve succeeded with a client certificate signed by an unrelated CA")
	}
	if !looksLikeCertificateFailure(err) {
		return fmt.Errorf("the refusal does not name a certificate failure: %v", err)
	}
	for label, keyPem := range map[string]string{
		"the rejected client key": impostorKeyPem,
		"the accepted client key": clientKeyPem,
	} {
		if leaksKey(err.Error(), keyPem) {
			return fmt.Errorf("the error text leaks %s", label)
		}
	}
	return nil
}

// ClientCertificateNeedsBothHalves asserts that half a client certificate is
// refused, and that the refusal names the half that is missing.
//
// The alternative — falling back to anonymous TLS — is the failure this
// guards: a caller who believed they were authenticating would discover
// otherwise from a 401 much later, in a message that says nothing about the
// certificate they thought they had supplied.
//
// No registry is needed. The halves are checked while the connection is being
// resolved, before an address is dialled, which is exactly where a
// misconfiguration of the call should be caught.
func (t *Tests) ClientCertificateNeedsBothHalves(ctx context.Context) error {
	ca, err := newCa(ctx, "halves")
	if err != nil {
		return err
	}
	cert, key, _, err := ca.issueClient(ctx, "oci-test-halves", "halves")
	if err != nil {
		return err
	}

	certOnly := dag.Oci().Registry("registry.invalid:5000", dagger.OciRegistryOpts{ClientCert: cert})
	if _, err := certOnly.Resolve(ctx, "some/repo", "v1"); err == nil {
		return errors.New("Resolve succeeded with a client certificate and no key")
	} else if !strings.Contains(err.Error(), "without clientKey") {
		return fmt.Errorf("the refusal does not name clientKey as the missing half: %v", err)
	}

	keyOnly := dag.Oci().Registry("registry.invalid:5000", dagger.OciRegistryOpts{ClientKey: key})
	if _, err := keyOnly.Resolve(ctx, "some/repo", "v1"); err == nil {
		return errors.New("Resolve succeeded with a client key and no certificate")
	} else if !strings.Contains(err.Error(), "without clientCert") {
		return fmt.Errorf("the refusal does not name clientCert as the missing half: %v", err)
	}
	return nil
}

// InsecureStaysIndependentOfCertificates asserts that insecure and the TLS
// material do not imply anything about each other.
//
// VerifiesAgainstPrivateCa covers the direction that matters most — a CA does
// not switch verification off. This covers the other one: a caller who has
// asked for plain HTTP still gets it with a CA supplied beside it. A module
// that treated the two as one setting would fail one of these two tests
// whichever way it resolved them.
func (t *Tests) InsecureStaysIndependentOfCertificates(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "insecure-with-ca")
	if err != nil {
		return err
	}
	ca, err := newCa(ctx, "independent")
	if err != nil {
		return err
	}

	client := dag.Oci().Registry("test-registry.invalid", dagger.OciRegistryOpts{
		Username: registryUser,
		Password: reg.Secret,
		Service:  reg.Service,
		Insecure: true,
		CaCert:   ca.CertPem,
	})
	pushed, err := client.PushImage(ctx, repo, "v1", []*dagger.Container{baseImage("linux/amd64")})
	if err != nil {
		return fmt.Errorf("PushImage over plain HTTP with a CA supplied beside insecure: %v", err)
	}
	got, err := client.Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve over plain HTTP with a CA supplied beside insecure: %v", err)
	}
	if got != pushed {
		return fmt.Errorf("Resolve returned %s, want the pushed digest %s", got, pushed)
	}
	return nil
}

// looksLikeCertificateFailure reports whether an error is the TLS layer
// refusing a peer rather than something further along.
//
// Both ends of a handshake describe the same refusal differently — a client
// that cannot build a chain says "x509: certificate signed by unknown
// authority", while a server rejecting a client certificate comes back as
// "remote error: tls: ..." — so the check is a set of substrings rather than
// one. What it rules out is a green assertion over a 404 or a connection
// refused, which would prove nothing about verification.
func looksLikeCertificateFailure(err error) bool {
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"x509", "certificate", "tls:"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// leaksKey reports whether text carries any part of a PEM private key.
//
// The whole document is checked, and so is every base64 line inside it: an
// error that echoed one line of a key has still put key material in a CI log,
// and the PEM envelope is the part most likely to be stripped on the way.
func leaksKey(text, keyPem string) bool {
	if strings.Contains(text, keyPem) {
		return true
	}
	for _, line := range strings.Split(keyPem, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 32 || strings.HasPrefix(line, "-----") {
			continue
		}
		if strings.Contains(text, line) {
			return true
		}
	}
	return false
}

// The TLS material a zot container is served from. The server key is the only
// one of the three that is mounted as a secret; a certificate and a CA
// certificate are public by construction.
const (
	serverCertPath = "/etc/zot/certs/server.crt"
	serverKeyPath  = "/etc/zot/certs/server.key"
	clientCaPath   = "/etc/zot/certs/client-ca.crt"
)

// testCa is a certificate authority minted for one test, together with the
// PEM file a client verifies against.
//
// Nothing here is committed: the key is generated by the crypto module and
// the certificate is signed by certificate-management, both at run time. A
// fixture CA would have to ship a private key, and a private key in a
// repository is a private key forever.
type testCa struct {
	Authority *dagger.CertificateManagementCertificateAuthority
	CertPem   *dagger.File
}

// newCa mints a fresh CA.
//
// notBefore and serial vary per call because certificate-management is a pure
// signer whose output is fully determined by its inputs — two calls sharing
// them would share a CA, and two tests sharing a CA would stop being
// independent.
func newCa(ctx context.Context, label string) (*testCa, error) {
	key, err := leafKey(ctx, label+"-ca")
	if err != nil {
		return nil, err
	}
	pwd, err := namedSecret(ctx, "oci-"+label+"-ca-pwd")
	if err != nil {
		return nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("random serial (%s ca): %v", label, err)
	}

	ca := dag.CertificateManagement().CreateCertificateAuthority(
		time.Now().UTC().Format(time.RFC3339), serial, pwd, key,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   "oci test ca " + label,
			ValidityDays: 1,
		})
	return &testCa{Authority: ca, CertPem: ca.CertPemFile()}, nil
}

// issueServer signs a server certificate naming host.
//
// host is the Dagger hostname the registry service is pinned to, which is
// what makes this possible at all: a service's hostname is normally assigned
// by the engine and therefore unknowable before the container that carries
// the certificate exists. WithHostname breaks that circle by letting the test
// choose the name first.
func (ca *testCa) issueServer(ctx context.Context, host, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label+"-server")
	if err != nil {
		return nil, nil, err
	}
	pwd, err := namedSecret(ctx, "oci-"+label+"-server-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("random serial (%s server cert): %v", label, err)
	}

	issued := ca.Authority.IssueServerCertificate(
		host, time.Now().UTC().Format(time.RFC3339), serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans:      []string{host, "localhost"},
			IPSans:       []string{"127.0.0.1"},
			ValidityDays: 1,
		})
	return issued.CertPemFile(), key, nil
}

// issueClient signs a client certificate, returning the certificate, the key
// as a secret, and the key's plaintext.
//
// The plaintext is returned so a test can assert it never appears in an error
// crossing the module boundary. It is the key the test minted rather than
// anything certificate-management echoed back, so the assertion is about the
// exact bytes the module was handed.
func (ca *testCa) issueClient(ctx context.Context, commonName, label string) (*dagger.File, *dagger.Secret, string, error) {
	keyPem, err := dag.Crypto().GenerateEcdsaP256Key().Pem().Contents(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate %s client key: %v", label, err)
	}
	name, err := uniqueName(ctx, "oci-"+label+"-client-key")
	if err != nil {
		return nil, nil, "", err
	}
	key := dag.SetSecret(name, keyPem)

	pwd, err := namedSecret(ctx, "oci-"+label+"-client-pwd")
	if err != nil {
		return nil, nil, "", err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("random serial (%s client cert): %v", label, err)
	}

	issued := ca.Authority.IssueClientCertificate(
		commonName, time.Now().UTC().Format(time.RFC3339), serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueClientCertificateOpts{
			ValidityDays: 1,
		})
	return issued.CertPemFile(), key, keyPem, nil
}

// leafKey mints a PKCS#8 PEM private key and wraps it as a secret, which is
// the form certificate-management signs from. P-256 rather than RSA because
// nothing here is testing key strength and every handshake in the suite pays
// for the choice.
func leafKey(ctx context.Context, label string) (*dagger.Secret, error) {
	pem, err := dag.Crypto().GenerateEcdsaP256Key().Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s key: %v", label, err)
	}
	name, err := uniqueName(ctx, "oci-"+label)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(name, pem), nil
}

// shortHostname mints a pinned service hostname, and is short on purpose.
//
// Dagger sets the container's own hostname to the fully-qualified session
// name — the chosen label plus a three-part session suffix that measured 41
// characters — and Linux refuses a hostname over 64, so the container fails to
// start with `sethostname: invalid argument` rather than with anything about
// the name being long. Twelve characters leaves room for a session suffix half
// again as long as the one seen, which is why the label carries no test name.
//
// Registry() dials Service.Endpoint(), and that reports the short name, so
// this is also the name a server certificate has to carry.
func shortHostname(ctx context.Context) (string, error) {
	hex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256 (registry hostname): %v", err)
	}
	return "oci-" + hex[:8], nil
}

// namedSecret mints a random secret under a uniquely suffixed name. Secret
// names surface in trace UIs, so the suffix is independent of the value.
func namedSecret(ctx context.Context, prefix string) (*dagger.Secret, error) {
	value, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("random sha256 (%s): %v", prefix, err)
	}
	name, err := uniqueName(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(name, value), nil
}

// tlsZotConfig renders a zot configuration serving HTTPS.
//
// clientCa turns the listener into a mutual-TLS one; htpasswd adds password
// authentication. They are rendered through encoding/json rather than a
// format string for the same reason the docker configs are: a hand-built
// document is one unescaped character away from a parse failure that reads
// like a module bug.
func tlsZotConfig(clientCa, htpasswd bool) (string, error) {
	tls := map[string]any{"cert": serverCertPath, "key": serverKeyPath}
	if clientCa {
		tls["cacert"] = clientCaPath
	}
	httpCfg := map[string]any{
		"address": "0.0.0.0",
		"port":    "5000",
		"tls":     tls,
	}
	if htpasswd {
		httpCfg["auth"] = map[string]any{"htpasswd": map[string]any{"path": htpasswdPath}}
	}
	raw, err := json.Marshal(map[string]any{
		"storage": map[string]any{"rootDirectory": zotStoragePath, "dedupe": false},
		"http":    httpCfg,
		"log":     map[string]any{"level": "warn"},
	})
	if err != nil {
		return "", fmt.Errorf("render zot tls config: %v", err)
	}
	return string(raw), nil
}

// tlsTestRegistry is a zot instance serving HTTPS under a certificate issued
// by a CA minted for the test that asked for it.
type tlsTestRegistry struct {
	Service *dagger.Service
	// Host is the pinned Dagger hostname, and the name the server
	// certificate carries.
	Host string
	// Ca is the authority that signed the server certificate; the mutual-TLS
	// variant also trusts client certificates it signs.
	Ca *testCa
	// Secret and Password are the htpasswd credential, nil and empty on a
	// registry that authenticates by client certificate instead.
	Secret   *dagger.Secret
	Password string
}

// newTlsRegistry stands up a registry reachable only over HTTPS.
//
// requireClientCert configures zot with a client CA and no password
// authentication, which is what makes it demand and verify a client
// certificate; without it the registry authenticates with htpasswd over TLS,
// which is the private-CA shape an internal registry usually has.
func newTlsRegistry(ctx context.Context, label string, requireClientCert bool) (*tlsTestRegistry, error) {
	host, err := shortHostname(ctx)
	if err != nil {
		return nil, err
	}
	ca, err := newCa(ctx, label)
	if err != nil {
		return nil, err
	}
	serverCert, serverKey, err := ca.issueServer(ctx, host, label)
	if err != nil {
		return nil, err
	}
	config, err := tlsZotConfig(requireClientCert, !requireClientCert)
	if err != nil {
		return nil, err
	}

	ctr := dag.Container().From(registryImage()).
		WithFile(serverCertPath, serverCert, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithMountedSecret(serverKeyPath, serverKey, dagger.ContainerWithMountedSecretOpts{Mode: 0o400})

	reg := &tlsTestRegistry{Host: host, Ca: ca}
	if requireClientCert {
		ctr = ctr.WithFile(clientCaPath, ca.CertPem, dagger.ContainerWithFileOpts{Permissions: 0o644})
	} else {
		htpasswd, secret, pwd, err := newHtpasswd(ctx)
		if err != nil {
			return nil, err
		}
		ctr = ctr.WithMountedFile(htpasswdPath, htpasswd)
		reg.Secret, reg.Password = secret, pwd
	}

	// WithHostname is what lets the certificate name the address the client
	// will dial: the name is chosen here, the certificate is signed for it
	// above, and Registry() resolves the service to exactly this endpoint.
	// The name carries a random suffix because Dagger's session DNS is
	// session-wide, so two concurrent tests sharing one would collide.
	reg.Service = ctr.
		WithNewFile(zotConfigPath, config).
		WithExposedPort(5000).
		AsService(dagger.ContainerAsServiceOpts{
			UseEntrypoint: true,
			Args:          []string{"serve", zotConfigPath},
		}).
		WithHostname(host)

	return reg, nil
}
