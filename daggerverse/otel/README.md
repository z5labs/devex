# otel

A Dagger module that spins up the OpenTelemetry Collector as a service
for local development and testing, in two distributions —
`otel/opentelemetry-collector` (core) and
`otel/opentelemetry-collector-contrib`. A small builder API composes
receivers, processors, and exporters into pipelines without writing
the collector YAML by hand. Plaintext is the only supported transport;
TLS lands in a follow‑up.

## Collectors

```go
// Core distribution.
core := dag.Otel().Core()

// Contrib distribution (identical method set; superset of components).
contrib := dag.Otel().Contrib()
```

`Core` and `Contrib` accept `registry` (default `docker.io`), `tag`
(pinned to a known‑good upstream version), and an optional
`configFile *dagger.File` that, when supplied, fully replaces the
rendered pipeline YAML.

## Components

Receivers, processors, and exporters are constructed independently of
any collector so they can be reused across pipelines. Component IDs in
the rendered YAML are `<kind>/<name>`. Names (and `kind` on the
`Custom*` escape hatches) must match `[A-Za-z0-9_-]+`.

```go
o := dag.Otel()

otlpIn   := o.OtlpReceiver("in")
batch    := o.BatchProcessor("b")
debugOut := o.DebugExporter("dbg")

// Cross‑module wiring uses *dagger.Service + endpoint strings only.
loki := dag.GrafanaStack().Loki()
toLoki := o.OtlpHTTPExporter("loki", "http://loki:3100/otlp")
```

`CustomReceiver`, `CustomProcessor`, and `CustomExporter` accept a
caller‑supplied YAML body that is spliced verbatim under
`<kind>/<name>` in the rendered config.

## Pipelines

```go
logs := o.Pipeline("logs", "p").
    WithReceiver(otlpIn).
    WithProcessor(batch).
    WithExporter(toLoki)

// Pre‑wired smoke‑test pipeline: otlp → batch → debug.
smoke := o.DebugPipeline("logs")

col := o.Core().
    WithServiceBinding("loki", loki.Service()).
    WithPipeline(logs)
```

The collector deduplicates components shared across pipelines into a
single top‑level entry per `<kind>/<name>` at YAML‑render time.

## Inspecting the rendered config

```go
contents, _ := col.ConfigFile().Contents(ctx)
fmt.Println(contents)
```

`ConfigFile` returns either the caller‑supplied override or the
pipeline‑rendered YAML; inspecting it does not launch the service.

## Service + endpoints

```go
svc := col.Service()                     // listens on :4317 (OTLP gRPC) + :4318 (OTLP HTTP)
grpc, _ := col.OtlpGrpcEndpoint(ctx)     // <host>:4317   (no scheme)
http, _ := col.OtlpHttpEndpoint(ctx)     // http://<host>:4318
```

When neither pipelines nor an override config are supplied, `Service`
launches the collector with no `--config` flag and the binary refuses
to start — a deliberate failure mode.

## Tests

End‑to‑end checks against the `grafana-stack` backends live in
`./tests/`:

```sh
dagger -m daggerverse/otel/tests call all
```
