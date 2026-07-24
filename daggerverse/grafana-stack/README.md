# grafana-stack

A Dagger module that spins up Loki, Tempo, and Mimir as Dagger services
for local development and testing, plus a Grafana UI wired up to all
three with file-based datasource and dashboard provisioning. Each
backend runs in single-binary / monolithic mode and exposes both its
native ingest API and an OTLP/HTTP receiver. Every listener defaults to
plaintext; `WithTls` (and optional `WithMtls`) enable TLS on the backends
and on the Grafana UI. See [TLS and mTLS](#tls-and-mtls).

## Backends and ports

| Backend | Native HTTP | OTLP HTTP                            | OTLP gRPC |
|---------|-------------|--------------------------------------|-----------|
| Loki    | `:3100`     | `:3100/otlp/v1/logs`                 | —         |
| Tempo   | `:3200`     | `:4318` (collector appends `/v1/...`)| `:4317`   |
| Mimir   | `:9009`     | `:9009/otlp/v1/metrics`              | —         |
| Grafana | `:3000`     | —                                    | —         |

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

After `WithTls`, the HTTP-scheme endpoints above return `https://` URLs
(`Loki.Endpoint`, `Mimir.Endpoint`, `Tempo.HttpEndpoint`, every
`OtlpHttpEndpoint`, and `Grafana.Endpoint`). `Tempo.OtlpGrpcEndpoint` stays
scheme-less. See [TLS and mTLS](#tls-and-mtls).

## Grafana UI

`Grafana(registry, tag, configFile, adminPassword, storage)` returns a
`*Grafana` builder. `adminPassword` is required and is supplied as a
`*dagger.Secret`; it is mounted into the container and read via
`GF_SECURITY_ADMIN_PASSWORD__FILE` so plaintext never enters generated
bindings. The default tag is `12.0.0`. `configFile` defaults to a
minimal `grafana.ini` that disables analytics and lets every other
setting fall through to the upstream image's default; supplying a
config file fully replaces it (no merge).

Datasources and dashboards are accumulated via builder methods that
return a new `*Grafana`:

```go
g := dag.GrafanaStack().Grafana(adminPassword).
    WithLokiDatasource("loki", dag.GrafanaStack().Loki()).
    WithTempoDatasource("tempo", dag.GrafanaStack().Tempo()).
    WithMimirDatasource("mimir", dag.GrafanaStack().Mimir()).
    WithDashboard("api-overview", dashboardFile).
    WithDashboards(dashboardDir)

svc := g.Service()                    // listens on :3000
url, _ := g.Endpoint(ctx)             // http://<host>:3000
```

The `name` argument to each `WithXDatasource` is used both as the
in-network service hostname **and** the datasource name + UID. Setting
`uid == name` lets callers address the datasource via Grafana's proxy
API without an extra lookup:

```
http://<host>:3000/api/datasources/proxy/uid/<name>/<backend-path>
```

Mimir is registered as a `prometheus`-type datasource pointing at
`http://<name>:9009/prometheus` (Mimir's Prometheus-compatible API
prefix, matching the existing Mimir round-trip test).

Dashboards land in a single flat folder at `/var/lib/grafana/dashboards`
on the container (auto-generated provider config). `WithDashboard(name,
file)` appends `.json` to the supplied name if missing;
`WithDashboards(dir)` includes every `*.json` entry in the directory,
preserving filenames. Callers wanting folder grouping should embed it
in the dashboard JSON itself.

## TLS and mTLS

Every listener defaults to plaintext. `WithTls` enables TLS on every
listener a backend exposes; `WithMtls` additionally requires clients to
present a certificate signed by a supplied CA (mutual TLS). Both are
builder methods that return a new value, so they chain off the
constructor:

```go
loki := dag.GrafanaStack().Loki().
    WithTls(serverCert, serverKey).   // https on :3100 (native + OTLP)
    WithMtls(clientCa)                // require a client cert too

url, _ := loki.OtlpHttpEndpoint(ctx)  // now https://<host>:3100/otlp/v1/logs
```

- `WithTls(serverCert *dagger.File, serverKey *dagger.Secret)` — the
  server certificate (PEM) and its private key (PEM, supplied as a
  `*dagger.Secret` so it never lands in the layer cache). The cert's SAN
  must cover the hostname clients dial. After this call the backend's
  `Endpoint` / `OtlpHttpEndpoint` (and `Tempo.HttpEndpoint`) return
  `https://` URLs.
- `WithMtls(clientCa *dagger.File)` — the CA (PEM) that must have signed
  any client certificate. Must be combined with `WithTls`; `Service`
  returns an error otherwise.

What each backend renders into its config:

| Backend | Native HTTP + OTLP HTTP        | OTLP gRPC                                            |
|---------|-------------------------------|-----------------------------------------------------|
| Loki    | `server.http_tls_config`      | — (internal gRPC stays plaintext)                   |
| Mimir   | `server.http_tls_config`      | — (internal gRPC stays plaintext)                   |
| Tempo   | `server.tls_config` (`:3200`) | `distributor.receivers.otlp.protocols.{grpc,http}.tls` |

Loki and Mimir serve their OTLP HTTP receiver on the same listener as the
native API, so one `http_tls_config` block secures both. Tempo's OTLP
receivers are separate OpenTelemetry Collector components, so TLS is
rendered into each protocol's `tls:` block. Under mTLS the dskit blocks
set `client_auth_type: RequireAndVerifyClientCert`; the OTLP receiver
blocks gain a `client_ca_file` (its presence is what makes the receiver
require a client cert).

`Tempo.OtlpGrpcEndpoint` stays scheme-less in every mode — gRPC callers
supply `host:port` and configure TLS on their own dialer.

If you pass your own `configFile`, the module does **not** splice TLS into
it (you own its `tls:` blocks); it still mounts the cert material at the
fixed paths below and switches the endpoint scheme, so your config can
reference them:

| Backend | cert | key | client CA (mTLS) |
|---------|------|-----|------------------|
| Loki    | `/etc/loki/tls/tls.crt`    | `/etc/loki/tls/tls.key`    | `/etc/loki/tls/ca.crt`    |
| Tempo   | `/etc/tempo/tls/tls.crt`   | `/etc/tempo/tls/tls.key`   | `/etc/tempo/tls/ca.crt`   |
| Mimir   | `/etc/mimir/tls/tls.crt`   | `/etc/mimir/tls/tls.key`   | `/etc/mimir/tls/ca.crt`   |
| Grafana | `/etc/grafana/tls/tls.crt` | `/etc/grafana/tls/tls.key` | —                         |

### Grafana UI TLS

`Grafana.WithTls(serverCert, serverKey)` switches the `:3000` listener from
`http` to `https` (`[server] protocol = https` plus `cert_file` /
`cert_key`). After this call `Grafana.Endpoint` returns an `https://` URL.

There is **no `Grafana.WithMtls`**: Grafana core cannot require client
certificates on its own HTTP listener (its `[server]` section has no
client-cert-auth setting; the upstream guidance is to terminate mTLS at a
reverse proxy). Requiring client certs at the Grafana UI is tracked as a
follow-up. Note this does *not* limit reaching an **mTLS backend** — see
below, where Grafana presents a client certificate to the backend.

### Datasource provisioning over TLS

`WithLokiDatasource` / `WithTempoDatasource` / `WithMimirDatasource` detect
the TLS state of the backend builder and render the datasource YAML
accordingly. Each takes optional client-side TLS material:

```go
g := dag.GrafanaStack().Grafana(adminPassword).
    WithTls(grafanaCert, grafanaKey).
    // TLS backend: Grafana verifies it against caCert.
    WithLokiDatasource("loki", tlsLoki, dagger.GrafanaStackGrafanaWithLokiDatasourceOpts{
        CaCert: caCert,
    }).
    // mTLS backend: Grafana also presents a client cert to reach it.
    WithMimirDatasource("mimir", mtlsMimir, dagger.GrafanaStackGrafanaWithMimirDatasourceOpts{
        CaCert:     caCert,
        ClientCert: clientCert,
        ClientKey:  clientKey,
    })
```

- When the backend has TLS, the datasource URL becomes `https://` and the
  entry sets `jsonData.tlsAuthWithCACert: true` with the CA referenced via
  `secureJsonData.tlsCACert`. Supply `caCert` (the CA that signed the
  backend's server cert); when omitted, the backend's own server cert is
  pinned (correct only for a self-signed server cert).
- When the backend has mTLS, the entry also sets `jsonData.tlsAuth: true`
  with `secureJsonData.tlsClientCert` / `tlsClientKey`. Supply `clientCert`
  / `clientKey`; `Service` returns an error if they are missing.

TLS material is mounted per datasource under
`/etc/grafana/tls/datasources/<name>/` and referenced from the
provisioning YAML via Grafana's `$__file{}` expansion, so the client key
stays a secret mount rather than being inlined into the config.

### Cert material

Pair this module with
[`daggerverse/certificate-management`](../certificate-management) (issues
CA / server / client certs) and [`daggerverse/crypto`](../crypto)
(generates keys); the `tests/` module uses both to mint per-test material.

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

| Test                                | What it does                                                              |
|-------------------------------------|--------------------------------------------------------------------------|
| `LokiAcceptsOtlpLogs`               | POST OTLP/HTTP log → poll LogQL until marker visible.                     |
| `TempoAcceptsOtlpTraces`            | POST OTLP/HTTP span → GET `/api/traces/<hex>` checks.                     |
| `MimirAcceptsOtlpMetrics`           | POST OTLP/HTTP gauge → poll Prometheus API for series.                    |
| `GrafanaProxiesLokiQuery`           | POST OTLP/HTTP log → query through Grafana's datasource proxy API.        |
| `LokiTlsRoundTrip`                  | Push + read back over **https**; assert a plaintext client is rejected.  |
| `LokiMtlsRequiresClientCert`        | mTLS Loki accepts a push with a valid client cert, rejects one without.  |
| `TempoTlsRoundTrip`                 | Push a span + read it back over https; assert plaintext is rejected.     |
| `TempoMtlsRequiresClientCert`       | mTLS Tempo accepts/rejects an OTLP span push by client-cert presence.    |
| `MimirTlsRoundTrip`                 | Push a metric + query it back over https; assert plaintext is rejected.  |
| `MimirMtlsRequiresClientCert`       | mTLS Mimir accepts/rejects an OTLP metric push by client-cert presence.  |
| `GrafanaTlsRejectsPlaintext`        | TLS Grafana answers over https and refuses a plaintext client.           |
| `GrafanaProxiesLokiQueryOverTls`    | TLS Grafana proxies a LogQL query to a TLS Loki datasource end-to-end.   |
| `GrafanaProxiesLokiQueryOverMtls`   | TLS Grafana presents a client cert to an mTLS Loki; a rogue cert fails.  |

The TLS/mTLS tests mint their own CA / server / client certs per run via
the `certificate-management` and `crypto` modules, so nothing is
hard-coded. Each carries `+check` (its own CI runner) and `+cache="never"`.

Run all four in parallel:

```sh
dagger -m daggerverse/grafana-stack/tests call all
```

Run a single one:

```sh
dagger -m daggerverse/grafana-stack/tests call loki-accepts-otlp-logs
dagger -m daggerverse/grafana-stack/tests call tempo-accepts-otlp-traces
dagger -m daggerverse/grafana-stack/tests call mimir-accepts-otlp-metrics
dagger -m daggerverse/grafana-stack/tests call grafana-proxies-loki-query
```

Each backend test exposes an image tag override (default matches the
parent module's pinned defaults) so a fresh upstream release can be
qualified end-to-end without editing any module. The single-backend
tests take `--tag`; `grafana-proxies-loki-query` takes `--grafana-tag`
and `--loki-tag`:

```sh
dagger -m daggerverse/grafana-stack/tests call loki-accepts-otlp-logs --tag=3.5.0
dagger -m daggerverse/grafana-stack/tests call tempo-accepts-otlp-traces --tag=2.8.0
dagger -m daggerverse/grafana-stack/tests call mimir-accepts-otlp-metrics --tag=2.16.0
dagger -m daggerverse/grafana-stack/tests call grafana-proxies-loki-query --grafana-tag=12.1.0
```

`call all` exposes `--loki-tag`, `--tempo-tag`, `--mimir-tag`, and
`--grafana-tag` (each with the same default as its single-test form):

```sh
dagger -m daggerverse/grafana-stack/tests call all --grafana-tag=12.1.0
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
