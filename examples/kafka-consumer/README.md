# kafka-consumer

A runnable reference Kafka consumer that ties the z5labs devex daggerverse
modules together end to end. It is intentionally a single `package main` driven
by flags/env — the "production-ready" bar here is the **integration** (TLS,
Schema Registry, OpenTelemetry) and how it's built and exercised with Dagger,
not the application-code architecture.

## What it does

- **Consumes** Avro records from a topic with a franz-go
  ([`github.com/twmb/franz-go`](https://github.com/twmb/franz-go)) consumer-group
  client.
- **Resolves the writer schema by id** from a Confluent-compatible Schema
  Registry. Records are in the Confluent wire format — a magic byte, a 4-byte
  big-endian schema id, then the Avro binary body — and the consumer fetches the
  schema for that id over HTTPS and decodes with
  [`github.com/z5labs/avro-go`](https://github.com/z5labs/avro-go).
- **TLS everywhere.** The broker dial (`kgo.DialTLSConfig`) and the Schema
  Registry HTTPS calls both verify the server against a CA truststore. Supplying
  a client keystore upgrades **both** hops to mTLS. There is no plaintext code
  path: the registry URL must be `https://` and a truststore is mandatory.
- **OpenTelemetry.** The franz-go
  [`kotel`](https://github.com/twmb/franz-go/tree/master/plugin/kotel) plugin
  emits a fetch span per poll plus client metrics; the consumer additionally
  opens a process span per record and emits an OTel log per record. All three
  signals are exported over **OTLP/gRPC** to `OTEL_EXPORTER_OTLP_ENDPOINT` via
  the OTel Go SDK.

To stay testable it consumes `-max-records` records, flushes telemetry, and
exits 0. It prints one JSON line per decoded record to stdout:

```json
{"topic":"events","partition":0,"offset":0,"schemaId":1,"value":{"x":"hello-world"}}
```

## Flags / environment

Each flag defaults to the matching environment variable, so the same binary is
driven by `-flag value` locally or purely by env from the Dagger harness. The
two **passwords are read from the environment only** (never a flag) so they
don't leak into `ps`.

| Flag | Env | Meaning |
| ---- | --- | ------- |
| `-brokers` | `BROKERS` | comma-separated `host:port` bootstrap brokers |
| `-topic` | `TOPIC` | topic to consume |
| `-group` | `GROUP` | consumer-group id (default `kafka-consumer`) |
| `-registry-url` | `REGISTRY_URL` | Schema Registry base URL — must be `https://host:port` |
| `-truststore` | `TRUSTSTORE` | path to the PKCS#12 CA truststore (**mandatory**) |
| — | `TRUSTSTORE_PASSWORD` | truststore password |
| `-keystore` | `KEYSTORE` | path to a PKCS#12 client keystore; **when set, both hops use mTLS** |
| — | `KEYSTORE_PASSWORD` | keystore password (required if `-keystore` is set) |
| `-max-records` | `MAX_RECORDS` | consume N records, flush, exit 0 (default 1) |
| `-timeout` | `TIMEOUT` | overall consume deadline (default `30s`) |

OpenTelemetry export is configured with the standard OTel env vars:
`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_SERVICE_NAME`.

## Cert material

The consumer consumes **PKCS#12** keystores/truststores directly — this is what
the devex `certificate-management` and `kafka` modules emit, so the Dagger
harness passes them through with zero conversion. TLS-only needs just the CA
truststore (server verification); mTLS additionally needs a client keystore
holding a `clientAuth` leaf.

To adapt this to **PEM** material (`ca.crt` / `client.crt` / `client.key`), swap
the two `pkcs12.Decode*` calls in `buildTLSConfig` (`main.go`) for
`x509.CertPool.AppendCertsFromPEM` and `tls.LoadX509KeyPair`.

> **Note on OTLP.** "TLS everywhere" applies to the broker and Schema Registry
> data-plane hops. Telemetry is exported to the trusted in-cluster OpenTelemetry
> collector, whose OTLP receiver terminates plaintext; the harness sets
> `OTEL_EXPORTER_OTLP_INSECURE=true`.

## Run it locally

You need a TLS Kafka broker, a TLS Schema Registry with an Avro subject
registered and some framed records produced, an OTLP collector, and the CA
truststore (plus a client keystore for mTLS). Then:

```sh
go run . \
  -brokers broker:9092 \
  -topic events \
  -registry-url https://schema-registry:8081 \
  -truststore ./ca.p12 \
  -keystore ./client.p12 \
  -max-records 3
# with TRUSTSTORE_PASSWORD / KEYSTORE_PASSWORD and
# OTEL_EXPORTER_OTLP_ENDPOINT set in the environment.
```

## Run it via Dagger — the `kafka-consumer` module

The example ships its own Dagger module, rooted at the example directory so
`dagger call` works from anywhere inside `examples/kafka-consumer/` as if it were
its own repo. Its `dagger-module.toml` lives at the example root (`source = "ci"`,
code in `ci/`); the module object is `KafkaConsumer`. It provides two things: a
`run-against` chain that codifies how to run the app, and the build/integration
checks.

It is named `kafka-consumer` rather than `ci` because the repository's root module
is already `ci`: a Dagger workspace that loads both renames this one after its
directory, and then finds no `Ci` main object under that name.

```sh
cd examples/kafka-consumer

dagger call run-against local        # stand up the local stack + run the app
dagger call go-ci                    # Go chain gofmt/vet/lint/test -race (+check)
dagger call mtls-avro-consume        # full mTLS integration — +check, red until #441 (see below)
dagger call tls-avro-consume         # server-TLS variant, on demand

# the two +checks CI runs
dagger check 'ci:go-ci' 'ci:mtls-avro-consume'
```

### `run-against local` — the codified run configuration

`run-against local` is the Dagger-native replacement for a `make` + docker-compose
"up": one command spins up every dependency the consumer needs — a single-node
Apache Kafka broker (KRaft) with a **separate** Confluent Schema Registry over
TLS, and an OpenTelemetry collector fronting Tempo/Mimir/Loki — seeds the topic
with framed Avro records, then builds and runs this consumer against the whole
stack, returning its stdout. It codifies the "run configuration" you'd otherwise
wire up by hand in an IDE, so it is reproducible and shareable.

The chain is designed to grow a sibling — `run-against non-prod` — that points the
same consumer container at services already deployed in a non-prod environment
instead of standing them up locally.

### Known blocker: `assertTelemetry` (#441)

`mtls-avro-consume` and `tls-avro-consume` stand up the stack, decode every Avro
record, and then **fail in `assertTelemetry`**: none of Tempo, Mimir or Loki
reports the consumer's telemetry. That assertion had never executed before this
module moved to Dagger v1 (see below), and the bug is tracked in
[#441](https://github.com/z5labs/devex/issues/441). `mtls-avro-consume` stays a
`+check`, so CI carries a live **red** signal that tracks it; `go-ci` (the build
check) stays green. `run-against local` asserts no telemetry and passes. The
consumer's own TLS/mTLS config, Confluent-header parsing, and Avro decoding are
covered offline by `main_test.go`.

#### History: #147, fixed by the v1 engine

On Dagger v0.21.x all three **failed at hosts-file setup**, reproducing
[#147](https://github.com/z5labs/devex/issues/147):

```
lookup <alias> for hosts file: ... no such host
```

A service given a custom hostname is namespaced into the DNS domain of whichever
module first *starts* it, while the consuming exec searches only its own module
domain plus the session domain. Both binds were affected —
`Cluster.BindBrokers` for the brokers and `SchemaRegistry.BindTo` for the
registry — and which alias the error named varied from run to run.
[dagger/dagger#13751](https://github.com/dagger/dagger/pull/13751) fixes it in
**v1.0.0-beta.12 and later** (no v0.21.x release carries it), and this module is
pinned to v1.0.0-beta.15.

| Engine | `dagger call run-against local` |
| --- | --- |
| `v0.21.8` | fails at hosts-file setup, `lookup broker-… no such host` |
| `v1.0.0-beta.13` | passes: all three Avro records decoded, 1m52s |
