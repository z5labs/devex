package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dagger/ci/internal/dagger"
)

// Pinned image tags for the integration stack. Never :latest — a moving tag
// would make these round-trips non-reproducible.
const (
	curlImage    = "curlimages/curl:8.10.1"
	alpineImage  = "alpine:3.22"
	collectorTag = "0.130.1"
	tempoTag     = "2.7.1"
	mimirTag     = "2.15.1"
	lokiTag      = "3.4.1"
)

// gitFixture wraps a source directory in a throwaway git working tree so
// GoApp.Ci's "must be a git working tree" guard passes. Verbatim from the
// z5labs tests (daggerverse/z5labs/tests/main.go): git init/add/commit inside
// dag.Go().Container(base), which mounts the source at /src.
func gitFixture(ctx context.Context, base *dagger.Directory, branch string) (*dagger.Directory, error) {
	ctr := dag.Go().Container(base).
		WithEnvVariable("GIT_AUTHOR_NAME", "CI").
		WithEnvVariable("GIT_AUTHOR_EMAIL", "ci@example.com").
		WithEnvVariable("GIT_COMMITTER_NAME", "CI").
		WithEnvVariable("GIT_COMMITTER_EMAIL", "ci@example.com").
		WithExec([]string{"git", "init", "--initial-branch=" + branch, "."}).
		WithExec([]string{"git", "add", "."}).
		WithExec([]string{"git", "commit", "-m", "initial"})
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	return ctr.Directory("/src"), nil
}

// marker returns a fresh random token used to make each run's telemetry (its
// OTel service.name) and produced records uniquely queryable.
func marker(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256: %w", err)
	}
	if len(h) < 16 {
		return "", fmt.Errorf("random sha256 too short: %d", len(h))
	}
	return h[:16], nil
}

// randHex returns a fresh hex suffix to disambiguate Dagger secret names across
// concurrent test invocations in one engine session.
func randHex(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("randHex: %w", err)
	}
	if len(h) < 16 {
		return "", fmt.Errorf("randHex too short: %d", len(h))
	}
	return h[:16], nil
}

// newClusterId mints a KRaft-shaped cluster id (16 random bytes as 22 base64url
// characters).
func newClusterId(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256: %w", err)
	}
	if len(h) < 32 {
		return "", fmt.Errorf("random sha256 too short: %d", len(h))
	}
	raw, err := hex.DecodeString(h[:32])
	if err != nil {
		return "", fmt.Errorf("decode random sha256: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// randomTopicName mints a fresh, valid Kafka topic name.
func randomTopicName(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256: %w", err)
	}
	if len(h) < 16 {
		return "", fmt.Errorf("random sha256 too short: %d", len(h))
	}
	return "t-" + h[:16], nil
}

// freshCa mints a fresh per-test root CA via the certificate-management module,
// with random key/password/serial so each run is unique. Copied from the kafka
// tests (daggerverse/kafka/tests/helpers.go).
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

	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca password: %w", label, err)
	}
	pwd := dag.SetSecret(label+"-ca-pwd-"+suffix, pwdHex)

	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	return dag.CertificateManagement().CreateCertificateAuthority(nb, serial, pwd, key,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   "Test CA " + label,
			ValidityDays: 30,
		}), nil
}

// issueClientKeystore mints a clientAuth leaf signed by ca and returns its
// PKCS#12 keystore + password. Copied from the kafka tests.
func issueClientKeystore(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, cn string) (*dagger.File, *dagger.Secret, error) {
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, nil, err
	}
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate %s client leaf key: %w", cn, err)
	}
	key := dag.SetSecret("client-leaf-key-"+cn+"-"+suffix, keyPem)
	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate %s client leaf password: %w", cn, err)
	}
	pwd := dag.SetSecret("client-leaf-pwd-"+cn+"-"+suffix, pwdHex)
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("generate %s client leaf serial: %w", cn, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	ks := ca.IssueClientCertificate(cn, nb, serial, pwd, key).KeyStore()
	return ks.Pkcs12(), ks.Password(), nil
}

// consumerRunnerConfig holds everything consumerRunner needs to assemble the
// example consumer container. The service bindings (brokers, schema registry,
// collector) are applied by the caller, which holds the concrete cluster type
// (Apache Cluster vs RedpandaCluster) and knows how to bind it.
type consumerRunnerConfig struct {
	bin          *dagger.File
	brokers      []string
	registryURL  string
	trustStore   *dagger.File
	trustStorePw *dagger.Secret
	keyStore     *dagger.File   // nil unless mTLS
	keyStorePw   *dagger.Secret // nil unless mTLS
	topic        string
	group        string
	serviceName  string
	maxRecords   int
	timeout      string
	otelEndpoint string
}

// consumerRunner drops the built example binary into a minimal base image and
// sets every flag-backing env var the consumer reads (see the example's main.go
// loadConfig: BROKERS, TOPIC, GROUP, REGISTRY_URL, TRUSTSTORE[_PASSWORD],
// optional KEYSTORE[_PASSWORD] for mTLS, MAX_RECORDS, TIMEOUT, OTEL_*). The
// caller then binds the broker/registry/collector services and execs "consumer".
// Shared by RunAgainst.Local and avroConsume so the two run configurations
// cannot drift apart.
func consumerRunner(cfg consumerRunnerConfig) *dagger.Container {
	runner := dag.Container().From(alpineImage).
		WithFile("/usr/local/bin/consumer", cfg.bin).
		WithFile("/certs/truststore.p12", cfg.trustStore).
		WithSecretVariable("TRUSTSTORE_PASSWORD", cfg.trustStorePw).
		WithEnvVariable("BROKERS", strings.Join(cfg.brokers, ",")).
		WithEnvVariable("REGISTRY_URL", cfg.registryURL).
		WithEnvVariable("TRUSTSTORE", "/certs/truststore.p12").
		WithEnvVariable("TOPIC", cfg.topic).
		WithEnvVariable("GROUP", cfg.group).
		WithEnvVariable("MAX_RECORDS", strconv.Itoa(cfg.maxRecords)).
		WithEnvVariable("TIMEOUT", cfg.timeout).
		WithEnvVariable("OTEL_EXPORTER_OTLP_ENDPOINT", cfg.otelEndpoint).
		WithEnvVariable("OTEL_EXPORTER_OTLP_INSECURE", "true").
		WithEnvVariable("OTEL_SERVICE_NAME", cfg.serviceName)
	if cfg.keyStore != nil {
		runner = runner.
			WithFile("/certs/keystore.p12", cfg.keyStore).
			WithSecretVariable("KEYSTORE_PASSWORD", cfg.keyStorePw).
			WithEnvVariable("KEYSTORE", "/certs/keystore.p12")
	}
	return runner
}

// assertTelemetry verifies that the consumer's OpenTelemetry reached the
// collector by querying the three grafana-stack backends behind it for the
// run's unique service.name: traces in Tempo, metrics in Mimir, logs in Loki.
// The consumer has already run and flushed, and the collector forwards without
// batching, so this is query-only — it polls each backend until all three
// report the marker.
func assertTelemetry(ctx context.Context, tempo, mimir, loki *dagger.Service, serviceName string) error {
	// One curl container bound to all three backends polls them together,
	// exiting as soon as each has observed the marker.
	script := `
set -eu
check_tempo() {
  RESP=$(curl -fsS --get \
    --data-urlencode "q={ resource.service.name = \"${SVC}\" }" \
    --data-urlencode "limit=5" \
    http://tempo:3200/api/search 2>/dev/null || true)
  case "${RESP}" in *"${SVC}"*) return 0 ;; esac
  return 1
}
check_mimir() {
  RESP=$(curl -fsS -H 'X-Scope-OrgID: anonymous' --get \
    --data-urlencode "query=target_info{service_name=\"${SVC}\"}" \
    http://mimir:9009/prometheus/api/v1/query 2>/dev/null || true)
  case "${RESP}" in *"${SVC}"*) return 0 ;; esac
  # fall back to the job label the OTLP->Prometheus translation derives from
  # service.name
  RESP=$(curl -fsS -H 'X-Scope-OrgID: anonymous' --get \
    --data-urlencode "match[]={job=\"${SVC}\"}" \
    http://mimir:9009/prometheus/api/v1/series 2>/dev/null || true)
  case "${RESP}" in *"${SVC}"*) return 0 ;; esac
  return 1
}
check_loki() {
  NOW=$(date +%s); START=$((NOW - 900))
  RESP=$(curl -fsS --get \
    --data-urlencode "query={service_name=\"${SVC}\"}" \
    --data-urlencode "start=${START}000000000" \
    --data-urlencode "end=${NOW}000000000" \
    --data-urlencode "limit=50" \
    http://loki:3100/loki/api/v1/query_range 2>/dev/null || true)
  case "${RESP}" in *"${SVC}"*) return 0 ;; esac
  return 1
}
t=0; m=0; l=0; ATTEMPT=0
while [ "${ATTEMPT}" -lt 90 ]; do
  [ "${t}" -eq 0 ] && check_tempo && { t=1; echo "tempo: trace observed"; }
  [ "${m}" -eq 0 ] && check_mimir && { m=1; echo "mimir: metric observed"; }
  [ "${l}" -eq 0 ] && check_loki && { l=1; echo "loki: log observed"; }
  if [ "${t}" -eq 1 ] && [ "${m}" -eq 1 ] && [ "${l}" -eq 1 ]; then
    echo "all telemetry (traces + metrics + logs) reached the collector"
    exit 0
  fi
  ATTEMPT=$((ATTEMPT + 1)); sleep 2
done
echo "telemetry missing after $((ATTEMPT * 2))s: tempo=${t} mimir=${m} loki=${l}" >&2
exit 1
`
	_, err := dag.Container().From(curlImage).
		WithServiceBinding("tempo", tempo).
		WithServiceBinding("mimir", mimir).
		WithServiceBinding("loki", loki).
		WithEnvVariable("SVC", serviceName).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("telemetry did not reach the collector for %q: %w", serviceName, err)
	}
	return nil
}
