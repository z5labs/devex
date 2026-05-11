// Package main is the otel-tests Dagger module: round-trip and unit
// checks for the otel daggerverse module.
package main

import (
	"context"
	"fmt"

	"dagger/tests/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
	"gopkg.in/yaml.v3"
)

type Tests struct{}

// All runs every otel test in parallel.
//
// collectorTag picks the otel/opentelemetry-collector{,-contrib} tag
// every spawned collector runs against; lokiTag/tempoTag/mimirTag
// pick the grafana/{loki,tempo,mimir} tags for the round-trip
// backends. Each default matches the upstream module's own default,
// so the no-arg invocation stays a smooth path.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
	// +default="3.4.1"
	lokiTag string,
	// +default="2.7.1"
	tempoTag string,
	// +default="2.15.1"
	mimirTag string,
) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	jobs = jobs.WithJob("RejectsInvalidComponentName", t.RejectsInvalidComponentName)
	jobs = jobs.WithJob("RejectsUnknownPipelineSignal", t.RejectsUnknownPipelineSignal)
	jobs = jobs.WithJob("SharedReceiverIsDedupedInRenderedYaml", func(ctx context.Context) error {
		return t.SharedReceiverIsDedupedInRenderedYaml(ctx, collectorTag)
	})
	jobs = jobs.WithJob("CustomComponentBodyIsSpliced", func(ctx context.Context) error {
		return t.CustomComponentBodyIsSpliced(ctx, collectorTag)
	})
	jobs = jobs.WithJob("BindsCollectorIntoFreshContainer", func(ctx context.Context) error {
		return t.BindsCollectorIntoFreshContainer(ctx, collectorTag)
	})
	jobs = jobs.WithJob("ServiceWithoutPipelinesOrConfigFails", func(ctx context.Context) error {
		return t.ServiceWithoutPipelinesOrConfigFails(ctx, collectorTag)
	})
	jobs = jobs.WithJob("DebugPipelineAcceptsOtlpPush", func(ctx context.Context) error {
		return t.DebugPipelineAcceptsOtlpPush(ctx, collectorTag)
	})
	jobs = jobs.WithJob("CoreForwardsLogsToLoki", func(ctx context.Context) error {
		return t.CoreForwardsLogsToLoki(ctx, collectorTag, lokiTag)
	})
	jobs = jobs.WithJob("CoreForwardsTracesToTempo", func(ctx context.Context) error {
		return t.CoreForwardsTracesToTempo(ctx, collectorTag, tempoTag)
	})
	jobs = jobs.WithJob("CoreForwardsMetricsToMimir", func(ctx context.Context) error {
		return t.CoreForwardsMetricsToMimir(ctx, collectorTag, mimirTag)
	})
	jobs = jobs.WithJob("ContribForwardsLogsToLoki", func(ctx context.Context) error {
		return t.ContribForwardsLogsToLoki(ctx, collectorTag, lokiTag)
	})
	return jobs.Run(ctx)
}

const probeImage = "alpine:3"
const curlImage = "curlimages/curl:8.10.1"

// marker returns a fresh hex marker suitable for tagging telemetry
// pushed during a single test run.
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

// DebugPipelineAcceptsOtlpPush asserts a collector configured with
// DebugPipeline("logs") accepts an OTLP/HTTP log push without
// erroring (HTTP 200/204).
func (t *Tests) DebugPipelineAcceptsOtlpPush(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	o := dag.Otel()
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).WithPipeline(o.DebugPipeline("logs"))
	_, err = dag.Container().From(curlImage).
		WithServiceBinding("col", col.Service()).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", `
set -eu
NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{
  "resourceLogs": [
    {
      "resource": {
        "attributes": [
          {"key": "service.name", "value": {"stringValue": "otel-tests"}}
        ]
      },
      "scopeLogs": [
        {
          "scope": {"name": "otel-tests"},
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
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  HTTP_CODE=$(curl -sS -o /tmp/post.out -w '%{http_code}' \
    -X POST -H 'content-type: application/json' \
    --data "${PAYLOAD}" \
    http://col:4318/v1/logs || echo 000)
  if [ "${HTTP_CODE}" = "200" ] || [ "${HTTP_CODE}" = "204" ]; then
    echo "collector accepted OTLP push (HTTP ${HTTP_CODE}) after ${ATTEMPT}s"
    exit 0
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
echo "collector rejected OTLP push for 60s; last HTTP=${HTTP_CODE}" >&2
cat /tmp/post.out >&2 || true
exit 1
`}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("OTLP push: %w", err)
	}
	return nil
}

func probeCollectorPorts(ctx context.Context, svc *dagger.Service) error {
	_, err := dag.Container().From(probeImage).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-extras"}).
		WithServiceBinding("col", svc).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  if nc -z col 4317 && nc -z col 4318; then
    echo "ports up after ${i}s"
    exit 0
  fi
  sleep 1
done
echo "ports did not come up" >&2
exit 1
`}).
		Sync(ctx)
	return err
}

// BindsCollectorIntoFreshContainer asserts that a collector configured
// with the DebugPipeline can be reached on :4317 and :4318 from a
// vanilla alpine container via WithServiceBinding.
func (t *Tests) BindsCollectorIntoFreshContainer(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	o := dag.Otel()
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).WithPipeline(o.DebugPipeline("logs"))
	if err := probeCollectorPorts(ctx, col.Service()); err != nil {
		return fmt.Errorf("probe collector ports: %w", err)
	}
	return nil
}

// ServiceWithoutPipelinesOrConfigFails asserts that calling Service()
// on a collector with no pipelines and no override produces a
// container whose exec exits non-zero — the collector binary refuses
// to start without --config.
func (t *Tests) ServiceWithoutPipelinesOrConfigFails(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	o := dag.Otel()
	if err := probeCollectorPorts(ctx, o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).Service()); err == nil {
		return fmt.Errorf("expected collector with no config to fail to start, but probe succeeded")
	}
	return nil
}

// SharedReceiverIsDedupedInRenderedYaml asserts that wiring one
// receiver into three pipelines emits a single top-level
// receivers.otlp/primary entry rather than three.
func (t *Tests) SharedReceiverIsDedupedInRenderedYaml(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	o := dag.Otel()
	primary := o.OtlpReceiver("primary")
	p1 := o.Pipeline("logs", "p1").WithReceiver(primary)
	p2 := o.Pipeline("traces", "p2").WithReceiver(primary)
	p3 := o.Pipeline("metrics", "p3").WithReceiver(primary)

	contents, err := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithPipeline(p1).WithPipeline(p2).WithPipeline(p3).
		ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg struct {
		Receivers map[string]any `yaml:"receivers"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return fmt.Errorf("parse rendered yaml: %w\n---\n%s", err, contents)
	}
	if len(cfg.Receivers) != 1 {
		return fmt.Errorf("expected 1 receiver, got %d: %v", len(cfg.Receivers), cfg.Receivers)
	}
	if _, ok := cfg.Receivers["otlp/primary"]; !ok {
		return fmt.Errorf("missing otlp/primary, got: %v", cfg.Receivers)
	}
	return nil
}

// CustomComponentBodyIsSpliced asserts that a Custom* component's
// caller-supplied YAML body lands structurally under the rendered
// config (not as a quoted scalar).
func (t *Tests) CustomComponentBodyIsSpliced(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
) error {
	o := dag.Otel()
	custom := o.CustomExporter("file", "out", "path: /tmp/out.json\n")
	p := o.Pipeline("logs", "p").
		WithReceiver(o.OtlpReceiver("r")).
		WithExporter(custom)

	contents, err := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithPipeline(p).ConfigFile().Contents(ctx)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg struct {
		Exporters map[string]map[string]any `yaml:"exporters"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return fmt.Errorf("parse rendered yaml: %w\n---\n%s", err, contents)
	}
	out, ok := cfg.Exporters["file/out"]
	if !ok {
		return fmt.Errorf("missing exporters.file/out, got: %v", cfg.Exporters)
	}
	if got := fmt.Sprintf("%v", out["path"]); got != "/tmp/out.json" {
		return fmt.Errorf("expected exporters.file/out.path == /tmp/out.json, got %q", got)
	}
	return nil
}

// RejectsUnknownPipelineSignal asserts Otel.Pipeline rejects signals
// outside {logs, traces, metrics}, and Otel.DebugPipeline does the same.
func (t *Tests) RejectsUnknownPipelineSignal(ctx context.Context) error {
	o := dag.Otel()
	if _, err := o.Pipeline("audit", "x").ID(ctx); err == nil {
		return fmt.Errorf("Pipeline(\"audit\", \"x\"): expected error, got nil")
	}
	if _, err := o.Pipeline("logs", "bad name").ID(ctx); err == nil {
		return fmt.Errorf("Pipeline(\"logs\", \"bad name\"): expected error on bad name, got nil")
	}
	if _, err := o.Pipeline("logs", "ok").ID(ctx); err != nil {
		return fmt.Errorf("Pipeline(\"logs\", \"ok\"): expected nil, got %w", err)
	}
	if _, err := o.DebugPipeline("audit").ID(ctx); err == nil {
		return fmt.Errorf("DebugPipeline(\"audit\"): expected error, got nil")
	}
	if _, err := o.DebugPipeline("logs").ID(ctx); err != nil {
		return fmt.Errorf("DebugPipeline(\"logs\"): expected nil, got %w", err)
	}
	return nil
}

// RejectsInvalidComponentName asserts every component factory and the
// Custom* escape hatches reject empty / non-conforming names (and
// non-conforming kinds, where applicable) with a non-nil error.
func (t *Tests) RejectsInvalidComponentName(ctx context.Context) error {
	o := dag.Otel()

	// Each factory: (description, valid call, invalid call).
	type factory struct {
		name    string
		invalid func() error
		valid   func() error
	}
	factories := []factory{
		{
			name:    "OtlpReceiver",
			invalid: func() error { _, err := o.OtlpReceiver("bad name").ID(ctx); return err },
			valid:   func() error { _, err := o.OtlpReceiver("primary").ID(ctx); return err },
		},
		{
			name:    "OtlpExporter",
			invalid: func() error { _, err := o.OtlpExporter("bad name", "x:1").ID(ctx); return err },
			valid:   func() error { _, err := o.OtlpExporter("out", "tempo:4317").ID(ctx); return err },
		},
		{
			name:    "OtlpHTTPExporter",
			invalid: func() error { _, err := o.OtlpHTTPExporter("", "x").ID(ctx); return err },
			valid:   func() error { _, err := o.OtlpHTTPExporter("out", "http://x").ID(ctx); return err },
		},
		{
			name:    "DebugExporter",
			invalid: func() error { _, err := o.DebugExporter("bad/name").ID(ctx); return err },
			valid:   func() error { _, err := o.DebugExporter("dbg").ID(ctx); return err },
		},
		{
			name:    "BatchProcessor",
			invalid: func() error { _, err := o.BatchProcessor("").ID(ctx); return err },
			valid:   func() error { _, err := o.BatchProcessor("batch1").ID(ctx); return err },
		},
		{
			name:    "MemoryLimiterProcessor",
			invalid: func() error { _, err := o.MemoryLimiterProcessor("bad name").ID(ctx); return err },
			valid:   func() error { _, err := o.MemoryLimiterProcessor("ml").ID(ctx); return err },
		},
		{
			name:    "ResourceProcessor",
			invalid: func() error { _, err := o.ResourceProcessor("bad/name").ID(ctx); return err },
			valid:   func() error { _, err := o.ResourceProcessor("res").ID(ctx); return err },
		},
		{
			name:    "CustomReceiver name",
			invalid: func() error { _, err := o.CustomReceiver("file", "bad name", "{}").ID(ctx); return err },
			valid:   func() error { _, err := o.CustomReceiver("file", "ok", "{}").ID(ctx); return err },
		},
		{
			name:    "CustomReceiver kind",
			invalid: func() error { _, err := o.CustomReceiver("bad kind", "ok", "{}").ID(ctx); return err },
			valid:   func() error { _, err := o.CustomReceiver("file", "ok", "{}").ID(ctx); return err },
		},
		{
			name:    "CustomProcessor",
			invalid: func() error { _, err := o.CustomProcessor("", "ok", "{}").ID(ctx); return err },
			valid:   func() error { _, err := o.CustomProcessor("filter", "ok", "{}").ID(ctx); return err },
		},
		{
			name:    "CustomExporter",
			invalid: func() error { _, err := o.CustomExporter("file", "", "{}").ID(ctx); return err },
			valid:   func() error { _, err := o.CustomExporter("file", "ok", "{}").ID(ctx); return err },
		},
	}

	for _, f := range factories {
		if err := f.invalid(); err == nil {
			return fmt.Errorf("%s: expected error on invalid input, got nil", f.name)
		}
		if err := f.valid(); err != nil {
			return fmt.Errorf("%s: expected nil error on valid input, got %w", f.name, err)
		}
	}
	return nil
}

// loksRoundTrip runs the shared logs round-trip body — usable by both
// CoreForwardsLogsToLoki and ContribForwardsLogsToLoki.
func lokiRoundTrip(ctx context.Context, collectorService *dagger.Service, lokiSvc *dagger.Service, mark string) error {
	script := `
set -eu
# Wait for loki readiness so its OTLP path is wired before we POST.
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  if curl -fsS http://loki:3100/ready >/dev/null 2>&1; then
    echo "loki ready after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then
  echo "loki not ready after 120s" >&2
  exit 1
fi

NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{
  "resourceLogs": [{
    "resource": {"attributes":[{"key":"service.name","value":{"stringValue":"otel-tests-${MARKER}"}}]},
    "scopeLogs":[{"scope":{"name":"otel-tests"},"logRecords":[
      {"timeUnixNano":"${NS_NANOS}","severityNumber":9,"severityText":"INFO","body":{"stringValue":"${MARKER}"}}
    ]}]
  }]
}
EOF
)
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  HTTP_CODE=$(curl -sS -o /tmp/post.out -w '%{http_code}' \
    -X POST -H 'content-type: application/json' \
    --data "${PAYLOAD}" http://col:4318/v1/logs || echo 000)
  case "${HTTP_CODE}" in 200|204) break ;; esac
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then
  echo "collector rejected push; last HTTP=${HTTP_CODE}" >&2
  cat /tmp/post.out >&2 || true
  exit 1
fi

sleep 3
QUERY="{service_name=\"otel-tests-${MARKER}\"}"
NOW=$(date +%s); START=$((NOW - 600))
END_NANOS="${NOW}000000000"; START_NANOS="${START}000000000"
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  RESP=$(curl -fsS --get --data-urlencode "query=${QUERY}" \
    --data-urlencode "start=${START_NANOS}" --data-urlencode "end=${END_NANOS}" \
    --data-urlencode 'limit=100' http://loki:3100/loki/api/v1/query_range || true)
  case "${RESP}" in *"${MARKER}"*) echo "marker observed"; exit 0 ;; esac
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
echo "marker ${MARKER} never appeared in LogQL response" >&2
echo "last: ${RESP}" >&2
exit 1
`
	_, err := dag.Container().From(curlImage).
		WithServiceBinding("col", collectorService).
		WithServiceBinding("loki", lokiSvc).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	return err
}

// CoreForwardsLogsToLoki asserts a Core collector forwards an OTLP/HTTP
// log push through to the grafana-stack Loki backend, where it is
// queryable via LogQL.
func (t *Tests) CoreForwardsLogsToLoki(
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
	loki := dag.GrafanaStack().Loki(dagger.GrafanaStackLokiOpts{Tag: lokiTag})
	o := dag.Otel()
	exp := o.OtlpHTTPExporter("loki", "http://loki:3100/otlp")
	p := o.Pipeline("logs", "p").
		WithReceiver(o.OtlpReceiver("in")).
		WithExporter(exp)
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithServiceBinding("loki", loki.Service()).
		WithPipeline(p)
	return lokiRoundTrip(ctx, col.Service(), loki.Service(), mark)
}

// ContribForwardsLogsToLoki — smoke check on the contrib distribution.
func (t *Tests) ContribForwardsLogsToLoki(
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
	loki := dag.GrafanaStack().Loki(dagger.GrafanaStackLokiOpts{Tag: lokiTag})
	o := dag.Otel()
	exp := o.OtlpHTTPExporter("loki", "http://loki:3100/otlp")
	p := o.Pipeline("logs", "p").
		WithReceiver(o.OtlpReceiver("in")).
		WithExporter(exp)
	col := o.Contrib(dagger.OtelContribOpts{Tag: collectorTag}).
		WithServiceBinding("loki", loki.Service()).
		WithPipeline(p)
	return lokiRoundTrip(ctx, col.Service(), loki.Service(), mark)
}

// CoreForwardsTracesToTempo asserts an OTLP/HTTP trace pushed to the
// collector lands in Tempo via the OTLP/gRPC exporter.
func (t *Tests) CoreForwardsTracesToTempo(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
	// +default="2.7.1"
	tempoTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	// Trace IDs are 16 random bytes, span IDs 8 — distinct hex slices
	// drawn from one Sha256 sample so they're independent and unique.
	pool, err := dag.Random().Sha256(ctx)
	if err != nil {
		return err
	}
	if len(pool) < 64 {
		return fmt.Errorf("random sha256 too short: %d", len(pool))
	}
	traceID := pool[:32]
	spanID := pool[32:48]

	tempo := dag.GrafanaStack().Tempo(dagger.GrafanaStackTempoOpts{Tag: tempoTag})
	o := dag.Otel()
	exp := o.OtlpExporter("tempo", "tempo:4317")
	p := o.Pipeline("traces", "p").
		WithReceiver(o.OtlpReceiver("in")).
		WithExporter(exp)
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithServiceBinding("tempo", tempo.Service()).
		WithPipeline(p)

	script := `
set -eu
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  if curl -fsS http://tempo:3200/ready >/dev/null 2>&1; then
    echo "tempo ready after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "tempo not ready" >&2; exit 1; fi

NS_NANOS="$(date +%s)000000000"
END_NANOS=$((NS_NANOS + 1000000000))
PAYLOAD=$(cat <<EOF
{
  "resourceSpans": [{
    "resource": {"attributes":[{"key":"service.name","value":{"stringValue":"otel-tests-${MARKER}"}}]},
    "scopeSpans": [{"scope":{"name":"otel-tests"},"spans":[
      {"traceId":"${TRACEID}","spanId":"${SPANID}","name":"otel-tests-span-${MARKER}",
       "kind":1,"startTimeUnixNano":"${NS_NANOS}","endTimeUnixNano":"${END_NANOS}",
       "status":{"code":1}}
    ]}]
  }]
}
EOF
)
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  HTTP_CODE=$(curl -sS -o /tmp/post.out -w '%{http_code}' \
    -X POST -H 'content-type: application/json' \
    --data "${PAYLOAD}" http://col:4318/v1/traces || echo 000)
  case "${HTTP_CODE}" in 200|204) break ;; esac
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then
  echo "collector rejected trace push; last HTTP=${HTTP_CODE}" >&2
  cat /tmp/post.out >&2 || true
  exit 1
fi

sleep 5
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  RESP=$(curl -fsS "http://tempo:3200/api/traces/${TRACEID}" || true)
  case "${RESP}" in *"${MARKER}"*) echo "trace observed in tempo"; exit 0 ;; esac
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
echo "trace ${TRACEID} (marker ${MARKER}) never appeared in tempo" >&2
echo "last: ${RESP}" >&2
exit 1
`
	_, err = dag.Container().From(curlImage).
		WithServiceBinding("col", col.Service()).
		WithServiceBinding("tempo", tempo.Service()).
		WithEnvVariable("MARKER", mark).
		WithEnvVariable("TRACEID", traceID).
		WithEnvVariable("SPANID", spanID).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	return err
}

// CoreForwardsMetricsToMimir asserts an OTLP/HTTP metric pushed to the
// collector lands in Mimir via the OTLP/HTTP exporter and is
// queryable via the Prometheus API.
func (t *Tests) CoreForwardsMetricsToMimir(
	ctx context.Context,
	// +default="0.130.1"
	collectorTag string,
	// +default="2.15.1"
	mimirTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	mimir := dag.GrafanaStack().Mimir(dagger.GrafanaStackMimirOpts{Tag: mimirTag})
	o := dag.Otel()
	exp := o.OtlpHTTPExporter("mimir", "http://mimir:9009/otlp")
	p := o.Pipeline("metrics", "p").
		WithReceiver(o.OtlpReceiver("in")).
		WithExporter(exp)
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithServiceBinding("mimir", mimir.Service()).
		WithPipeline(p)

	script := `
set -eu
ATTEMPT=0
while [ "${ATTEMPT}" -lt 120 ]; do
  if curl -fsS http://mimir:9009/ready >/dev/null 2>&1; then
    echo "mimir ready after ${ATTEMPT}s"
    break
  fi
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 120 ]; then echo "mimir not ready" >&2; exit 1; fi

NS_NANOS="$(date +%s)000000000"
PAYLOAD=$(cat <<EOF
{
  "resourceMetrics": [{
    "resource": {"attributes":[
      {"key":"service.name","value":{"stringValue":"otel-tests-${MARKER}"}}
    ]},
    "scopeMetrics": [{"scope":{"name":"otel-tests"},"metrics":[
      {"name":"otel_test_marker","unit":"1",
       "gauge":{"dataPoints":[
         {"timeUnixNano":"${NS_NANOS}","asDouble":1.0,
          "attributes":[{"key":"marker","value":{"stringValue":"${MARKER}"}}]}
       ]}}
    ]}]
  }]
}
EOF
)
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  HTTP_CODE=$(curl -sS -o /tmp/post.out -w '%{http_code}' \
    -X POST -H 'content-type: application/json' \
    -H 'X-Scope-OrgID: anonymous' \
    --data "${PAYLOAD}" http://col:4318/v1/metrics || echo 000)
  case "${HTTP_CODE}" in 200|204) break ;; esac
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
if [ "${ATTEMPT}" -ge 60 ]; then
  echo "collector rejected metrics push; last HTTP=${HTTP_CODE}" >&2
  cat /tmp/post.out >&2 || true
  exit 1
fi

sleep 5
ATTEMPT=0
while [ "${ATTEMPT}" -lt 60 ]; do
  RESP=$(curl -fsS -H 'X-Scope-OrgID: anonymous' --get \
    --data-urlencode "query=otel_test_marker{marker=\"${MARKER}\"}" \
    http://mimir:9009/prometheus/api/v1/query || true)
  case "${RESP}" in *"${MARKER}"*) echo "metric observed in mimir"; exit 0 ;; esac
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done
echo "metric marker ${MARKER} never appeared in mimir" >&2
echo "last: ${RESP}" >&2
exit 1
`
	_, err = dag.Container().From(curlImage).
		WithServiceBinding("col", col.Service()).
		WithServiceBinding("mimir", mimir.Service()).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx)
	return err
}
