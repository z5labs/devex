// Package main is the otel-examples Dagger module: a runnable cookbook of
// otel recipes. Each one walks the same builder chain — receivers,
// processors, and exporters composed into a pipeline, pipelines composed
// into a collector — and then returns either the endpoint a telemetry
// client would point at or the YAML that chain rendered.
//
// Read them in order: DebugTracesPipeline is the shortest chain that runs,
// OtlpToTempo swaps the throwaway exporter for a real backend,
// BatchedMetricsPipeline adds the processor stage, ConfigDumpYaml shows
// what any of those chains actually rendered, and CustomReceiverYaml is the
// escape hatch for the components the typed factories do not cover.
package main

import (
	"context"
	"fmt"

	"dagger/otel-examples/internal/dagger"
)

// OtelExamples is the module's main object: a namespace for the otel usage
// recipes.
type OtelExamples struct{}

// tempoHost is the hostname OtlpToTempo binds Tempo into the collector's
// network under. It has to appear in two places that must agree — the
// WithServiceBinding alias and the exporter's endpoint — so it lives here
// rather than being spelled twice.
const tempoHost = "tempo"

// tempoOtlpGrpcPort is Tempo's OTLP/gRPC receiver port. The collector
// reaches it in-network, so this is the port on the service itself, not a
// published one.
const tempoOtlpGrpcPort = 4317

// DebugTracesPipeline builds the shortest collector that does something
// observable: DebugPipeline pre-wires otlp receiver → batch → debug
// exporter, so spans pushed to the returned OTLP/gRPC address are printed
// to the collector's stdout instead of being forwarded anywhere. Start
// here when you want to see what a tracer is actually emitting.
//
// +cache="never"
func (m *OtelExamples) DebugTracesPipeline(ctx context.Context) (string, error) {
	o := dag.Otel()
	endpoint, err := o.Core().
		WithPipeline(o.DebugPipeline("traces")).
		OtlpGrpcEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve collector otlp/grpc endpoint: %w", err)
	}
	return endpoint, nil
}

// OtlpToTempo replaces the debug exporter with a real backend: a
// grafana-stack Tempo, bound into the collector's network under the
// "tempo" hostname, which is why the exporter can address it as
// "tempo:4317". The returned OTLP/gRPC address is what a tracer targets;
// spans pushed there are batched and forwarded on to Tempo. This is the
// recipe to copy whenever an exporter needs to reach another service —
// the binding alias and the exporter endpoint are one contract.
//
// +cache="never"
func (m *OtelExamples) OtlpToTempo(ctx context.Context) (string, error) {
	o := dag.Otel()
	tempo := dag.GrafanaStack().Tempo()

	pipeline := o.Pipeline("traces", "to-tempo").
		WithReceiver(o.OtlpReceiver("in")).
		WithProcessor(o.BatchProcessor("out")).
		WithExporter(o.OtlpExporter("tempo", fmt.Sprintf("%s:%d", tempoHost, tempoOtlpGrpcPort)))

	endpoint, err := o.Core().
		WithServiceBinding(tempoHost, tempo.Service()).
		WithPipeline(pipeline).
		OtlpGrpcEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve collector otlp/grpc endpoint: %w", err)
	}
	return endpoint, nil
}

// BatchedMetricsPipeline shows the processor stage carrying real weight:
// a memory_limiter in front of a batch processor, in that order. Order is
// the lesson — processors run in the sequence they were added, so the
// limiter has to see data first if it is to shed load before the batcher
// buffers it. Returns the collector's OTLP/HTTP endpoint, the one an
// exporter posting protobuf over HTTP wants.
//
// +cache="never"
func (m *OtelExamples) BatchedMetricsPipeline(ctx context.Context) (string, error) {
	endpoint, err := batchedMetricsCollector().OtlpHTTPEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve collector otlp/http endpoint: %w", err)
	}
	return endpoint, nil
}

// ConfigDumpYaml returns the collector config that BatchedMetricsPipeline
// runs, as a file, without starting anything. ConfigFile is the debugging
// tool for the builder API: when a pipeline misbehaves, render it and read
// the YAML the components were spliced into rather than guessing.
//
//	dagger -m daggerverse/otel/examples/go call config-dump-yaml contents
func (m *OtelExamples) ConfigDumpYaml() *dagger.File {
	return batchedMetricsCollector().ConfigFile()
}

// batchedMetricsCollector is the metrics collector shared by
// BatchedMetricsPipeline and ConfigDumpYaml, so the config the second one
// prints is exactly the config the first one runs.
func batchedMetricsCollector() *dagger.OtelCoreCollector {
	o := dag.Otel()
	pipeline := o.Pipeline("metrics", "batched").
		WithReceiver(o.OtlpReceiver("in")).
		WithProcessor(o.MemoryLimiterProcessor("guard")).
		WithProcessor(o.BatchProcessor("out")).
		WithExporter(o.DebugExporter("stdout"))
	return o.Core().WithPipeline(pipeline)
}

// tunedOtlpReceiverYaml is the body CustomReceiverYaml splices in. It is
// the standard OTLP receiver plus two knobs OtlpReceiver does not expose:
// a raised gRPC message-size ceiling and HTTP metadata propagation.
const tunedOtlpReceiverYaml = `protocols:
  grpc:
    endpoint: 0.0.0.0:4317
    max_recv_msg_size_mib: 16
  http:
    endpoint: 0.0.0.0:4318
    include_metadata: true
`

// CustomReceiverYaml is the escape hatch. The typed factories cover the
// common components, but every collector option they do not expose is
// still reachable: CustomReceiver takes a kind, a name, and a YAML body
// that is spliced verbatim under receivers.<kind>/<name>. The body is
// parsed (so malformed YAML fails at build time, not at collector
// startup) but never interpreted, which is what lets it configure
// components this module has never heard of. CustomProcessor and
// CustomExporter work the same way.
//
// +cache="never"
func (m *OtelExamples) CustomReceiverYaml(ctx context.Context) (string, error) {
	o := dag.Otel()
	pipeline := o.Pipeline("traces", "tuned").
		WithReceiver(o.CustomReceiver("otlp", "tuned", tunedOtlpReceiverYaml)).
		WithExporter(o.DebugExporter("stdout"))

	endpoint, err := o.Core().WithPipeline(pipeline).OtlpGrpcEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve collector otlp/grpc endpoint: %w", err)
	}
	return endpoint, nil
}
