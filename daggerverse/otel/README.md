# otel

A Dagger module that spins up the OpenTelemetry Collector as a service
for local development and testing, in two distributions —
`otel/opentelemetry-collector` (core) and
`otel/opentelemetry-collector-contrib`. A small builder API composes
receivers, processors, and exporters into pipelines without writing
the collector YAML by hand. Transports default to plaintext; TLS and
mTLS are opt‑in (see [TLS](#tls)).

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
http, _ := col.OtlpHttpEndpoint(ctx)     // http://<host>:4318 (https:// once WithTls is set)
```

When neither pipelines nor an override config are supplied, `Service`
launches the collector with no `--config` flag and the binary refuses
to start — a deliberate failure mode.

## TLS

Cert material crosses the module boundary as `*dagger.File` (public
certs) and `*dagger.Secret` (private keys), both PEM‑encoded; the
[`certificate-management`](../certificate-management) and
[`crypto`](../crypto) modules mint it.

### Receiver side

`WithTls` terminates TLS on both OTLP receivers (gRPC :4317 and HTTP
:4318); `WithMtls` additionally requires a client certificate signed by
the supplied CA on every incoming connection (and must be combined with
`WithTls`).

```go
col := o.Core().
    WithPipeline(logs).
    WithTls(serverCert /* *dagger.File */, serverKey /* *dagger.Secret */).
    WithMtls(clientCa /* *dagger.File */) // optional

http, _ := col.OtlpHttpEndpoint(ctx)     // now https://<host>:4318
```

After `WithTls`, `OtlpHttpEndpoint` returns an `https://` URL;
`OtlpGrpcEndpoint` stays scheme‑less, so configure gRPC clients for TLS
out of band.

### Exporter side

`OtlpExporter` and `OtlpHTTPExporter` take optional `CaCert`,
`ClientCert`, and `ClientKey`. Setting `CaCert` pins the receiver's CA
(and switches the exporter off plaintext); `ClientCert` + `ClientKey`
(required together) present an mTLS identity.

```go
toRecv := o.OtlpHTTPExporter("recv", "https://recv:4318",
    dagger.OtelOtlpHTTPExporterOpts{
        CaCert:     recvCa,     // *dagger.File
        ClientCert: clientCert, // *dagger.File (optional; with ClientKey)
        ClientKey:  clientKey,  // *dagger.Secret
    })
```

## Tests

End‑to‑end checks against the `grafana-stack` backends live in
`./tests/`:

```sh
dagger -m daggerverse/otel/tests call all
```
