// TLS + mTLS coverage for the otel module (issue #24): receiver-side
// WithTls/WithMtls, exporter-side TLS options, and an end-to-end TLS hop.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
	"gopkg.in/yaml.v3"
)

// Tls runs the otel TLS/mTLS suite: receiver-side TLS enforcement,
// exporter-side TLS rendering, and an end-to-end TLS pipeline hop that
// lands in Loki. Carries its own +check so CI schedules it on its own
// runner alongside Validation/Core/Contrib.
//
// +check
// +cache="session"
func (t *Tests) Tls(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
	// +default="3.4.1"
	lokiTag string,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("WithTlsInjectsReceiverTls", func(ctx context.Context) error {
		return t.WithTlsInjectsReceiverTls(ctx, collectorTag)
	})
	jobs = jobs.WithJob("OtlpExporterTlsRendersCaFile", func(ctx context.Context) error {
		return t.OtlpExporterTlsRendersCaFile(ctx, collectorTag)
	})
	jobs = jobs.WithJob("OtlpExporterRejectsClientCertWithoutKey", t.OtlpExporterRejectsClientCertWithoutKey)
	jobs = jobs.WithJob("WithTlsAcceptsTlsRejectsPlaintext", func(ctx context.Context) error {
		return t.WithTlsAcceptsTlsRejectsPlaintext(ctx, collectorTag)
	})
	jobs = jobs.WithJob("WithMtlsRejectsWithoutClientCert", func(ctx context.Context) error {
		return t.WithMtlsRejectsWithoutClientCert(ctx, collectorTag)
	})
	jobs = jobs.WithJob("TlsPipelineForwardsToLoki", func(ctx context.Context) error {
		return t.TlsPipelineForwardsToLoki(ctx, collectorTag, lokiTag)
	})
	return jobs.Run(ctx)
}

// WithTlsInjectsReceiverTls asserts WithTls (and WithMtls) splice a `tls:`
// block referencing the fixed server cert/key (and client CA) mount paths
// into both the grpc and http protocols of every otlp receiver in the
// rendered config. Render-only: no service is stood up, so dummy material
// suffices.
func (t *Tests) WithTlsInjectsReceiverTls(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	cert, key := dummyTlsMaterial()
	o := dag.Otel()
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithTLS(cert, key).
		WithMtls(cert).
		WithPipeline(o.DebugPipeline("logs"))

	contents, err := col.ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg struct {
		Receivers map[string]struct {
			Protocols map[string]struct {
				TLS map[string]string `yaml:"tls"`
			} `yaml:"protocols"`
		} `yaml:"receivers"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return fmt.Errorf("parse rendered yaml: %w\n---\n%s", err, contents)
	}
	recv, ok := cfg.Receivers["otlp/debug"]
	if !ok {
		return fmt.Errorf("missing receivers.otlp/debug, got: %v\n---\n%s", cfg.Receivers, contents)
	}
	for _, proto := range []string{"grpc", "http"} {
		tls := recv.Protocols[proto].TLS
		if tls["cert_file"] != "/etc/otelcol/tls/server-cert.pem" {
			return fmt.Errorf("%s: expected cert_file server-cert.pem, got %q\n---\n%s", proto, tls["cert_file"], contents)
		}
		if tls["key_file"] != "/etc/otelcol/tls/server-key.pem" {
			return fmt.Errorf("%s: expected key_file server-key.pem, got %q\n---\n%s", proto, tls["key_file"], contents)
		}
		if tls["client_ca_file"] != "/etc/otelcol/tls/client-ca.pem" {
			return fmt.Errorf("%s: expected client_ca_file client-ca.pem, got %q\n---\n%s", proto, tls["client_ca_file"], contents)
		}
	}
	return nil
}

// OtlpExporterTlsRendersCaFile asserts that supplying CaCert to
// OtlpHttpExporter renders a tls.ca_file pointing at the per-component
// mount path and drops the plaintext insecure flag.
func (t *Tests) OtlpExporterTlsRendersCaFile(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	cert, _ := dummyTlsMaterial()
	o := dag.Otel()
	exp := o.OtlpHTTPExporter("recv", "https://recv:4318", dagger.OtelOtlpHTTPExporterOpts{CaCert: cert})
	p := o.Pipeline("logs", "p").
		WithReceiver(o.OtlpReceiver("in")).
		WithExporter(exp)

	contents, err := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithPipeline(p).ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg struct {
		Exporters map[string]struct {
			TLS map[string]any `yaml:"tls"`
		} `yaml:"exporters"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return fmt.Errorf("parse rendered yaml: %w\n---\n%s", err, contents)
	}
	exporter, ok := cfg.Exporters["otlphttp/recv"]
	if !ok {
		return fmt.Errorf("missing exporters.otlphttp/recv, got: %v\n---\n%s", cfg.Exporters, contents)
	}
	if got := fmt.Sprintf("%v", exporter.TLS["ca_file"]); got != "/etc/otelcol/tls/exporters/otlphttp_recv/ca.pem" {
		return fmt.Errorf("expected ca_file exporters/otlphttp_recv/ca.pem, got %q\n---\n%s", got, contents)
	}
	if _, present := exporter.TLS["insecure"]; present {
		return fmt.Errorf("TLS exporter should not set insecure, got: %v\n---\n%s", exporter.TLS, contents)
	}
	return nil
}

// OtlpExporterRejectsClientCertWithoutKey asserts the factories reject a
// client cert supplied without its key (and vice versa), since an mTLS
// identity needs both halves.
func (t *Tests) OtlpExporterRejectsClientCertWithoutKey(ctx context.Context) error {
	cert, key := dummyTlsMaterial()
	o := dag.Otel()
	if _, err := o.OtlpExporter("x", "recv:4317", dagger.OtelOtlpExporterOpts{ClientCert: cert}).ID(ctx); err == nil {
		return fmt.Errorf("OtlpExporter with clientCert but no clientKey: expected error, got nil")
	}
	if _, err := o.OtlpHTTPExporter("x", "https://recv:4318", dagger.OtelOtlpHTTPExporterOpts{ClientKey: key}).ID(ctx); err == nil {
		return fmt.Errorf("OtlpHTTPExporter with clientKey but no clientCert: expected error, got nil")
	}
	if _, err := o.OtlpExporter("x", "recv:4317", dagger.OtelOtlpExporterOpts{ClientCert: cert, ClientKey: key}).ID(ctx); err != nil {
		return fmt.Errorf("OtlpExporter with both clientCert+clientKey: expected nil, got %w", err)
	}
	return nil
}

// WithTlsAcceptsTlsRejectsPlaintext asserts a WithTls collector accepts an
// OTLP/HTTP push over TLS (client verifying against the issuing CA) and
// rejects a plaintext push on the same port.
func (t *Tests) WithTlsAcceptsTlsRejectsPlaintext(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	assets, err := mintTls(ctx, "col")
	if err != nil {
		return err
	}
	o := dag.Otel()
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithTLS(assets.serverCert, assets.serverKey).
		WithPipeline(o.DebugPipeline("logs"))

	script := `
set -eu
NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"otel-tests"}}]},
"scopeLogs":[{"scope":{"name":"otel-tests"},"logRecords":[
{"timeUnixNano":"${NS_NANOS}","severityNumber":9,"severityText":"INFO","body":{"stringValue":"${MARKER}"}}]}]}]}
EOF
)
# 1. TLS push must be accepted.
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem -o /tmp/o -w '%{http_code}' \
    -X POST -H 'content-type: application/json' --data "${PAYLOAD}" \
    https://col:4318/v1/logs || echo 000)
  case "${CODE}" in 200|204) echo "tls push accepted (HTTP ${CODE}) after ${ATTEMPT}s"; break ;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then echo "tls push never accepted; last HTTP=${CODE}" >&2; cat /tmp/o >&2 || true; exit 1; fi
# 2. Plaintext push on the same port must be refused: a TLS listener
# answers a cleartext request with HTTP 400 ("Client sent an HTTP request
# to an HTTPS server"), never a 2xx accept.
CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' \
  -X POST -H 'content-type: application/json' --data "${PAYLOAD}" \
  http://col:4318/v1/logs 2>/dev/null || echo 000)
case "${CODE}" in
  200|204) echo "plaintext push unexpectedly accepted (HTTP ${CODE}) by a TLS-only receiver" >&2; exit 1 ;;
esac
echo "plaintext push correctly rejected (HTTP ${CODE})"
exit 0
`
	_, err = dag.Container().From(curlImage).
		WithUser("0:0").
		WithServiceBinding("col", col.Service()).
		WithMountedFile("/tls/ca.pem", assets.caCert).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("tls-vs-plaintext push: %w", err)
	}
	return nil
}

// WithMtlsRejectsWithoutClientCert asserts a WithTls+WithMtls collector
// accepts a push presenting a client cert signed by the trusted CA and
// rejects one that presents none.
func (t *Tests) WithMtlsRejectsWithoutClientCert(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	assets, err := mintTls(ctx, "col")
	if err != nil {
		return err
	}
	o := dag.Otel()
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithTLS(assets.serverCert, assets.serverKey).
		WithMtls(assets.caCert).
		WithPipeline(o.DebugPipeline("logs"))

	script := `
set -eu
NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"otel-tests"}}]},
"scopeLogs":[{"scope":{"name":"otel-tests"},"logRecords":[
{"timeUnixNano":"${NS_NANOS}","severityNumber":9,"severityText":"INFO","body":{"stringValue":"${MARKER}"}}]}]}]}
EOF
)
# 1. Push presenting a valid client cert must be accepted.
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem --cert /tls/client.pem --key /tls/client.key \
    -o /tmp/o -w '%{http_code}' \
    -X POST -H 'content-type: application/json' --data "${PAYLOAD}" \
    https://col:4318/v1/logs || echo 000)
  case "${CODE}" in 200|204) echo "mtls push accepted (HTTP ${CODE}) after ${ATTEMPT}s"; break ;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then echo "mtls push never accepted; last HTTP=${CODE}" >&2; cat /tmp/o >&2 || true; exit 1; fi
# 2. Push presenting no client cert must be refused: the mTLS handshake
# fails (no HTTP response, code 000), so the one thing we must never see
# is a 2xx accept.
CODE=$(curl -sS --max-time 10 --cacert /tls/ca.pem -o /dev/null -w '%{http_code}' \
  -X POST -H 'content-type: application/json' --data "${PAYLOAD}" \
  https://col:4318/v1/logs 2>/dev/null || echo 000)
case "${CODE}" in
  200|204) echo "push without a client cert unexpectedly accepted (HTTP ${CODE}) by an mTLS receiver" >&2; exit 1 ;;
esac
echo "clientless push correctly rejected (HTTP ${CODE})"
exit 0
`
	_, err = dag.Container().From(curlImage).
		WithUser("0:0").
		WithServiceBinding("col", col.Service()).
		WithMountedFile("/tls/ca.pem", assets.caCert).
		WithMountedFile("/tls/client.pem", assets.clientCert).
		WithMountedSecret("/tls/client.key", assets.clientKey).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("mtls client-cert enforcement: %w", err)
	}
	return nil
}

// TlsPipelineForwardsToLoki is the end-to-end TLS pipeline test: a
// plaintext edge collector forwards over a TLS hop (its OtlpHttpExporter
// pins the downstream CA) to a WithTls receiver collector, which relays
// the logs to Loki where they are queryable. Exercises exporter-side TLS
// (criterion 3) and a full TLS pipeline (criterion 4) with an observable
// sink.
func (t *Tests) TlsPipelineForwardsToLoki(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
	// +default="3.4.1"
	lokiTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	assets, err := mintTls(ctx, "recv")
	if err != nil {
		return err
	}
	loki := dag.GrafanaStack().Loki(dagger.GrafanaStackLokiOpts{Tag: lokiTag})
	o := dag.Otel()

	// Downstream: TLS receiver relaying to Loki over plaintext.
	recv := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithServiceBinding("loki", loki.Service()).
		WithTLS(assets.serverCert, assets.serverKey).
		WithPipeline(o.Pipeline("logs", "p").
			WithReceiver(o.OtlpReceiver("in")).
			WithExporter(o.OtlpHTTPExporter("loki", "http://loki:3100/otlp")))

	// Edge: plaintext receiver forwarding to recv over a TLS hop.
	edge := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithServiceBinding("recv", recv.Service()).
		WithPipeline(o.Pipeline("logs", "p").
			WithReceiver(o.OtlpReceiver("in")).
			WithExporter(o.OtlpHTTPExporter("recv", "https://recv:4318",
				dagger.OtelOtlpHTTPExporterOpts{CaCert: assets.caCert})))

	return lokiRoundTrip(ctx, edge.Service(), loki.Service(), mark)
}

// --- helpers ---

// tlsAssets bundles CA + server + client PEM material minted for one test.
type tlsAssets struct {
	caCert     *dagger.File   // CA cert PEM — trust anchor and mTLS client CA
	serverCert *dagger.File   // server leaf cert PEM
	serverKey  *dagger.Secret // server leaf key PEM (PKCS#8)
	clientCert *dagger.File   // client leaf cert PEM
	clientKey  *dagger.Secret // client leaf key PEM (PKCS#8)
}

// mintTls stands up a fresh CA and issues a server leaf (with host as its
// DNS SAN) plus a client leaf, all via the certificate-management module.
func mintTls(ctx context.Context, host string) (*tlsAssets, error) {
	caKey, err := newKey(ctx, "otel-ca")
	if err != nil {
		return nil, err
	}
	caPwd, err := newPassword(ctx, "otel-ca-pwd")
	if err != nil {
		return nil, err
	}
	caSerial, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	ca := dag.CertificateManagement().CreateCertificateAuthority(nowRfc3339(), caSerial, caPwd, caKey)

	serverKey, err := newKey(ctx, "otel-server")
	if err != nil {
		return nil, err
	}
	serverPwd, err := newPassword(ctx, "otel-server-pwd")
	if err != nil {
		return nil, err
	}
	serverSerial, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	server := ca.IssueServerCertificate(host, nowRfc3339(), serverSerial, serverPwd, serverKey,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{DNSSans: []string{host}})

	clientKey, err := newKey(ctx, "otel-client")
	if err != nil {
		return nil, err
	}
	clientPwd, err := newPassword(ctx, "otel-client-pwd")
	if err != nil {
		return nil, err
	}
	clientSerial, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	client := ca.IssueClientCertificate("otel-client", nowRfc3339(), clientSerial, clientPwd, clientKey)

	return &tlsAssets{
		caCert:     ca.CertPemFile(),
		serverCert: server.CertPemFile(),
		serverKey:  server.PrivateKeyPem(),
		clientCert: client.CertPemFile(),
		clientKey:  client.PrivateKeyPem(),
	}, nil
}

// dummyTlsMaterial returns throwaway cert/key handles for render-only
// tests, which inspect the emitted YAML paths and never read the bytes.
func dummyTlsMaterial() (*dagger.File, *dagger.Secret) {
	f := dag.Directory().WithNewFile("cert.pem", "dummy-cert").File("cert.pem")
	s := dag.SetSecret("otel-dummy-key", "dummy-key")
	return f, s
}

// newKey mints a fresh PKCS#8 PEM ECDSA P-256 private key via the crypto
// module and wraps it as a *dagger.Secret. PEM is text, so File.Contents()
// is safe here.
func newKey(ctx context.Context, name string) (*dagger.Secret, error) {
	contents, err := dag.Crypto().GenerateEcdsaP256Key().Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read generated key: %w", err)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(name+"-"+suffix, contents), nil
}

// newPassword mints a fresh PKCS#12 password by hashing random bytes via
// the random module and wrapping the hex string as a Dagger secret.
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

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random hex: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// nowRfc3339 returns the current UTC time in RFC3339 form for the
// notBefore / cache-busting inputs on the certificate-management API.
func nowRfc3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
