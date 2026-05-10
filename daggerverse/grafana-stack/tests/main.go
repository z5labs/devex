// Package main is the grafana-stack-tests Dagger module: round-trip checks
// for each backend exposed by the grafana-stack module.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/dagger/dagger/util/parallel"
)

const curlImage = "curlimages/curl:8.10.1"

type Tests struct{}

// All runs every grafana-stack round-trip test in parallel.
//
// +check
// +cache="session"
func (t *Tests) All(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)

	jobs = jobs.WithJob("LokiAcceptsOtlpLogs", t.LokiAcceptsOtlpLogs)
	jobs = jobs.WithJob("TempoAcceptsOtlpTraces", t.TempoAcceptsOtlpTraces)
	jobs = jobs.WithJob("MimirAcceptsOtlpMetrics", t.MimirAcceptsOtlpMetrics)

	return jobs.Run(ctx)
}

// randomHex returns a hex-encoded random byte string of length n bytes
// (output is 2n hex characters).
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randomIDPair returns n random bytes encoded as both hex (for URL
// lookups, e.g. /api/traces/<id>) and base64-standard (for OTLP/HTTP
// JSON push, where bytes fields are base64-encoded per protobuf JSON).
func randomIDPair(n int) (hexEnc, b64Enc string, err error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(b), base64.StdEncoding.EncodeToString(b), nil
}

// LokiAcceptsOtlpLogs starts a Loki service, posts a single log record via
// the OTLP/HTTP receiver carrying a unique marker UUID, then queries Loki
// LogQL until the marker reappears in the query response. Verifies the
// default config wires up the OTLP HTTP ingester end-to-end.
func (t *Tests) LokiAcceptsOtlpLogs(ctx context.Context) error {
	marker, err := dag.Random().UUIDV4(ctx)
	if err != nil {
		return fmt.Errorf("generate marker: %w", err)
	}

	loki := dag.GrafanaStack().Loki()

	script := `set -eu
# Wait for Loki to become ready. /ready returns 503 during warmup
# (tsdb init, ring stabilization). Plain shell loop so behavior is
# transparent regardless of curl version's retry quirks.
READY_TIMEOUT=120
ATTEMPT=0
while [ "${ATTEMPT}" -lt "${READY_TIMEOUT}" ]; do
  if curl -fsS http://loki:3100/ready >/dev/null 2>&1; then
    echo "loki ready after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge "${READY_TIMEOUT}" ]; then
  echo "loki not ready after ${READY_TIMEOUT}s" >&2
  exit 1
fi

# Post one OTLP log record carrying $MARKER as the body. busybox
# date drops %N silently, so build nanos by appending 9 zeros to
# %s (good enough for a per-test marker; loki only needs second
# resolution to land in the right schema period).
NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{
  "resourceLogs": [
    {
      "resource": {
        "attributes": [
          {"key": "service.name", "value": {"stringValue": "grafana-stack-test"}}
        ]
      },
      "scopeLogs": [
        {
          "scope": {"name": "grafana-stack-tests"},
          "logRecords": [
            {
              "timeUnixNano": "${NS_NANOS}",
              "severityNumber": 9,
              "severityText": "INFO",
              "body": {"stringValue": "${MARKER}"}
            }
          ]
        }
      ]
    }
  ]
}
EOF
)

# POST may fail with 503 briefly after /ready turns green if a
# downstream component (distributor / ingester) isn't fully up.
# Retry until the OTLP endpoint accepts the payload.
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  HTTP_CODE=$(curl -sS -o /tmp/post.out -w '%{http_code}' \
    -X POST -H 'content-type: application/json' \
    --data "${PAYLOAD}" \
    http://loki:3100/otlp/v1/logs || echo 000)
  if [ "${HTTP_CODE}" = "200" ] || [ "${HTTP_CODE}" = "204" ]; then
    echo "loki accepted OTLP push (HTTP ${HTTP_CODE}) after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then
  echo "loki rejected OTLP push for 60s; last HTTP=${HTTP_CODE}" >&2
  echo "response body:" >&2
  cat /tmp/post.out >&2 || true
  exit 1
fi

# Allow Loki's ingester a moment to expose the just-pushed entry to
# queriers before polling. Without this, query_range can return an
# empty result for the first attempt or two.
sleep 2

# Poll LogQL for the marker. Loki ingest is fast but allow ~30s.
QUERY='{service_name="grafana-stack-test"}'
NOW_SECONDS=$(date +%s)
END_NANOS="${NOW_SECONDS}000000000"
START_SECONDS=$((NOW_SECONDS - 600))
START_NANOS="${START_SECONDS}000000000"

ATTEMPT=0
while [ "${ATTEMPT}" -lt 30 ]; do
  RESP=$(curl -fsS --get \
    --data-urlencode "query=${QUERY}" \
    --data-urlencode "start=${START_NANOS}" \
    --data-urlencode "end=${END_NANOS}" \
    --data-urlencode 'limit=100' \
    http://loki:3100/loki/api/v1/query_range || true)
  case "${RESP}" in
    *"${MARKER}"*)
      echo "marker observed in LogQL response"
      exit 0
      ;;
  esac
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done

echo "marker ${MARKER} never appeared in LogQL response" >&2
echo "last response: ${RESP}" >&2
exit 1
`

	out, err := dag.Container().From(curlImage).
		WithServiceBinding("loki", loki.Service()).
		WithEnvVariable("MARKER", marker).
		WithExec([]string{"sh", "-c", script}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("loki round-trip: %w", err)
	}
	_ = out
	return nil
}

// TempoAcceptsOtlpTraces starts a Tempo service, posts a single span via
// the OTLP/HTTP receiver carrying a unique 16-byte trace ID, then polls
// /api/traces/<trace_id> until Tempo returns the trace. Verifies the
// default config wires up the OTLP HTTP receiver and the local trace
// store end-to-end.
func (t *Tests) TempoAcceptsOtlpTraces(ctx context.Context) error {
	traceIDHex, traceIDB64, err := randomIDPair(16)
	if err != nil {
		return fmt.Errorf("generate trace id: %w", err)
	}
	spanIDHex, spanIDB64, err := randomIDPair(8)
	if err != nil {
		return fmt.Errorf("generate span id: %w", err)
	}

	tempo := dag.GrafanaStack().Tempo()

	script := `set -eu
# Wait for Tempo to become ready. Tempo takes a moment to bring up
# all internal subservices; /ready returns 503 until they're up.
READY_TIMEOUT=120
ATTEMPT=0
while [ "${ATTEMPT}" -lt "${READY_TIMEOUT}" ]; do
  if curl -fsS http://tempo:3200/ready >/dev/null 2>&1; then
    echo "tempo ready after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge "${READY_TIMEOUT}" ]; then
  echo "tempo not ready after ${READY_TIMEOUT}s" >&2
  exit 1
fi

# Tempo's OTLP/HTTP JSON receiver expects trace_id/span_id as
# hex strings (pdata.TraceID/SpanID custom marshaler). On read-back
# Tempo re-encodes them as base64 (protojson default for bytes),
# so we keep the base64 form for the post-query assertion.
# Times are unix nanoseconds; build via second resolution + 9 zeros
# because busybox date drops %N.
START_NANOS="$(date +%s)000000000"
END_NANOS="${START_NANOS}"
PAYLOAD=$(cat <<EOF
{
  "resourceSpans": [
    {
      "resource": {
        "attributes": [
          {"key": "service.name", "value": {"stringValue": "grafana-stack-test"}}
        ]
      },
      "scopeSpans": [
        {
          "scope": {"name": "grafana-stack-tests"},
          "spans": [
            {
              "traceId": "${TRACE_ID_HEX}",
              "spanId": "${SPAN_ID_HEX}",
              "name": "round-trip",
              "kind": 1,
              "startTimeUnixNano": "${START_NANOS}",
              "endTimeUnixNano": "${END_NANOS}"
            }
          ]
        }
      ]
    }
  ]
}
EOF
)

ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  HTTP_CODE=$(curl -sS -o /tmp/post.out -w '%{http_code}' \
    -X POST -H 'content-type: application/json' \
    --data "${PAYLOAD}" \
    http://tempo:4318/v1/traces || echo 000)
  if [ "${HTTP_CODE}" = "200" ] || [ "${HTTP_CODE}" = "204" ]; then
    echo "tempo accepted OTLP push (HTTP ${HTTP_CODE}) after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then
  echo "tempo rejected OTLP push for 60s; last HTTP=${HTTP_CODE}" >&2
  echo "response body:" >&2
  cat /tmp/post.out >&2 || true
  exit 1
fi

# Tempo's ingester serves traces from memory while they're still
# in the WAL. Allow a brief flush window.
sleep 2

ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  HTTP_CODE=$(curl -sS -o /tmp/get.out -w '%{http_code}' \
    "http://tempo:3200/api/traces/${TRACE_ID_HEX}" || echo 000)
  if [ "${HTTP_CODE}" = "200" ]; then
    case "$(cat /tmp/get.out)" in
      *"${SPAN_ID_B64}"*)
        echo "tempo returned trace ${TRACE_ID_HEX} carrying span ${SPAN_ID_HEX}"
        exit 0
        ;;
    esac
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done

echo "trace ${TRACE_ID_HEX} never appeared in tempo" >&2
echo "last HTTP=${HTTP_CODE} body:" >&2
cat /tmp/get.out >&2 || true
exit 1
`

	out, err := dag.Container().From(curlImage).
		WithServiceBinding("tempo", tempo.Service()).
		WithEnvVariable("TRACE_ID_HEX", traceIDHex).
		WithEnvVariable("TRACE_ID_B64", traceIDB64).
		WithEnvVariable("SPAN_ID_HEX", spanIDHex).
		WithEnvVariable("SPAN_ID_B64", spanIDB64).
		WithExec([]string{"sh", "-c", script}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("tempo round-trip: %w", err)
	}
	_ = out
	return nil
}

// MimirAcceptsOtlpMetrics starts a Mimir service, posts a single gauge
// sample via the OTLP/HTTP receiver under a uniquely-named metric, then
// queries Mimir's Prometheus-compatible API until that metric appears.
// Verifies the default config wires up the OTLP HTTP ingester and the
// filesystem block store end-to-end.
func (t *Tests) MimirAcceptsOtlpMetrics(ctx context.Context) error {
	suffix, err := randomHex(8)
	if err != nil {
		return fmt.Errorf("generate metric suffix: %w", err)
	}
	// Prometheus / Mimir lowercase metric names with underscores. The
	// suffix gives us a unique series per test run.
	metricName := "grafana_stack_test_marker_" + suffix

	mimir := dag.GrafanaStack().Mimir()

	script := `set -eu
READY_TIMEOUT=120
ATTEMPT=0
while [ "${ATTEMPT}" -lt "${READY_TIMEOUT}" ]; do
  if curl -fsS http://mimir:9009/ready >/dev/null 2>&1; then
    echo "mimir ready after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge "${READY_TIMEOUT}" ]; then
  echo "mimir not ready after ${READY_TIMEOUT}s" >&2
  exit 1
fi

# Build a single gauge sample at "now" (millisecond precision; mimir
# rejects samples >1h in the past or future). Busybox date drops %N
# so we synthesise nanos from %s + 9 zeros and millis from %s + 3.
NOW_SECONDS=$(date +%s)
TIME_NANOS="${NOW_SECONDS}000000000"
PAYLOAD=$(cat <<EOF
{
  "resourceMetrics": [
    {
      "resource": {
        "attributes": [
          {"key": "service.name", "value": {"stringValue": "grafana-stack-test"}}
        ]
      },
      "scopeMetrics": [
        {
          "scope": {"name": "grafana-stack-tests"},
          "metrics": [
            {
              "name": "${METRIC_NAME}",
              "description": "round-trip marker",
              "unit": "1",
              "gauge": {
                "dataPoints": [
                  {
                    "timeUnixNano": "${TIME_NANOS}",
                    "asDouble": 1.0
                  }
                ]
              }
            }
          ]
        }
      ]
    }
  ]
}
EOF
)

ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  HTTP_CODE=$(curl -sS -o /tmp/post.out -w '%{http_code}' \
    -X POST -H 'content-type: application/json' \
    --data "${PAYLOAD}" \
    http://mimir:9009/otlp/v1/metrics || echo 000)
  if [ "${HTTP_CODE}" = "200" ] || [ "${HTTP_CODE}" = "204" ]; then
    echo "mimir accepted OTLP push (HTTP ${HTTP_CODE}) after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then
  echo "mimir rejected OTLP push for 60s; last HTTP=${HTTP_CODE}" >&2
  echo "response body:" >&2
  cat /tmp/post.out >&2 || true
  exit 1
fi

# Mimir flushes from head to TSDB on a periodic basis; the in-memory
# head is queryable immediately, but allow a brief settle.
sleep 2

ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  RESP=$(curl -fsS --get \
    --data-urlencode "query=${METRIC_NAME}" \
    "http://mimir:9009/prometheus/api/v1/query" || true)
  case "${RESP}" in
    *"\"${METRIC_NAME}\""*)
      echo "mimir returned metric ${METRIC_NAME}"
      exit 0
      ;;
  esac
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done

echo "metric ${METRIC_NAME} never appeared in mimir" >&2
echo "last response: ${RESP}" >&2
exit 1
`

	out, err := dag.Container().From(curlImage).
		WithServiceBinding("mimir", mimir.Service()).
		WithEnvVariable("METRIC_NAME", metricName).
		WithExec([]string{"sh", "-c", script}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("mimir round-trip: %w", err)
	}
	_ = out
	return nil
}
