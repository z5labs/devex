# grafana-stack

A Dagger module that spins up Loki, Tempo, and Mimir as Dagger services
for local development and testing. Each backend runs in single-binary /
monolithic mode and exposes both its native ingest API and an OTLP/HTTP
receiver. Plaintext is the only supported transport on every listener.

The Grafana UI itself is intentionally out of scope for this module —
that lands in a follow-up.

## Backends and ports

| Backend | Native HTTP | OTLP HTTP                            | OTLP gRPC |
|---------|-------------|--------------------------------------|-----------|
| Loki    | `:3100`     | `:3100/otlp/v1/logs`                 | —         |
| Tempo   | `:3200`     | `:4318` (collector appends `/v1/...`)| `:4317`   |
| Mimir   | `:9009`     | `:9009/otlp/v1/metrics`              | —         |

## Quickstart

```go
loki := dag.GrafanaStack().Loki()
tempo := dag.GrafanaStack().Tempo()
mimir := dag.GrafanaStack().Mimir()

// Bind into a downstream container.
client := dag.Container().From("alpine").
    WithServiceBinding("loki", loki.Service()).
    WithServiceBinding("tempo", tempo.Service()).
    WithServiceBinding("mimir", mimir.Service()).
    WithExec([]string{"sh", "-c", "echo using loki=$LOKI_URL"})
```

Or grab a URL from the host side:

```go
url, _ := loki.OtlpHttpEndpoint(ctx) // http://<host>:3100/otlp/v1/logs
```

## Constructor inputs (all three)

```
Loki(registry, tag, configFile, storage)
Tempo(registry, tag, configFile, storage)
Mimir(registry, tag, configFile, storage)
```

- `registry` — defaults to `docker.io`. The image is built as
  `<registry>/grafana/<product>:<tag>`. The `grafana/<product>` portion
  is fixed and not caller-overridable.
- `tag` — defaults to a known-good upstream version (Loki `3.4.1`,
  Tempo `2.7.1`, Mimir `2.15.1`).
- `configFile *dagger.File` — `+optional`. Fully replaces the embedded
  default when supplied. The defaults live in `configs/{loki,tempo,mimir}.yaml`.
- `storage *dagger.CacheVolume` — `+optional`. When non-nil, the
  product's data dir (`/var/lib/<product>`) is mounted as a Dagger
  cache volume so writes survive across runs. When nil, the data dir
  is mounted as an empty `*dagger.Directory` (ephemeral; everything
  vanishes when the service stops).

## Endpoint signatures and `ctx`

The story description shows endpoint methods as `Endpoint() string`.
The actual Go signatures take `(ctx context.Context) (string, error)`
because Dagger's `*Service.Endpoint` resolves the host:port pair via a
GraphQL roundtrip, which can fail. The CLI surface still works the same
way (`dagger call loki endpoint`), and the SDK simply passes ctx as the
first argument.

```go
url, err := loki.Endpoint(ctx)         // http://<host>:3100
url, err = loki.OtlpHttpEndpoint(ctx)  // http://<host>:3100/otlp/v1/logs

httpURL, err := tempo.HttpEndpoint(ctx)         // http://<host>:3200
otlpHTTP, err := tempo.OtlpHttpEndpoint(ctx)    // http://<host>:4318
otlpGRPC, err := tempo.OtlpGrpcEndpoint(ctx)    // <host>:4317  (no scheme)

url, err = mimir.Endpoint(ctx)         // http://<host>:9009
url, err = mimir.OtlpHttpEndpoint(ctx) // http://<host>:9009/otlp/v1/metrics
```

## Plaintext-only

Every listener is plaintext HTTP/gRPC. The story explicitly defers TLS
to a follow-up, so there is no `tls:` block in any default config and no
mechanism for the constructor functions to materialize a serving cert.
If you need TLS today, supply a fully-formed `configFile` with your own
TLS configuration and front the service with your own proxy.

For TLS material, pair this module with
[`daggerverse/certificate-management`](../certificate-management) and
[`daggerverse/crypto`](../crypto).

## Default configs

Each backend ships an embedded default config tuned for monolithic /
single-binary mode:

- **Loki** — `auth_enabled: false`, single-binary ring with `inmemory`
  kvstore, filesystem storage at `/var/lib/loki`,
  `limits_config.allow_structured_metadata: true` so the OTLP HTTP
  receiver accepts default-shaped payloads, schema `v13` `tsdb`.
- **Tempo** — single-binary, `distributor.receivers.otlp` enabling
  both the gRPC (`0.0.0.0:4317`) and HTTP (`0.0.0.0:4318`) receivers,
  local backend with WAL + blocks under `/var/lib/tempo`.
- **Mimir** — `multitenancy_enabled: false` (anonymous tenant),
  `-target=all` (passed to the binary explicitly so the upstream image
  default is overridden), single-binary ingester / store-gateway /
  compactor rings using `inmemory` kvstores, filesystem block / rules
  / TSDB / compactor storage rooted at `/var/lib/mimir`.

To override, pass a `*dagger.File` for `configFile`. The supplied file
fully replaces the default — it is not merged.

## Container user

Every backend is invoked as `root` inside its container
(`WithUser("0:0")`). The upstream Grafana images each ship with a
non-root `USER` directive that varies per product (`10001:10001` on
Loki, sometimes a named user on others); running as root sidesteps
"permission denied" on the data-dir mount without us second-guessing
each image's intended UID. This is fine for ephemeral dev / test
services but means the module is not appropriate as-is for a
multi-tenant production deployment.

## Tests

`tests/` contains a sibling Dagger module that exercises each backend
end-to-end:

| Test                       | What it does                                           |
|----------------------------|--------------------------------------------------------|
| `LokiAcceptsOtlpLogs`      | POST OTLP/HTTP log → poll LogQL until marker visible.  |
| `TempoAcceptsOtlpTraces`   | POST OTLP/HTTP span → GET `/api/traces/<hex>` checks.  |
| `MimirAcceptsOtlpMetrics`  | POST OTLP/HTTP gauge → poll Prometheus API for series. |

Run all three in parallel:

```sh
dagger -m daggerverse/grafana-stack/tests call all
```

Run a single one:

```sh
dagger -m daggerverse/grafana-stack/tests call loki-accepts-otlp-logs
dagger -m daggerverse/grafana-stack/tests call tempo-accepts-otlp-traces
dagger -m daggerverse/grafana-stack/tests call mimir-accepts-otlp-metrics
```

Note the kebab-case CLI form (`loki-accepts-otlp-logs`); the Go SDK
exposes the camel-case method names directly.

### Encoding gotchas exercised by the tests

- Tempo's OTLP/HTTP JSON receiver expects `traceId` / `spanId` as **hex
  strings**, but it re-encodes them as **base64** in the response from
  `/api/traces/<id>`. The Tempo test sends hex on the way in and looks
  for base64 in the response.
- The `curlimages/curl` image is Alpine-based, so `date +%N` is silently
  dropped by busybox `date`. The tests synthesize nanosecond timestamps
  by appending nine zeros to `date +%s`.
- Loki rejects writes outside any configured schema period. The default
  config sets `from: 2020-05-15` so any contemporary timestamp lands in
  the only configured schema.
