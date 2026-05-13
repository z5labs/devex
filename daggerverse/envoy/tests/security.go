package main

import (
	"context"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"

	"gopkg.in/yaml.v3"
)

// testCa mints a fresh CA via certificate-management for tests that
// need TLS material. The returned secret is the CA's bound password,
// shared by ca.KeyStore() and ca.TrustStore().
func testCa(ctx context.Context, label string) (*dagger.CertificateManagementCertificateAuthority, *dagger.Secret, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	caKey := dag.SetSecret(label+"-ca-key", keyPem)

	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA pwd: %w", err)
	}
	caPwd := dag.SetSecret(label+"-ca-pwd", pwdHex)

	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA serial: %w", err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)

	ca := dag.CertificateManagement().CreateCertificateAuthority(nb, serial, caPwd, caKey,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   label + " Test CA",
			ValidityDays: 365,
		})
	return ca, caPwd, nil
}

// issueClientLeaf mints a fresh client leaf signed by ca, returning
// the leaf as a cert PEM file + private key secret suitable for curl
// `--cert` / `--key`.
func issueClientLeaf(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, label, cn string) (*dagger.File, *dagger.Secret, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf key: %w", err)
	}
	leafKey := dag.SetSecret(label+"-leaf-key", keyPem)

	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf pwd: %w", err)
	}
	leafPwd := dag.SetSecret(label+"-leaf-pwd", pwdHex)

	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf serial: %w", err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)

	issued := ca.IssueClientCertificate(cn, nb, serial, leafPwd, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueClientCertificateOpts{
			ValidityDays: 365,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// listenerOf parses the rendered bootstrap and returns the first
// listener's map for inspection.
func listenerOf(contents string) (map[string]any, error) {
	var cfg struct {
		StaticResources struct {
			Listeners []map[string]any `yaml:"listeners"`
		} `yaml:"static_resources"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w\n---\n%s", err, contents)
	}
	if len(cfg.StaticResources.Listeners) != 1 {
		return nil, fmt.Errorf("expected 1 listener, got %d", len(cfg.StaticResources.Listeners))
	}
	return cfg.StaticResources.Listeners[0], nil
}

// clusterOf parses the rendered bootstrap and returns the first
// cluster's map for inspection.
func clusterOf(contents string) (map[string]any, error) {
	var cfg struct {
		StaticResources struct {
			Clusters []map[string]any `yaml:"clusters"`
		} `yaml:"static_resources"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w\n---\n%s", err, contents)
	}
	if len(cfg.StaticResources.Clusters) != 1 {
		return nil, fmt.Errorf("expected 1 cluster, got %d", len(cfg.StaticResources.Clusters))
	}
	return cfg.StaticResources.Clusters[0], nil
}

// minimalHttpListener builds a bare HCM + listener wired through the
// "upstream" cluster name, used by the listener-side rendering
// tests. Returns the listener, an HCM cluster name to register on
// the proxy, and any error.
func minimalHttpListener(name string, port int, security *dagger.EnvoyServerSecurity) *dagger.EnvoyListener {
	e := dag.Envoy()
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "upstream")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	return e.HTTPListener(name, port, hcm, dagger.EnvoyHTTPListenerOpts{Security: security})
}

// TlsServerSecurityRendersDownstreamTlsContext asserts that an
// HttpListener built with TlsServerSecurity renders a downstream TLS
// transport_socket on its filter chain referencing the listener's
// PKCS#12 mount path and a password env var, with no
// require_client_certificate.
func (t *Tests) TlsServerSecurityRendersDownstreamTlsContext(ctx context.Context) error {
	e := dag.Envoy()
	ca, caPwd, err := testCa(ctx, "tls-server")
	if err != nil {
		return err
	}
	sec := e.TLSServerSecurity(ca.KeyStore().Pkcs12(), caPwd)
	listener := minimalHttpListener("http", 18443, sec)
	contents, err := e.Proxy().
		WithListener(listener).
		WithCluster(e.Cluster("upstream")).
		ConfigFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile().Contents: %w", err)
	}
	l, err := listenerOf(contents)
	if err != nil {
		return err
	}
	fcs, _ := l["filter_chains"].([]any)
	if len(fcs) != 1 {
		return fmt.Errorf("expected 1 filter chain, got %d", len(fcs))
	}
	fc, _ := fcs[0].(map[string]any)
	ts, ok := fc["transport_socket"].(map[string]any)
	if !ok {
		return fmt.Errorf("expected transport_socket on filter_chains[0], got: %v", fc)
	}
	if got := ts["name"]; got != "envoy.transport_sockets.tls" {
		return fmt.Errorf("transport_socket.name = %v, want envoy.transport_sockets.tls", got)
	}
	typed, _ := ts["typed_config"].(map[string]any)
	if got := typed["@type"]; got != "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext" {
		return fmt.Errorf("typed_config.@type = %v, want DownstreamTlsContext", got)
	}
	if _, present := typed["require_client_certificate"]; present {
		return fmt.Errorf("TLS-only listener should not set require_client_certificate, got typed_config: %v", typed)
	}
	common, _ := typed["common_tls_context"].(map[string]any)
	certs, _ := common["tls_certificates"].([]any)
	if len(certs) != 1 {
		return fmt.Errorf("expected 1 tls_certificate, got %d", len(certs))
	}
	cert, _ := certs[0].(map[string]any)
	chain, _ := cert["certificate_chain"].(map[string]any)
	if got := chain["filename"]; got != "/etc/envoy/secrets/listener-http.crt" {
		return fmt.Errorf("tls_certificates[0].certificate_chain.filename = %v, want listener cert path", got)
	}
	key, _ := cert["private_key"].(map[string]any)
	if got := key["filename"]; got != "/etc/envoy/secrets/listener-http.key" {
		return fmt.Errorf("tls_certificates[0].private_key.filename = %v, want listener key path", got)
	}
	return nil
}

// MtlsServerSecurityRequiresClientCert asserts that mTLS adds
// require_client_certificate + a validation_context.trusted_ca
// pointing at the listener's trust PEM mount path.
func (t *Tests) MtlsServerSecurityRequiresClientCert(ctx context.Context) error {
	e := dag.Envoy()
	serverCa, serverCaPwd, err := testCa(ctx, "mtls-server-ca")
	if err != nil {
		return err
	}
	clientCa, clientCaPwd, err := testCa(ctx, "mtls-server-client-ca")
	if err != nil {
		return err
	}
	sec := e.MtlsServerSecurity(serverCa.KeyStore().Pkcs12(), serverCaPwd, clientCa.TrustStore().Pkcs12(), clientCaPwd)
	listener := minimalHttpListener("http", 18443, sec)
	contents, err := e.Proxy().
		WithListener(listener).
		WithCluster(e.Cluster("upstream")).
		ConfigFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile().Contents: %w", err)
	}
	l, err := listenerOf(contents)
	if err != nil {
		return err
	}
	fcs, _ := l["filter_chains"].([]any)
	fc, _ := fcs[0].(map[string]any)
	ts, _ := fc["transport_socket"].(map[string]any)
	typed, _ := ts["typed_config"].(map[string]any)
	if got, _ := typed["require_client_certificate"].(bool); !got {
		return fmt.Errorf("MTLS listener must set require_client_certificate=true, got typed_config: %v", typed)
	}
	common, _ := typed["common_tls_context"].(map[string]any)
	vc, _ := common["validation_context"].(map[string]any)
	tca, _ := vc["trusted_ca"].(map[string]any)
	if got := tca["filename"]; got != "/etc/envoy/secrets/listener-http-trust.pem" {
		return fmt.Errorf("validation_context.trusted_ca.filename = %v, want listener trust pem path", got)
	}
	return nil
}

// PlaintextServerSecurityRendersNoTransportSocket asserts that a
// listener built with PlaintextServerSecurity is byte-identical to
// one built with nil security (no transport_socket in either).
func (t *Tests) PlaintextServerSecurityRendersNoTransportSocket(ctx context.Context) error {
	e := dag.Envoy()
	nilContents, err := e.Proxy().
		WithListener(minimalHttpListener("http", 18080, nil)).
		WithCluster(e.Cluster("upstream")).
		ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("nil-security ConfigFile: %w", err)
	}
	plainContents, err := e.Proxy().
		WithListener(minimalHttpListener("http", 18080, e.PlaintextServerSecurity())).
		WithCluster(e.Cluster("upstream")).
		ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("plaintext-security ConfigFile: %w", err)
	}
	if nilContents != plainContents {
		return fmt.Errorf("nil and PLAINTEXT renders differ:\n--- nil ---\n%s\n--- plaintext ---\n%s", nilContents, plainContents)
	}
	l, err := listenerOf(plainContents)
	if err != nil {
		return err
	}
	fcs, _ := l["filter_chains"].([]any)
	fc, _ := fcs[0].(map[string]any)
	if _, present := fc["transport_socket"]; present {
		return fmt.Errorf("PLAINTEXT listener must not render transport_socket, got: %v", fc)
	}
	return nil
}

// TlsUpstreamSecurityRendersUpstreamTlsContext asserts that a
// Cluster built with TlsUpstreamSecurity renders an
// UpstreamTlsContext transport_socket on the cluster with a
// validation_context pointing at the upstream trust PEM path.
func (t *Tests) TlsUpstreamSecurityRendersUpstreamTlsContext(ctx context.Context) error {
	e := dag.Envoy()
	ca, caPwd, err := testCa(ctx, "tls-upstream")
	if err != nil {
		return err
	}
	sec := e.TLSUpstreamSecurity(ca.TrustStore().Pkcs12(), caPwd)
	cluster := e.Cluster("upstream", dagger.EnvoyClusterOpts{Upstream: sec})
	contents, err := e.Proxy().WithCluster(cluster).ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile: %w", err)
	}
	c, err := clusterOf(contents)
	if err != nil {
		return err
	}
	ts, ok := c["transport_socket"].(map[string]any)
	if !ok {
		return fmt.Errorf("expected cluster.transport_socket, got: %v", c)
	}
	typed, _ := ts["typed_config"].(map[string]any)
	if got := typed["@type"]; got != "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext" {
		return fmt.Errorf("typed_config.@type = %v, want UpstreamTlsContext", got)
	}
	common, _ := typed["common_tls_context"].(map[string]any)
	if _, present := common["tls_certificates"]; present {
		return fmt.Errorf("TLS-only upstream should not set tls_certificates, got: %v", common)
	}
	vc, _ := common["validation_context"].(map[string]any)
	tca, _ := vc["trusted_ca"].(map[string]any)
	if got := tca["filename"]; got != "/etc/envoy/secrets/upstream-upstream-trust.pem" {
		return fmt.Errorf("trusted_ca.filename = %v, want upstream trust pem path", got)
	}
	return nil
}

// MtlsUpstreamSecurityIncludesClientLeaf asserts that mTLS upstream
// renders both validation_context AND tls_certificates referencing
// the cluster keystore PKCS#12 path + env-var password.
func (t *Tests) MtlsUpstreamSecurityIncludesClientLeaf(ctx context.Context) error {
	e := dag.Envoy()
	ksCa, ksPwd, err := testCa(ctx, "mtls-up-ks")
	if err != nil {
		return err
	}
	tsCa, tsPwd, err := testCa(ctx, "mtls-up-ts")
	if err != nil {
		return err
	}
	sec := e.MtlsUpstreamSecurity(ksCa.KeyStore().Pkcs12(), ksPwd, tsCa.TrustStore().Pkcs12(), tsPwd)
	cluster := e.Cluster("backend", dagger.EnvoyClusterOpts{Upstream: sec})
	contents, err := e.Proxy().WithCluster(cluster).ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile: %w", err)
	}
	c, err := clusterOf(contents)
	if err != nil {
		return err
	}
	ts, _ := c["transport_socket"].(map[string]any)
	typed, _ := ts["typed_config"].(map[string]any)
	common, _ := typed["common_tls_context"].(map[string]any)
	certs, ok := common["tls_certificates"].([]any)
	if !ok || len(certs) != 1 {
		return fmt.Errorf("expected 1 tls_certificate on MTLS upstream, got: %v", common)
	}
	cert, _ := certs[0].(map[string]any)
	chain, _ := cert["certificate_chain"].(map[string]any)
	if got := chain["filename"]; got != "/etc/envoy/secrets/upstream-backend.crt" {
		return fmt.Errorf("upstream certificate_chain.filename = %v, want upstream cert path", got)
	}
	key, _ := cert["private_key"].(map[string]any)
	if got := key["filename"]; got != "/etc/envoy/secrets/upstream-backend.key" {
		return fmt.Errorf("upstream private_key.filename = %v, want upstream key path", got)
	}
	return nil
}

// PlaintextUpstreamSecurityRendersNoTransportSocket asserts that
// PlaintextUpstreamSecurity renders the same cluster as nil upstream.
func (t *Tests) PlaintextUpstreamSecurityRendersNoTransportSocket(ctx context.Context) error {
	e := dag.Envoy()
	nilContents, err := e.Proxy().WithCluster(e.Cluster("upstream")).ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("nil-upstream ConfigFile: %w", err)
	}
	plainContents, err := e.Proxy().WithCluster(
		e.Cluster("upstream", dagger.EnvoyClusterOpts{Upstream: e.PlaintextUpstreamSecurity()}),
	).ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("plaintext-upstream ConfigFile: %w", err)
	}
	if nilContents != plainContents {
		return fmt.Errorf("nil and PLAINTEXT upstream renders differ:\n--- nil ---\n%s\n--- plaintext ---\n%s", nilContents, plainContents)
	}
	c, err := clusterOf(plainContents)
	if err != nil {
		return err
	}
	if _, present := c["transport_socket"]; present {
		return fmt.Errorf("PLAINTEXT upstream must not render transport_socket, got: %v", c)
	}
	return nil
}
