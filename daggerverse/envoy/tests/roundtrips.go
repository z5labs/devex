package main

import (
	"context"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"
)

// pythonHttpsUpstream stands up a python:3-alpine HTTPS server on
// the given port, presenting a server leaf signed by ca and valid
// for the given hostname (DNS SAN). Every GET returns 200 with
// marker as the body.
func pythonHttpsUpstream(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, marker string, port int, hostname string, mtlsClientTrust *dagger.File) (*dagger.Service, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("upstream key: %w", err)
	}
	leafKey := dag.SetSecret("upstream-server-key-"+hostname, keyPem)
	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("upstream pwd: %w", err)
	}
	leafPwd := dag.SetSecret("upstream-server-pwd-"+hostname, pwdHex)
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("upstream serial: %w", err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	issued := ca.IssueServerCertificate(hostname, nb, serial, leafPwd, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans:      []string{hostname, "localhost"},
			IPSans:       []string{"127.0.0.1"},
			ValidityDays: 365,
		})

	reqClient := "False"
	if mtlsClientTrust != nil {
		reqClient = "True"
	}
	script := fmt.Sprintf(`import http.server, ssl, socketserver
MARKER = %q
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = MARKER.encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a, **k): pass
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain("/certs/server.crt", "/certs/server.key")
if %s:
    ctx.load_verify_locations("/certs/client-ca.pem")
    ctx.verify_mode = ssl.CERT_REQUIRED
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("", %d), H) as srv:
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    srv.serve_forever()
`, marker, reqClient, port)

	ctr := dag.Container().From("python:3-alpine").
		WithExposedPort(port).
		WithFile("/certs/server.crt", issued.CertPemFile()).
		WithMountedSecret("/certs/server.key", issued.PrivateKeyPem())
	if mtlsClientTrust != nil {
		ctr = ctr.WithFile("/certs/client-ca.pem", mtlsClientTrust)
	}
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		Args: []string{"python", "-u", "-c", script},
	}), nil
}

// L7HttpsRoundTrip stands up a plaintext HTTP upstream behind an
// Envoy TLS-terminated HttpListener and asserts a client trusting
// the CA can complete an HTTPS round-trip.
func (t *Tests) L7HttpsRoundTrip(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	upstream := pythonHttpUpstream(mark, 5678)

	ca, caPwd, err := testCa(ctx, "l7https")
	if err != nil {
		return err
	}
	e := dag.Envoy()
	sec := e.TLSServerSecurity(ca.KeyStore().Pkcs12(), caPwd)
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "upstream")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18443, hcm, dagger.EnvoyHTTPListenerOpts{Security: sec})
	cluster := e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5678))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("upstream", upstream).
		WithListener(listener).
		WithCluster(cluster).
		Service()

	_, err = dag.Container().From(curlImage).
		WithServiceBinding("envoy", svc).
		WithFile("/certs/ca.pem", ca.CertPemFile(), dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  BODY=$(curl -sS --cacert /certs/ca.pem https://envoy:18443/ || true)
  case "${BODY}" in *"${MARKER}"*) echo "marker observed after ${i}s"; exit 0 ;; esac
  sleep 1
done
echo "marker ${MARKER} never appeared via TLS; last body: ${BODY}" >&2
exit 1
`}).Sync(ctx)
	if err != nil {
		return fmt.Errorf("L7Https round-trip: %w", err)
	}
	return nil
}

// L7HttpsMtlsRejectsAnonymousClient asserts that an mTLS-terminated
// HttpListener refuses clients that don't present a cert signed by
// the configured clientTrustStore CA — curl exits non-zero.
func (t *Tests) L7HttpsMtlsRejectsAnonymousClient(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	upstream := pythonHttpUpstream(mark, 5678)

	serverCa, serverCaPwd, err := testCa(ctx, "mtls-reject-server-ca")
	if err != nil {
		return err
	}
	clientCa, clientCaPwd, err := testCa(ctx, "mtls-reject-client-ca")
	if err != nil {
		return err
	}
	e := dag.Envoy()
	sec := e.MtlsServerSecurity(serverCa.KeyStore().Pkcs12(), serverCaPwd, clientCa.TrustStore().Pkcs12(), clientCaPwd)
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "upstream")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18443, hcm, dagger.EnvoyHTTPListenerOpts{Security: sec})
	cluster := e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5678))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("upstream", upstream).
		WithListener(listener).
		WithCluster(cluster).
		Service()

	// Wait for envoy to come up (admin /ready 200), THEN probe without
	// a client cert and assert curl fails.
	_, err = dag.Container().From(curlImage).
		WithServiceBinding("envoy", svc).
		WithFile("/certs/ca.pem", serverCa.CertPemFile(), dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  CODE=$(curl -sS -o /dev/null -w '%{http_code}' http://envoy:9901/ready || echo 000)
  if [ "$CODE" = "200" ]; then
    break
  fi
  sleep 1
done
if [ "$CODE" != "200" ]; then
  echo "envoy admin never ready" >&2
  exit 2
fi
# No client cert — handshake must fail.
if curl -sS --max-time 10 --cacert /certs/ca.pem https://envoy:18443/ >/tmp/out 2>/tmp/err; then
  echo "expected curl to fail without client cert; stdout:" >&2
  cat /tmp/out >&2
  exit 1
fi
echo "curl rejected without client cert (expected)"
`}).Sync(ctx)
	if err != nil {
		return fmt.Errorf("L7HttpsMtls reject: %w", err)
	}
	return nil
}

// L7HttpsMtlsAcceptsAuthorizedClient asserts that an mTLS-terminated
// listener accepts curl clients that present a leaf signed by the
// configured clientTrustStore CA.
func (t *Tests) L7HttpsMtlsAcceptsAuthorizedClient(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	upstream := pythonHttpUpstream(mark, 5678)

	serverCa, serverCaPwd, err := testCa(ctx, "mtls-accept-server-ca")
	if err != nil {
		return err
	}
	clientCa, clientCaPwd, err := testCa(ctx, "mtls-accept-client-ca")
	if err != nil {
		return err
	}
	clientCert, clientKey, err := issueClientLeaf(ctx, clientCa, "mtls-accept", "test-client")
	if err != nil {
		return err
	}

	e := dag.Envoy()
	sec := e.MtlsServerSecurity(serverCa.KeyStore().Pkcs12(), serverCaPwd, clientCa.TrustStore().Pkcs12(), clientCaPwd)
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "upstream")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18443, hcm, dagger.EnvoyHTTPListenerOpts{Security: sec})
	cluster := e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5678))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("upstream", upstream).
		WithListener(listener).
		WithCluster(cluster).
		Service()

	_, err = dag.Container().From(curlImage).
		WithServiceBinding("envoy", svc).
		WithFile("/certs/ca.pem", serverCa.CertPemFile(), dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithFile("/certs/client.crt", clientCert, dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithMountedSecret("/certs/client.key", clientKey, dagger.ContainerWithMountedSecretOpts{Mode: 0o644}).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  BODY=$(curl -sS --cacert /certs/ca.pem --cert /certs/client.crt --key /certs/client.key https://envoy:18443/ || true)
  case "${BODY}" in *"${MARKER}"*) echo "marker observed after ${i}s"; exit 0 ;; esac
  sleep 1
done
echo "marker ${MARKER} never appeared; last body: ${BODY}" >&2
exit 1
`}).Sync(ctx)
	if err != nil {
		return fmt.Errorf("L7HttpsMtls accept: %w", err)
	}
	return nil
}

// UpstreamTlsRoundTrip stands up a python HTTPS upstream and asserts
// Envoy connects to it over TLS when the cluster's
// UpstreamSecurity truststore matches the upstream's server cert CA.
func (t *Tests) UpstreamTlsRoundTrip(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	ca, caPwd, err := testCa(ctx, "upstream-tls")
	if err != nil {
		return err
	}
	upstream, err := pythonHttpsUpstream(ctx, ca, mark, 5679, "tls-upstream", nil)
	if err != nil {
		return err
	}

	e := dag.Envoy()
	upSec := e.TLSUpstreamSecurity(ca.TrustStore().Pkcs12(), caPwd)
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "secured")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18080, hcm)
	cluster := e.Cluster("secured", dagger.EnvoyClusterOpts{Upstream: upSec}).
		WithEndpoint(e.Endpoint("tls-upstream", 5679))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("tls-upstream", upstream).
		WithListener(listener).
		WithCluster(cluster).
		Service()

	_, err = dag.Container().From(curlImage).
		WithServiceBinding("envoy", svc).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  BODY=$(curl -sS http://envoy:18080/ || true)
  case "${BODY}" in *"${MARKER}"*) echo "marker observed after ${i}s"; exit 0 ;; esac
  sleep 1
done
echo "marker ${MARKER} never appeared via upstream TLS; last body: ${BODY}" >&2
exit 1
`}).Sync(ctx)
	if err != nil {
		return fmt.Errorf("UpstreamTls round-trip: %w", err)
	}
	return nil
}

// UpstreamMtlsRoundTrip asserts Envoy presents a client leaf to a
// mTLS upstream that verifies it against its own clientTrust CA.
func (t *Tests) UpstreamMtlsRoundTrip(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	serverCa, serverCaPwd, err := testCa(ctx, "upstream-mtls-server")
	if err != nil {
		return err
	}
	clientCa, _, err := testCa(ctx, "upstream-mtls-client")
	if err != nil {
		return err
	}
	upstream, err := pythonHttpsUpstream(ctx, serverCa, mark, 5680, "mtls-upstream", clientCa.CertPemFile())
	if err != nil {
		return err
	}

	// Mint a client leaf signed by clientCa, packaged as PKCS#12 for
	// Envoy's MtlsUpstreamSecurity keyStore.
	leafKeyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return err
	}
	leafKey := dag.SetSecret("upstream-mtls-envoy-leaf-key", leafKeyPem)
	leafPwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return err
	}
	leafPwd := dag.SetSecret("upstream-mtls-envoy-leaf-pwd", leafPwdHex)
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return err
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	envoyClient := clientCa.IssueClientCertificate("envoy-upstream-client", nb, serial, leafPwd, leafKey,
		dagger.CertificateManagementCertificateAuthorityIssueClientCertificateOpts{ValidityDays: 365})

	e := dag.Envoy()
	upSec := e.MtlsUpstreamSecurity(envoyClient.KeyStore().Pkcs12(), leafPwd, serverCa.TrustStore().Pkcs12(), serverCaPwd)
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "secured")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18080, hcm)
	cluster := e.Cluster("secured", dagger.EnvoyClusterOpts{Upstream: upSec}).
		WithEndpoint(e.Endpoint("mtls-upstream", 5680))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("mtls-upstream", upstream).
		WithListener(listener).
		WithCluster(cluster).
		Service()

	_, err = dag.Container().From(curlImage).
		WithServiceBinding("envoy", svc).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  BODY=$(curl -sS http://envoy:18080/ || true)
  case "${BODY}" in *"${MARKER}"*) echo "marker observed after ${i}s"; exit 0 ;; esac
  sleep 1
done
echo "marker ${MARKER} never appeared via upstream MTLS; last body: ${BODY}" >&2
exit 1
`}).Sync(ctx)
	if err != nil {
		return fmt.Errorf("UpstreamMtls round-trip: %w", err)
	}
	return nil
}

// L4TcpTlsRoundTrip stands up an nc echo upstream behind an Envoy
// TLS-terminated TcpListener and asserts an openssl s_client probe
// can complete a TLS round-trip with marker bytes echoed back.
func (t *Tests) L4TcpTlsRoundTrip(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	upstream := dag.Container().From(probeImage).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-extras"}).
		WithExposedPort(5000).
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{"sh", "-c", "while true; do nc -l -p 5000 -e /bin/cat; done"},
		})

	ca, caPwd, err := testCa(ctx, "l4tcp-tls")
	if err != nil {
		return err
	}
	e := dag.Envoy()
	sec := e.TLSServerSecurity(ca.KeyStore().Pkcs12(), caPwd)
	tcp := e.TCPProxy("tcp", "upstream")
	listener := e.TCPListener("ingress", 14443, tcp, dagger.EnvoyTCPListenerOpts{Security: sec})
	cluster := e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5000))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("upstream", upstream).
		WithListener(listener).
		WithCluster(cluster).
		Service()

	_, err = dag.Container().From(probeImage).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-extras", "openssl"}).
		WithServiceBinding("envoy", svc).
		WithFile("/certs/ca.pem", ca.CertPemFile(), dagger.ContainerWithFileOpts{Permissions: 0o644}).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  OUT=$(printf "%s" "${MARKER}" | timeout 5 openssl s_client -quiet -connect envoy:14443 -CAfile /certs/ca.pem -verify_return_error 2>/dev/null || true)
  case "${OUT}" in *"${MARKER}"*) echo "marker echoed via TLS after ${i}s"; exit 0 ;; esac
  sleep 1
done
echo "marker ${MARKER} never echoed back through TLS; last out: ${OUT}" >&2
exit 1
`}).Sync(ctx)
	if err != nil {
		return fmt.Errorf("L4TcpTls round-trip: %w", err)
	}
	return nil
}
