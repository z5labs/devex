# dgraph

Daggerverse module that spins up a Dgraph graph-database cluster
(one Zero coordinator + N Alpha data nodes grouped at a configurable
replication factor) and exposes a pure-Go client (built on
[`github.com/dgraph-io/dgo`](https://github.com/dgraph-io/dgo)) that
can target either the local cluster or a remote one
(e.g. Dgraph Cloud, an existing self-hosted cluster).

Every client-facing listener supports **plaintext, one-way TLS, or
mutual TLS**. ACL / Login, the Ratel UI service, multi-Zero HA, and
RDF / delete-mutation forms all land in follow-ups (see
[Follow-ups](#follow-ups) below).

## Security profiles

`*ServerSecurity` configures how a cluster's listeners authenticate and
encrypt traffic; `*ClientSecurity` configures how a dgo client connects.
The two are distinct types so a cluster and its client are chosen
independently but coupled at connect time (see
[Mode coupling](#mode-coupling)).

```go
// Server (applied to every Alpha + Zero listener via Dgraph's --tls superflag)
Dgraph.PlaintextServerSecurity() *ServerSecurity
Dgraph.TlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret) *ServerSecurity
Dgraph.MtlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret, clientCa *dagger.File) *ServerSecurity

// Client (dgo *tls.Config material)
Dgraph.PlaintextClientSecurity() *ClientSecurity
Dgraph.TlsClientSecurity(serverCa *dagger.File) *ClientSecurity
Dgraph.MtlsClientSecurity(serverCa *dagger.File, clientCert *dagger.File, clientKey *dagger.Secret) *ClientSecurity
```

Cert material is caller-supplied PEM (`*dagger.File` for certs / CAs,
`*dagger.Secret` for private keys) — pair with the
`certificate-management` and `crypto` modules to mint it.

### TLS / mTLS behaviour

- **`TlsServerSecurity`** enables one-way TLS on every listener. Dgraph
  boots each node with `--tls "server-cert=…; server-key=…"`; the default
  `client-auth-type=VERIFYIFGIVEN` means clients need not present a
  certificate, so a plain `TlsClientSecurity` (pinning the server CA)
  connects cleanly.
- **`MtlsServerSecurity`** additionally passes `ca-cert=<clientCa>` and
  `client-auth-type=REQUIREANDVERIFY`, so connecting clients must present
  a certificate signed by `clientCa`. A `MtlsClientSecurity` supplies the
  matching client leaf + key.
- Inter-node traffic (Alpha↔Zero) stays plaintext (`internal-port=false`),
  so no peer certificates are needed.
- The server certificate's SAN must cover the Alpha / Zero hostnames the
  client dials — the dgo client and `wget` verify the presented cert
  against the dialed host. Cluster hostnames derive from `name`, so a
  TLS / mTLS cluster requires a non-empty `name` (enforced by the
  constructor).

> **Note on flag naming.** Dgraph v24 configures TLS through a single
> `--tls` *superflag* (`key=value;`-delimited), not the discrete
> `--tls.server_cert` flags an earlier draft of this feature assumed. The
> `certificate-management` / `crypto` dependencies live in
> `tests/dagger.json` (cert generation is a test-time concern); the module
> itself only takes `*dagger.File` / `*dagger.Secret`.

### Mode coupling

`Cluster.Client(security)` validates that the supplied `*ClientSecurity`
mode exactly matches the cluster's client-facing listener mode and
returns an error naming both modes on a mismatch
(e.g. `client uses plaintext but cluster listener is TLS`) — before any
wire activity. The standalone `Dgraph.Client(endpoints, security)`
constructor has no cluster reference and cannot perform this check; a
mismatched standalone client instead fails at the gRPC handshake
(e.g. a TLS-only client against an mTLS listener is rejected for
presenting no certificate).

## Cluster

Single Zero, N Alphas grouped at replication factor `replicas`. The
`clientListenerSecurity` profile is applied uniformly to every Alpha
(client-facing HTTP :8080 / gRPC :9080) and Zero (admin HTTP :6080)
listener. Built from a single `<registry>/dgraph/dgraph:<tag>` image; the
`dgraph/dgraph` portion is fixed and only `registry` and `tag` are
caller-overridable.

```go
Dgraph.Cluster(
    ctx,
    zeros=1, alphas=1, replicas=1,
    registry="docker.io", tag="v24.0.4",
    clientListenerSecurity *ServerSecurity,
) *Cluster, error

Cluster.GrpcEndpoints(ctx) ([]string, error)
Cluster.HttpEndpoints(ctx) ([]string, error)
Cluster.AlphaHostNames() []string
Cluster.BindAlphas(*dagger.Container) *dagger.Container
Cluster.Client(ctx, security *ClientSecurity) (*Client, error)
Cluster.Stop(ctx) error
```

### Endpoint scheme

`GrpcEndpoints` returns scheme-less `host:9080` pairs in every mode (dgo
takes a bare address). `HttpEndpoints` returns scheme-less `host:8080`
for a plaintext cluster and `https://host:8080` once the client-facing
listener is TLS or mTLS.

### Topology constraints

The following inputs are rejected with a descriptive error rather than
booting a half-broken cluster:

- **`zeros != 1`** — multi-Zero quorum needs every peer's address at
  static config time via `--peer`, which Dagger's `WithServiceBinding`
  model can't express without an unresolvable cycle. Multi-Zero HA
  lands in a follow-up.
- **`alphas < 1`** or **`replicas < 1`**.
- **`replicas > 1 && replicas % 2 == 0`** — Dgraph's Raft consensus
  requires an odd number of replicas per group (or `replicas=1` for
  no replication).
- **`alphas % replicas != 0`** — Dgraph requires every group to be
  full.
- **`clientListenerSecurity == nil`** — plaintext must be a
  deliberate caller choice so a TLS upgrade stays explicit. TLS / mTLS
  profiles are additionally rejected if their cert material is
  incomplete (TLS needs `serverCert` + `serverKey`; mTLS also needs
  `clientCa`).
- **`name == ""` for a TLS / mTLS cluster** — the Alpha / Zero hostnames
  derive from `name`, and the server certificate's SAN must match them,
  so each TLS / mTLS cluster needs a unique `name`.

### Function caching

`Dgraph.Cluster` carries `+cache="session"`, **not** `+cache="never"`
as the original story suggested. Chained method calls on the returned
cluster (e.g. `Client.Mutate → Client.RunQuery` in
`client-mutate-then-query-round-trip`) need to observe the same
backing services to preserve graph state, and `+cache="never"` on the
generator re-spawns the cluster between method calls (verified during
implementation). Every method on `*Cluster` and `*Client` still
carries `+cache="never"` on its own line so any data-returning call
re-executes per invocation.

Because identical-shape `Cluster()` calls within one engine session
collapse to the same backing services, the `All()` test aggregator
runs serially by default (`--parallel=1`). Individual tests are
parallel-safe to run via `dagger -m daggerverse/dgraph/tests call <name>`
in separate engine sessions.

## Client

Pure-Go dgo-based client. No container image. Each method opens a
fresh `*grpc.ClientConn` to every endpoint and closes it on return,
so calls are stateless from Dagger's perspective. Works against the
local cluster or any reachable remote cluster (e.g. Dgraph Cloud).

```go
Dgraph.Client(grpcEndpoints []string, security *ClientSecurity) *Client

Client.DropAll(ctx) error
Client.AlterSchema(ctx, schema string) error
Client.Mutate(ctx, setJson string, commit bool) (string, error)
Client.RunQuery(ctx, dql string) (string, error)
Client.QueryWithVars(ctx, dql string, varsJson string) (string, error)
```

Naming caveats — deliberate deviations from the story spec:

- **`RunQuery`** rather than `Query`: Dagger's Go SDK codegen
  allocates a struct field named after the lowercase method name to
  cache the result, and `query` collides with the always-present
  querybuilder field on every generated object type. `RunQuery`
  (`runQuery` field) sidesteps the collision while preserving the
  verb-noun shape callers expect.
- **`QueryWithVars(..., varsJson string)`** rather than
  `vars map[string]string`: Dagger function signatures don't support
  Go map parameters, so the variable bindings are passed as a
  JSON-encoded string and unmarshalled inside the method.

`Mutate` returns the assigned-UIDs JSON object
(`{"<blank-node>":"<uid>"}`). `commit=false` runs the mutation as a
dry run (txn discarded, no triples persisted). `RunQuery` and
`QueryWithVars` return the Dgraph response JSON body verbatim
(the `data` object).

## Tests

After `dagger develop` in both `daggerverse/dgraph` and
`daggerverse/dgraph/tests`, run any individual test:

```sh
dagger -m daggerverse/dgraph/tests call defaults-produce-working-single-node-cluster
dagger -m daggerverse/dgraph/tests call client-mutate-then-query-round-trip
dagger -m daggerverse/dgraph/tests call cluster-mtls-round-trip-from-client
# ... etc
```

Tests are grouped into three `+check` aggregators, each scheduled onto
its own CI runner: `validation` (input rejections + cache-directive
probes), `cluster` (plaintext topology / client round-trips), and
`security` (TLS / mTLS round-trips, mode coupling, and container
binding). `dagger -m daggerverse/dgraph/tests call all` runs every test
in one engine session.

## Follow-ups

Out of scope in this story; tracked separately:

- **Inter-node TLS** (`internal-port=true`) — encrypting Alpha↔Zero
  traffic, which needs peer client certs. Client-facing TLS / mTLS is
  supported (see [Security profiles](#security-profiles)).
- **Multi-Zero HA** (Zero quorum with `--peer` flags).
- **ACL / Login** (Dgraph Enterprise auth).
- **Ratel UI** service for browsing the cluster from a web UI.
- **RDF mutation form** and **delete mutations** (`del`/`delJson`).
