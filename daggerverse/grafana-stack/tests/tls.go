// TLS / mTLS round-trip checks for the grafana-stack module. Each backend
// (Loki, Tempo, Mimir) is exercised with WithTls (one-way TLS) and WithMtls
// (mutual TLS); Grafana is exercised with WithTls; and the Grafana datasource
// proxy is exercised end-to-end against a TLS and an mTLS Loki backend.
//
// Cert material is minted per test from the certificate-management + crypto
// modules so nothing is hard-coded. Every test carries +check so CI schedules
// each on its own runner, and +cache="never" so a re-run always re-exercises
// the live handshake.
package main

import (
	"context"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"
)

// tlsStack is the cert material for a single-backend TLS/mTLS test: the CA
// trust anchor, the backend's server leaf (+ key), and a client leaf (+ key)
// signed by the same CA.
type tlsStack struct {
	caCert     *dagger.File
	serverCert *dagger.File
	serverKey  *dagger.Secret
	clientCert *dagger.File
	clientKey  *dagger.Secret
}

// nowRFC3339 is the not-before stamp for freshly minted certs.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// newKey generates a fresh RSA key and returns it PEM-encoded as a uniquely
// named secret (the random suffix keeps distinct keys from colliding).
func newKey(ctx context.Context, name string) (*dagger.Secret, error) {
	pem, err := dag.Crypto().
		GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).
		Pem().
		Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read generated key: %w", err)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(name+"-"+suffix, pem), nil
}

// newPassword returns a throwaway PKCS#12 password (the cert-management
// issuers require one but the tests consume PEM directly).
func newPassword(ctx context.Context, name string) (*dagger.Secret, error) {
	pwd, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(name+"-"+suffix, pwd), nil
}

// certAuthority bundles a CA and its PEM certificate for repeated issuance.
type certAuthority struct {
	authority *dagger.CertificateManagementCertificateAuthority
	certFile  *dagger.File
}

// newCA mints a fresh root CA.
func newCA(ctx context.Context, label string) (*certAuthority, error) {
	key, err := newKey(ctx, label+"-ca")
	if err != nil {
		return nil, err
	}
	pwd, err := newPassword(ctx, label+"-ca-pwd")
	if err != nil {
		return nil, err
	}
	serial, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	authority := dag.CertificateManagement().CreateCertificateAuthority(nowRFC3339(), serial, pwd, key)
	return &certAuthority{authority: authority, certFile: authority.CertPemFile()}, nil
}

// serverCert issues a server leaf whose SAN covers host.
func (c *certAuthority) serverCert(ctx context.Context, host string) (*dagger.File, *dagger.Secret, error) {
	key, err := newKey(ctx, host+"-srv")
	if err != nil {
		return nil, nil, err
	}
	pwd, err := newPassword(ctx, host+"-srv-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomHex(16)
	if err != nil {
		return nil, nil, err
	}
	issued := c.authority.IssueServerCertificate(host, nowRFC3339(), serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans: []string{host},
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// clientCert issues a client leaf with the given common name.
func (c *certAuthority) clientCert(ctx context.Context, cn string) (*dagger.File, *dagger.Secret, error) {
	key, err := newKey(ctx, cn+"-cli")
	if err != nil {
		return nil, nil, err
	}
	pwd, err := newPassword(ctx, cn+"-cli-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomHex(16)
	if err != nil {
		return nil, nil, err
	}
	issued := c.authority.IssueClientCertificate(cn, nowRFC3339(), serial, pwd, key)
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// mintStack mints a CA, a server leaf for host, and a client leaf in one go.
func mintStack(ctx context.Context, host string) (*tlsStack, error) {
	ca, err := newCA(ctx, host)
	if err != nil {
		return nil, err
	}
	serverCert, serverKey, err := ca.serverCert(ctx, host)
	if err != nil {
		return nil, err
	}
	clientCert, clientKey, err := ca.clientCert(ctx, host+"-client")
	if err != nil {
		return nil, err
	}
	return &tlsStack{
		caCert:     ca.certFile,
		serverCert: serverCert,
		serverKey:  serverKey,
		clientCert: clientCert,
		clientKey:  clientKey,
	}, nil
}

// tlsClient builds a curl container bound to a backend service with the CA
// (and, when withClient is set, a client cert/key) mounted, exporting
// CURL_OPTS so scripts pass the right TLS flags without re-typing paths.
func tlsClient(svcName string, svc *dagger.Service, s *tlsStack, withClient bool) *dagger.Container {
	ctr := dag.Container().From(curlImage).
		WithUser("0:0").
		WithServiceBinding(svcName, svc).
		WithMountedFile("/tls/ca.pem", s.caCert)
	opts := "--cacert /tls/ca.pem"
	if withClient {
		ctr = ctr.
			WithMountedFile("/tls/client.crt", s.clientCert).
			WithMountedSecret("/tls/client.key", s.clientKey)
		opts += " --cert /tls/client.crt --key /tls/client.key"
	}
	return ctr.WithEnvVariable("CURL_OPTS", opts)
}

// LokiTlsRoundTrip stands up a TLS-enabled Loki, pushes an OTLP log over
// https, reads the marker back over https, and asserts a plaintext client is
// refused.
//
// +check
// +cache="never"
func (t *Tests) LokiTlsRoundTrip(
	ctx context.Context,
	// +default="3.4.1"
	tag string,
) error {
	marker, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return fmt.Errorf("generate marker: %w", err)
	}
	s, err := mintStack(ctx, "loki")
	if err != nil {
		return err
	}
	loki := dag.GrafanaStack().
		Loki(dagger.GrafanaStackLokiOpts{Tag: tag}).
		WithTLS(s.serverCert, s.serverKey)

	script := `set -eu
wait_ready() {
  ATTEMPT=0
  while [ "${ATTEMPT}" -lt 120 ]; do
    if curl -fsS ${CURL_OPTS} https://loki:3100/ready >/dev/null 2>&1; then
      echo "loki ready after ${ATTEMPT}s"; return 0
    fi
    ATTEMPT=$((ATTEMPT + 1)); sleep 1
  done
  echo "loki not ready" >&2; return 1
}
wait_ready

NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"grafana-stack-test"}}]},"scopeLogs":[{"scope":{"name":"grafana-stack-tests"},"logRecords":[{"timeUnixNano":"${NS_NANOS}","severityNumber":9,"severityText":"INFO","body":{"stringValue":"${MARKER}"}}]}]}]}
EOF
)

ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  CODE=$(curl -sS ${CURL_OPTS} -o /tmp/o -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://loki:3100/otlp/v1/logs || echo 000)
  case "${CODE}" in 200|204) echo "tls push accepted (${CODE})"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then echo "loki rejected tls push; last=${CODE}" >&2; cat /tmp/o >&2 || true; exit 1; fi

sleep 2
QUERY='{service_name="grafana-stack-test"}'
NOW=$(date +%s); END="${NOW}000000000"; START=$((NOW - 600)); START="${START}000000000"
ATTEMPT=0
while [ "${ATTEMPT}" -lt 30 ]; do
  RESP=$(curl -fsS ${CURL_OPTS} --get --data-urlencode "query=${QUERY}" --data-urlencode "start=${START}" --data-urlencode "end=${END}" --data-urlencode 'limit=100' https://loki:3100/loki/api/v1/query_range || true)
  case "${RESP}" in *"${MARKER}"*) echo "marker observed over tls"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
  if [ "${ATTEMPT}" -ge 30 ]; then echo "marker never appeared over tls" >&2; echo "${RESP}" >&2; exit 1; fi
done

# A plaintext client against the TLS listener must never get a 2xx.
CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' http://loki:3100/ready 2>/dev/null || echo 000)
case "${CODE}" in 200) echo "plaintext unexpectedly accepted by TLS loki" >&2; exit 1;; esac
echo "plaintext correctly rejected (${CODE})"
`

	_, err = tlsClient("loki", loki.Service(), s, false).
		WithEnvVariable("MARKER", marker).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("loki tls round-trip: %w", err)
	}
	return nil
}

// LokiMtlsRequiresClientCert stands up an mTLS-required Loki and asserts a
// push presenting a valid client cert is accepted while one without any client
// cert is refused.
//
// +check
// +cache="never"
func (t *Tests) LokiMtlsRequiresClientCert(
	ctx context.Context,
	// +default="3.4.1"
	tag string,
) error {
	s, err := mintStack(ctx, "loki")
	if err != nil {
		return err
	}
	loki := dag.GrafanaStack().
		Loki(dagger.GrafanaStackLokiOpts{Tag: tag}).
		WithTLS(s.serverCert, s.serverKey).
		WithMtls(s.caCert)

	script := `set -eu
PAYLOAD='{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"mtls-probe"}}]}]}]}'

# 1. With a valid client cert the mTLS listener must accept (retry through startup).
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem --cert /tls/client.crt --key /tls/client.key -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://loki:3100/otlp/v1/logs || echo 000)
  case "${CODE}" in 200|204) echo "mtls push accepted (${CODE})"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "mtls push never accepted; last=${CODE}" >&2; exit 1; fi

# 2. Without a client cert the handshake must fail: never a 2xx.
CODE=$(curl -sS --max-time 10 --cacert /tls/ca.pem -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://loki:3100/otlp/v1/logs 2>/dev/null || echo 000)
case "${CODE}" in 200|204) echo "clientless push unexpectedly accepted (${CODE})" >&2; exit 1;; esac
echo "clientless push correctly rejected (${CODE})"
`

	_, err = tlsClient("loki", loki.Service(), s, true).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("loki mtls: %w", err)
	}
	return nil
}

// TempoTlsRoundTrip stands up a TLS-enabled Tempo, pushes a span over the
// https OTLP receiver, reads it back over the https query API, and asserts a
// plaintext client is refused.
//
// +check
// +cache="never"
func (t *Tests) TempoTlsRoundTrip(
	ctx context.Context,
	// +default="2.7.1"
	tag string,
) error {
	traceIDHex, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("generate trace id: %w", err)
	}
	spanIDHex, spanIDB64, err := randomIDPair(8)
	if err != nil {
		return fmt.Errorf("generate span id: %w", err)
	}
	s, err := mintStack(ctx, "tempo")
	if err != nil {
		return err
	}
	tempo := dag.GrafanaStack().
		Tempo(dagger.GrafanaStackTempoOpts{Tag: tag}).
		WithTLS(s.serverCert, s.serverKey)

	script := `set -eu
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  if curl -fsS ${CURL_OPTS} https://tempo:3200/ready >/dev/null 2>&1; then echo "tempo ready"; break; fi
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "tempo not ready" >&2; exit 1; fi

START_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"grafana-stack-test"}}]},"scopeSpans":[{"scope":{"name":"grafana-stack-tests"},"spans":[{"traceId":"${TRACE_ID_HEX}","spanId":"${SPAN_ID_HEX}","name":"round-trip","kind":1,"startTimeUnixNano":"${START_NANOS}","endTimeUnixNano":"${START_NANOS}"}]}]}]}
EOF
)

ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  CODE=$(curl -sS ${CURL_OPTS} -o /tmp/o -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://tempo:4318/v1/traces || echo 000)
  case "${CODE}" in 200|204) echo "tls span push accepted (${CODE})"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then echo "tempo rejected tls push; last=${CODE}" >&2; cat /tmp/o >&2 || true; exit 1; fi

sleep 2
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  CODE=$(curl -sS ${CURL_OPTS} -o /tmp/g -w '%{http_code}' "https://tempo:3200/api/traces/${TRACE_ID_HEX}" || echo 000)
  if [ "${CODE}" = "200" ]; then
    case "$(cat /tmp/g)" in *"${SPAN_ID_B64}"*) echo "trace read back over tls"; break;; esac
  fi
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
  if [ "${ATTEMPT}" -ge 60 ]; then echo "trace never returned over tls; last=${CODE}" >&2; cat /tmp/g >&2 || true; exit 1; fi
done

CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' http://tempo:3200/ready 2>/dev/null || echo 000)
case "${CODE}" in 200) echo "plaintext unexpectedly accepted by TLS tempo" >&2; exit 1;; esac
echo "plaintext correctly rejected (${CODE})"
`

	_, err = tlsClient("tempo", tempo.Service(), s, false).
		WithEnvVariable("TRACE_ID_HEX", traceIDHex).
		WithEnvVariable("SPAN_ID_HEX", spanIDHex).
		WithEnvVariable("SPAN_ID_B64", spanIDB64).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("tempo tls round-trip: %w", err)
	}
	return nil
}

// TempoMtlsRequiresClientCert asserts an mTLS-required Tempo accepts an OTLP
// span push presenting a valid client cert and refuses one without.
//
// +check
// +cache="never"
func (t *Tests) TempoMtlsRequiresClientCert(
	ctx context.Context,
	// +default="2.7.1"
	tag string,
) error {
	s, err := mintStack(ctx, "tempo")
	if err != nil {
		return err
	}
	tempo := dag.GrafanaStack().
		Tempo(dagger.GrafanaStackTempoOpts{Tag: tag}).
		WithTLS(s.serverCert, s.serverKey).
		WithMtls(s.caCert)

	script := `set -eu
NOW="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331","name":"p","kind":1,"startTimeUnixNano":"${NOW}","endTimeUnixNano":"${NOW}"}]}]}]}
EOF
)

ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem --cert /tls/client.crt --key /tls/client.key -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://tempo:4318/v1/traces || echo 000)
  case "${CODE}" in 200|204) echo "mtls span push accepted (${CODE})"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "mtls span push never accepted; last=${CODE}" >&2; exit 1; fi

CODE=$(curl -sS --max-time 10 --cacert /tls/ca.pem -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://tempo:4318/v1/traces 2>/dev/null || echo 000)
case "${CODE}" in 200|204) echo "clientless span push unexpectedly accepted (${CODE})" >&2; exit 1;; esac
echo "clientless span push correctly rejected (${CODE})"
`

	_, err = tlsClient("tempo", tempo.Service(), s, true).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("tempo mtls: %w", err)
	}
	return nil
}

// MimirTlsRoundTrip stands up a TLS-enabled Mimir, pushes an OTLP metric over
// https, queries it back over https, and asserts a plaintext client is
// refused.
//
// +check
// +cache="never"
func (t *Tests) MimirTlsRoundTrip(
	ctx context.Context,
	// +default="2.15.1"
	tag string,
) error {
	suffix, err := randomHex(8)
	if err != nil {
		return fmt.Errorf("generate metric suffix: %w", err)
	}
	metricName := "grafana_stack_test_marker_" + suffix
	s, err := mintStack(ctx, "mimir")
	if err != nil {
		return err
	}
	mimir := dag.GrafanaStack().
		Mimir(dagger.GrafanaStackMimirOpts{Tag: tag}).
		WithTLS(s.serverCert, s.serverKey)

	script := `set -eu
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  if curl -fsS ${CURL_OPTS} https://mimir:9009/ready >/dev/null 2>&1; then echo "mimir ready"; break; fi
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "mimir not ready" >&2; exit 1; fi

NOW=$(date +%s); TIME_NANOS="${NOW}000000000"
PAYLOAD=$(cat <<EOF
{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"grafana-stack-test"}}]},"scopeMetrics":[{"scope":{"name":"grafana-stack-tests"},"metrics":[{"name":"${METRIC_NAME}","unit":"1","gauge":{"dataPoints":[{"timeUnixNano":"${TIME_NANOS}","asDouble":1.0}]}}]}]}]}
EOF
)

ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  CODE=$(curl -sS ${CURL_OPTS} -o /tmp/o -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://mimir:9009/otlp/v1/metrics || echo 000)
  case "${CODE}" in 200|204) echo "tls metric push accepted (${CODE})"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then echo "mimir rejected tls push; last=${CODE}" >&2; cat /tmp/o >&2 || true; exit 1; fi

sleep 2
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  RESP=$(curl -fsS ${CURL_OPTS} --get --data-urlencode "query=${METRIC_NAME}" "https://mimir:9009/prometheus/api/v1/query" || true)
  case "${RESP}" in *"\"${METRIC_NAME}\""*) echo "metric observed over tls"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
  if [ "${ATTEMPT}" -ge 60 ]; then echo "metric never appeared over tls" >&2; echo "${RESP}" >&2; exit 1; fi
done

CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' http://mimir:9009/ready 2>/dev/null || echo 000)
case "${CODE}" in 200) echo "plaintext unexpectedly accepted by TLS mimir" >&2; exit 1;; esac
echo "plaintext correctly rejected (${CODE})"
`

	_, err = tlsClient("mimir", mimir.Service(), s, false).
		WithEnvVariable("METRIC_NAME", metricName).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("mimir tls round-trip: %w", err)
	}
	return nil
}

// MimirMtlsRequiresClientCert asserts an mTLS-required Mimir accepts an OTLP
// metric push presenting a valid client cert and refuses one without.
//
// +check
// +cache="never"
func (t *Tests) MimirMtlsRequiresClientCert(
	ctx context.Context,
	// +default="2.15.1"
	tag string,
) error {
	s, err := mintStack(ctx, "mimir")
	if err != nil {
		return err
	}
	mimir := dag.GrafanaStack().
		Mimir(dagger.GrafanaStackMimirOpts{Tag: tag}).
		WithTLS(s.serverCert, s.serverKey).
		WithMtls(s.caCert)

	script := `set -eu
NOW=$(date +%s); TIME_NANOS="${NOW}000000000"
PAYLOAD=$(cat <<EOF
{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"mtls_probe","unit":"1","gauge":{"dataPoints":[{"timeUnixNano":"${TIME_NANOS}","asDouble":1.0}]}}]}]}]}
EOF
)

ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem --cert /tls/client.crt --key /tls/client.key -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://mimir:9009/otlp/v1/metrics || echo 000)
  case "${CODE}" in 200|204) echo "mtls metric push accepted (${CODE})"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "mtls metric push never accepted; last=${CODE}" >&2; exit 1; fi

CODE=$(curl -sS --max-time 10 --cacert /tls/ca.pem -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://mimir:9009/otlp/v1/metrics 2>/dev/null || echo 000)
case "${CODE}" in 200|204) echo "clientless metric push unexpectedly accepted (${CODE})" >&2; exit 1;; esac
echo "clientless metric push correctly rejected (${CODE})"
`

	_, err = tlsClient("mimir", mimir.Service(), s, true).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("mimir mtls: %w", err)
	}
	return nil
}

// GrafanaTlsRejectsPlaintext stands up a TLS-enabled Grafana and asserts its
// :3000 listener answers over https and refuses a plaintext client.
//
// +check
// +cache="never"
func (t *Tests) GrafanaTlsRejectsPlaintext(
	ctx context.Context,
	// +default="12.0.0"
	grafanaTag string,
) error {
	pwd, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate admin password: %w", err)
	}
	adminPassword := dag.SetSecret("grafana-admin-password", pwd)
	s, err := mintStack(ctx, "grafana")
	if err != nil {
		return err
	}
	grafana := dag.GrafanaStack().
		Grafana(adminPassword, dagger.GrafanaStackGrafanaOpts{Tag: grafanaTag}).
		WithTLS(s.serverCert, s.serverKey)

	script := `set -eu
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  HEALTH=$(curl -fsS ${CURL_OPTS} https://grafana:3000/api/health || true)
  case "${HEALTH}" in *'"database": "ok"'*|*'"database":"ok"'*) echo "grafana ready over tls"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "grafana not ready over tls; last=${HEALTH}" >&2; exit 1; fi

# Plaintext against the https listener must never get a 2xx.
CODE=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' http://grafana:3000/api/health 2>/dev/null || echo 000)
case "${CODE}" in 200) echo "plaintext unexpectedly accepted by TLS grafana" >&2; exit 1;; esac
echo "plaintext correctly rejected (${CODE})"
`

	_, err = tlsClient("grafana", grafana.Service(), s, false).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("grafana tls: %w", err)
	}
	return nil
}

// GrafanaProxiesLokiQueryOverTls wires a TLS Grafana to a TLS Loki datasource
// (Grafana verifies Loki via the pinned CA), pushes a marker to Loki over
// https, and asserts an authenticated LogQL query *through* Grafana's
// datasource proxy (itself served over https) returns the marker.
//
// +check
// +cache="never"
func (t *Tests) GrafanaProxiesLokiQueryOverTls(
	ctx context.Context,
	// +default="3.4.1"
	lokiTag string,
	// +default="12.0.0"
	grafanaTag string,
) error {
	pwd, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate admin password: %w", err)
	}
	adminPassword := dag.SetSecret("grafana-admin-password", pwd)
	marker, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return fmt.Errorf("generate marker: %w", err)
	}

	ca, err := newCA(ctx, "gs-proxy-tls")
	if err != nil {
		return err
	}
	lokiCert, lokiKey, err := ca.serverCert(ctx, "loki")
	if err != nil {
		return err
	}
	grafanaCert, grafanaKey, err := ca.serverCert(ctx, "grafana")
	if err != nil {
		return err
	}

	loki := dag.GrafanaStack().
		Loki(dagger.GrafanaStackLokiOpts{Tag: lokiTag}).
		WithTLS(lokiCert, lokiKey)
	grafana := dag.GrafanaStack().
		Grafana(adminPassword, dagger.GrafanaStackGrafanaOpts{Tag: grafanaTag}).
		WithTLS(grafanaCert, grafanaKey).
		WithLokiDatasource("loki", loki, dagger.GrafanaStackGrafanaWithLokiDatasourceOpts{
			CaCert: ca.certFile,
		})

	script := lokiProxyScript
	_, err = dag.Container().From(curlImage).
		WithUser("0:0").
		WithServiceBinding("loki", loki.Service()).
		WithServiceBinding("grafana", grafana.Service()).
		WithMountedFile("/tls/ca.pem", ca.certFile).
		WithEnvVariable("MARKER", marker).
		WithEnvVariable("GRAFANA_PASSWORD", pwd).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("grafana proxies loki over tls: %w", err)
	}
	return nil
}

// GrafanaProxiesLokiQueryOverMtls wires a TLS Grafana to an mTLS-required Loki
// datasource: Grafana presents a client cert to reach Loki. It asserts the
// proxied query succeeds with the right client cert, and that a Grafana whose
// datasource presents a client cert from an untrusted CA fails.
//
// +check
// +cache="never"
func (t *Tests) GrafanaProxiesLokiQueryOverMtls(
	ctx context.Context,
	// +default="3.4.1"
	lokiTag string,
	// +default="12.0.0"
	grafanaTag string,
) error {
	pwd, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate admin password: %w", err)
	}
	adminPassword := dag.SetSecret("grafana-admin-password", pwd)
	marker, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return fmt.Errorf("generate marker: %w", err)
	}

	ca, err := newCA(ctx, "gs-proxy-mtls")
	if err != nil {
		return err
	}
	lokiCert, lokiKey, err := ca.serverCert(ctx, "loki")
	if err != nil {
		return err
	}
	grafanaCert, grafanaKey, err := ca.serverCert(ctx, "grafana")
	if err != nil {
		return err
	}
	clientCert, clientKey, err := ca.clientCert(ctx, "grafana-loki-client")
	if err != nil {
		return err
	}

	loki := dag.GrafanaStack().
		Loki(dagger.GrafanaStackLokiOpts{Tag: lokiTag}).
		WithTLS(lokiCert, lokiKey).
		WithMtls(ca.certFile)
	grafana := dag.GrafanaStack().
		Grafana(adminPassword, dagger.GrafanaStackGrafanaOpts{Tag: grafanaTag}).
		WithTLS(grafanaCert, grafanaKey).
		WithLokiDatasource("loki", loki, dagger.GrafanaStackGrafanaWithLokiDatasourceOpts{
			CaCert:     ca.certFile,
			ClientCert: clientCert,
			ClientKey:  clientKey,
		})

	// The client container pushes to Loki directly (presenting the trusted
	// client cert) and then queries through the Grafana proxy.
	pushAndQuery := `set -eu
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  if curl -fsS --cacert /tls/ca.pem --cert /tls/client.crt --key /tls/client.key https://loki:3100/ready >/dev/null 2>&1; then echo "loki ready"; break; fi
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "loki not ready" >&2; exit 1; fi

ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  HEALTH=$(curl -fsS --cacert /tls/ca.pem https://grafana:3000/api/health || true)
  case "${HEALTH}" in *'"database": "ok"'*|*'"database":"ok"'*) echo "grafana ready"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "grafana not ready; last=${HEALTH}" >&2; exit 1; fi

NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"grafana-stack-test"}}]},"scopeLogs":[{"scope":{"name":"grafana-stack-tests"},"logRecords":[{"timeUnixNano":"${NS_NANOS}","severityNumber":9,"severityText":"INFO","body":{"stringValue":"${MARKER}"}}]}]}]}
EOF
)
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem --cert /tls/client.crt --key /tls/client.key -o /tmp/o -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://loki:3100/otlp/v1/logs || echo 000)
  case "${CODE}" in 200|204) echo "mtls push accepted (${CODE})"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then echo "loki rejected mtls push; last=${CODE}" >&2; cat /tmp/o >&2 || true; exit 1; fi

sleep 2
QUERY='{service_name="grafana-stack-test"}'
NOW=$(date +%s); END="${NOW}000000000"; START=$((NOW - 600)); START="${START}000000000"
ATTEMPT=0
while [ "${ATTEMPT}" -lt 30 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem -o /tmp/g -w '%{http_code}' --get -u "admin:${GRAFANA_PASSWORD}" --data-urlencode "query=${QUERY}" --data-urlencode "start=${START}" --data-urlencode "end=${END}" --data-urlencode 'limit=100' 'https://grafana:3000/api/datasources/proxy/uid/loki/loki/api/v1/query_range' || echo 000)
  if [ "${CODE}" = "200" ]; then
    case "$(cat /tmp/g)" in *"${MARKER}"*) echo "marker observed through mtls proxy"; exit 0;; esac
  fi
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
echo "marker never appeared through mtls proxy; last=${CODE}" >&2; cat /tmp/g >&2 || true; exit 1
`

	_, err = dag.Container().From(curlImage).
		WithUser("0:0").
		WithServiceBinding("loki", loki.Service()).
		WithServiceBinding("grafana", grafana.Service()).
		WithMountedFile("/tls/ca.pem", ca.certFile).
		WithMountedFile("/tls/client.crt", clientCert).
		WithMountedSecret("/tls/client.key", clientKey).
		WithEnvVariable("MARKER", marker).
		WithEnvVariable("GRAFANA_PASSWORD", pwd).
		WithExec([]string{"sh", "-c", pushAndQuery}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("grafana proxies loki over mtls (positive): %w", err)
	}

	// Negative: a Grafana whose datasource presents a client cert from an
	// untrusted CA must fail to proxy (Loki rejects the handshake). We reuse
	// the same mTLS Loki; only the Grafana datasource client material differs.
	rogueCA, err := newCA(ctx, "gs-proxy-rogue")
	if err != nil {
		return err
	}
	rogueCert, rogueKey, err := rogueCA.clientCert(ctx, "rogue")
	if err != nil {
		return err
	}
	badGrafana := dag.GrafanaStack().
		Grafana(adminPassword, dagger.GrafanaStackGrafanaOpts{Tag: grafanaTag}).
		WithTLS(grafanaCert, grafanaKey).
		WithLokiDatasource("loki", loki, dagger.GrafanaStackGrafanaWithLokiDatasourceOpts{
			CaCert:     ca.certFile,
			ClientCert: rogueCert,
			ClientKey:  rogueKey,
		})

	negativeScript := `set -eu
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  HEALTH=$(curl -fsS --cacert /tls/ca.pem https://grafana:3000/api/health || true)
  case "${HEALTH}" in *'"database": "ok"'*|*'"database":"ok"'*) echo "grafana ready"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "grafana not ready; last=${HEALTH}" >&2; exit 1; fi

QUERY='{service_name="grafana-stack-test"}'
NOW=$(date +%s); END="${NOW}000000000"; START=$((NOW - 600)); START="${START}000000000"
# A rogue client cert must never yield a successful (200) proxied query.
CODE=$(curl -sS --max-time 20 --cacert /tls/ca.pem -o /dev/null -w '%{http_code}' --get -u "admin:${GRAFANA_PASSWORD}" --data-urlencode "query=${QUERY}" --data-urlencode "start=${START}" --data-urlencode "end=${END}" --data-urlencode 'limit=100' 'https://grafana:3000/api/datasources/proxy/uid/loki/loki/api/v1/query_range' 2>/dev/null || echo 000)
case "${CODE}" in 200) echo "rogue-client proxy unexpectedly succeeded (${CODE})" >&2; exit 1;; esac
echo "rogue-client proxy correctly failed (${CODE})"
`

	_, err = dag.Container().From(curlImage).
		WithUser("0:0").
		WithServiceBinding("loki", loki.Service()).
		WithServiceBinding("grafana", badGrafana.Service()).
		WithMountedFile("/tls/ca.pem", ca.certFile).
		WithEnvVariable("GRAFANA_PASSWORD", pwd).
		WithExec([]string{"sh", "-c", negativeScript}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("grafana proxies loki over mtls (negative): %w", err)
	}
	return nil
}

// lokiProxyScript waits for a TLS Loki + TLS Grafana, pushes a marker log to
// Loki over https, and reads it back through Grafana's https datasource proxy.
// Shared by the one-way-TLS proxy test. The CA is mounted at /tls/ca.pem.
const lokiProxyScript = `set -eu
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  if curl -fsS --cacert /tls/ca.pem https://loki:3100/ready >/dev/null 2>&1; then echo "loki ready"; break; fi
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "loki not ready" >&2; exit 1; fi

ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  HEALTH=$(curl -fsS --cacert /tls/ca.pem https://grafana:3000/api/health || true)
  case "${HEALTH}" in *'"database": "ok"'*|*'"database":"ok"'*) echo "grafana ready"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "grafana not ready; last=${HEALTH}" >&2; exit 1; fi

NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"grafana-stack-test"}}]},"scopeLogs":[{"scope":{"name":"grafana-stack-tests"},"logRecords":[{"timeUnixNano":"${NS_NANOS}","severityNumber":9,"severityText":"INFO","body":{"stringValue":"${MARKER}"}}]}]}]}
EOF
)
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem -o /tmp/o -w '%{http_code}' -X POST -H 'content-type: application/json' --data "${PAYLOAD}" https://loki:3100/otlp/v1/logs || echo 000)
  case "${CODE}" in 200|204) echo "push accepted (${CODE})"; break;; esac
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then echo "loki rejected push; last=${CODE}" >&2; cat /tmp/o >&2 || true; exit 1; fi

sleep 2
QUERY='{service_name="grafana-stack-test"}'
NOW=$(date +%s); END="${NOW}000000000"; START=$((NOW - 600)); START="${START}000000000"
ATTEMPT=0
while [ "${ATTEMPT}" -lt 30 ]; do
  CODE=$(curl -sS --cacert /tls/ca.pem -o /tmp/g -w '%{http_code}' --get -u "admin:${GRAFANA_PASSWORD}" --data-urlencode "query=${QUERY}" --data-urlencode "start=${START}" --data-urlencode "end=${END}" --data-urlencode 'limit=100' 'https://grafana:3000/api/datasources/proxy/uid/loki/loki/api/v1/query_range' || echo 000)
  if [ "${CODE}" = "200" ]; then
    case "$(cat /tmp/g)" in *"${MARKER}"*) echo "marker observed through tls proxy"; exit 0;; esac
  fi
  ATTEMPT=$((ATTEMPT + 1)); sleep 1
done
echo "marker never appeared through tls proxy; last=${CODE}" >&2; cat /tmp/g >&2 || true; exit 1
`
