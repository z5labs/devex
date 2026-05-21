# dgraph

Daggerverse module that spins up a Dgraph graph-database cluster
(one Zero coordinator + N Alpha data nodes grouped at a configurable
replication factor) and exposes a pure-Go client (built on
[`github.com/dgraph-io/dgo`](https://github.com/dgraph-io/dgo)) that
can target either the local cluster or a remote one
(e.g. Dgraph Cloud, an existing self-hosted cluster).

This story is **plaintext-only** on every listener (client-facing and
internal). TLS / mTLS, ACL / Login, the Ratel UI service, multi-Zero
HA, and RDF / delete-mutation forms all land in follow-ups (see
[Follow-ups](#follow-ups) below).

## Security profiles

Empty-struct profile types are kept distinct so future TLS / mTLS
constructors slot in without changing existing `Cluster` / `Client`
signatures.

```go
Dgraph.PlaintextServerSecurity() *ServerSecurity
Dgraph.PlaintextClientSecurity() *ClientSecurity
```

## Cluster

Single Zero, N Alphas grouped at replication factor `replicas`. All
listeners are plaintext. Built from a single `<registry>/dgraph/dgraph:<tag>`
image; the `dgraph/dgraph` portion is fixed and only `registry` and
`tag` are caller-overridable.

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
  deliberate caller choice so a future TLS upgrade stays explicit.

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
`daggerverse/dgraph/tests`, run any of the fifteen individual tests:

```sh
dagger -m daggerverse/dgraph/tests call defaults-produce-working-single-node-cluster
dagger -m daggerverse/dgraph/tests call client-mutate-then-query-round-trip
# ... etc
```

`dagger -m daggerverse/dgraph/tests call all` runs every test
serially in one engine session.

## Follow-ups

Out of scope in this story; tracked separately:

- **TLS / mTLS** on client-facing and inter-node listeners.
- **Multi-Zero HA** (Zero quorum with `--peer` flags).
- **ACL / Login** (Dgraph Enterprise auth).
- **Ratel UI** service for browsing the cluster from a web UI.
- **RDF mutation form** and **delete mutations** (`del`/`delJson`).
