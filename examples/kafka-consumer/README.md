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

## Run it via Dagger

The `tests/` Dagger module builds this app through the z5labs `GoApp` archetype
and, in its integration tests, stands up the whole stack (a TLS/mTLS Kafka
cluster, a TLS/mTLS Schema Registry, and an OpenTelemetry collector wired to
Tempo/Mimir/Loki), produces framed Avro records, runs this consumer against it,
and asserts it both decoded the records and exported telemetry.

```sh
# from the repo root
dagger -m examples/kafka-consumer/tests call go-app-ci           # GoApp fmt/vet/lint/test + multi-arch build
dagger -m examples/kafka-consumer/tests call mtls-avro-consume   # full mTLS integration (see #147 below)
dagger -m examples/kafka-consumer/tests call tls-avro-consume    # server-TLS variant

# as CI runs it
dagger check 'kafka-consumer-tests:go-app-ci'
```

### Known blocker: the integration tests reproduce #147

`mtls-avro-consume` / `tls-avro-consume` currently **fail** at
`KafkaSchemaRegistry.bindTo`, reproducing
[#147](https://github.com/z5labs/devex/issues/147): the Schema Registry's
advertised alias from `SchemaRegistry.BindTo(ctr)` is not resolvable from a
`WithExec` process (the service handle detaches when it rides on the
cross-module `SchemaRegistry` object). Every mTLS-capable registry backend
(Confluent/Apicurio/Karapace) is a separate service reached via `BindTo`, so
there is no mTLS path that avoids the bug.

Because of this, only `go-app-ci` is a `+check` (it runs in CI); the integration
tests are runnable on demand and serve as a faithful, end-to-end reproduction of
the user experience that triggers #147. They should be promoted back to `+check`
once #147 lands. The consumer's own TLS/mTLS config, Confluent-header parsing,
and Avro decoding are covered offline by `main_test.go`.
