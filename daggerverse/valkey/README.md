# valkey

Daggerverse module that spins up [Valkey](https://valkey.io) topologies
(from the upstream `valkey/valkey` image) — a single node, or a primary
with N read replicas — and exposes a pure-Go client (built on
[`github.com/valkey-io/valkey-go`](https://github.com/valkey-io/valkey-go))
that can target either a local node or a remote Valkey (e.g.
ElastiCache Serverless, MemoryDB, a self-hosted node).

It is the daggerverse's first key-value store, and it targets Valkey
(BSD-3, Linux Foundation) rather than Redis (RSALv2/SSPLv1 since 7.4).

It supports three client-facing listener modes — plaintext (`requirepass`
auth over an unencrypted TCP listener), one-way TLS, and mutual TLS.
Valkey Cluster, `valkey-server` config passthrough, the `valkey-bundle`
image, TLS for the replication link, and keyspace export/import all land
in follow-ups.

## Why `Server` and not `Cluster`

Postgres calls its single node a `Cluster` because that is PostgreSQL's
own word for an instance. In Valkey, "cluster" means slot-sharded Valkey
Cluster, so the single-node type here is `Server` and `Cluster` stays
reserved for the follow-up that adds sharding.

## Security profiles

A `*ServerSecurity` configures the node's `:6379` listener; a matching
`*ClientSecurity` configures how a client connects. Three modes are
supported, and the client's mode must match the node's.

```go
// Plaintext — requirepass auth over an unencrypted TCP listener.
Valkey.PlaintextServerSecurity() *ServerSecurity
Valkey.PlaintextClientSecurity() *ClientSecurity

// TLS — one-way: the node presents serverCert on a --tls-port listener;
// the client verifies it against serverCa. Password auth still applies.
Valkey.TlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret) *ServerSecurity
Valkey.TlsClientSecurity(serverCa *dagger.File) *ClientSecurity

// mTLS — mutual: the client must additionally present clientCert/clientKey
// signed by the node's trusted clientCa.
Valkey.MtlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret, clientCa *dagger.File) *ServerSecurity
Valkey.MtlsClientSecurity(serverCa *dagger.File, clientCert *dagger.File, clientKey *dagger.Secret) *ClientSecurity
```

The cert material is caller-supplied PEM: certs as `*dagger.File`, keys as
`*dagger.Secret`. valkey-server reads them via `--tls-cert-file` /
`--tls-key-file` / `--tls-ca-cert-file`; the client builds a `*tls.Config`
(`RootCAs` pinned to `serverCa`, `ServerName` set to the dialed host) and
hands it to valkey-go via `ClientOption.TLSConfig`.

**Second listener, not an upgrade.** TLS terminates on a *separate*
listener: the node starts with `--tls-port 6379 --port 0`. Leaving `port`
non-zero would keep a plaintext listener alive alongside the encrypted
one, so `--port 0` is what turns plaintext genuinely off.

**The `tls-auth-clients` trap.** It defaults to `yes`, so a TLS-enabled
node demands client certificates unless `--tls-auth-clients no` is passed.
One-way TLS is therefore the opt-*out* here (the module passes the flag),
inverting the postgres posture where mTLS is the opt-in. mTLS leaves the
flag at its default and adds `--tls-ca-cert-file`. Forgetting the flag in
TLS mode would silently produce mTLS that rejects every well-formed
one-way client.

**Empty name rejected for TLS/mTLS.** The node hostname derives from
`name` alone (`valkey-<sha12(name)>`) and the server certificate's SAN
must match the dialed host, so a TLS or mTLS `Server` with an empty
`name` is rejected in the constructor.

**Mode coupling.** `Server.Client(security)` validates that the client's
mode exactly matches the node's listener mode and otherwise returns an
error naming both modes (e.g. *"client uses plaintext but server
listener is TLS"*). The standalone `Valkey.Client(...)` has no server
reference, so it cannot cross-validate — a mismatched standalone client
fails at the wire instead.

## Server

Single-node server listening on 6379 with `requirepass` auth; the
listener mode (plaintext / TLS / mTLS) is chosen by
`clientListenerSecurity`. Built from a single `<registry>/valkey/valkey:<tag>`
image; the `valkey/valkey` portion is fixed and only `registry` and `tag`
are caller-overridable. Default tag is `"9.1"`.

```go
Valkey.Server(
    ctx,
    name="",
    registry="docker.io", tag="9.1",
    password *dagger.Secret,
    clientListenerSecurity *ServerSecurity,
) (*Server, error)

Server.Endpoint() string              // host:6379 (pure accessor; BindServer makes it reachable)
Server.User() string                  // the requirepass user, "default"
Server.Password() *dagger.Secret
Server.BindServer(*dagger.Container) *dagger.Container
Server.Client(ctx, security *ClientSecurity) (*Client, error)
Server.Stop(ctx) error
```

Rejected inputs (each a descriptive error rather than a half-broken or
wide-open boot): `password == nil`, `clientListenerSecurity == nil`, an
incomplete TLS/mTLS profile (missing cert, key, or client CA), and an
empty `name` for a TLS/mTLS node.

The password reaches `valkey-server` through a secret environment
variable expanded by a shell wrapper, not through a literal `argv` entry
— a plaintext `--requirepass <pw>` would be baked into the container's
args and surface in the Dagger graph and traces.

`Server()` is `+cache="session"` so a single test's chained `Client.Set`
→ `Client.Get` calls observe the same backing service and its keyspace.
The `name` argument folds into that session-cache key: pass a unique
value per parallel test to get isolated services. The hostname is
`valkey-<sha12(name)>`, derived from `name` alone so a caller can predict
it when minting a server certificate whose SAN must match the dialed
host. Every method
on `*Server` / `*Client` is `+cache="never"` so any data-returning call
re-executes per invocation.

`Endpoint()` is a pure accessor and does **not** start the service.
Reachability from a consumer container comes from `BindServer`, which
lets `WithServiceBinding` start the service as the consumer's dependency
and wire its IP into `/etc/hosts`. (Pre-starting from this module would
register the service in the module's DNS domain, which a session-domain
consumer's host-file lookup can't resolve.) For module-runtime access,
use `Server.Client`, which starts the service itself.

## Replication

Primary/replica topology: one primary plus `replicas` asynchronous read
replicas, all from the same image. A replica is asymmetric — it dials a
primary that is already up — so the whole topology is an ordinary service
binding, with none of the symmetric-peer startup problems Valkey Cluster
will bring.

```go
Valkey.Replication(
    ctx,
    name="",
    registry="docker.io", tag="9.1",
    replicas=1,
    password *dagger.Secret,
    clientListenerSecurity *ServerSecurity,
) (*Replication, error)

Replication.Primary() *Server    // the only node that accepts writes
Replication.Replicas() []*Server // read replicas, in creation order
Replication.Stop(ctx) error      // tears down every node
```

Each replica boots with `--replicaof <primary-host> 6379`, the primary's
password, and `--replica-read-only yes`. The password is a single secret
shared by the whole topology: it is each node's own `requirepass` *and*
the replicas' `masterauth`, and it reaches every node through the same
secret environment variable, so it never enters any node's `argv` in the
Dagger graph.

**`masterauth`, not `primaryauth`.** Valkey 9.1 accepts both spellings
(verified against the pinned image), but `primaryauth` only exists from
Valkey 8.0 onwards and `tag` is caller-overridable, so the older spelling
is the one that works across every tag a caller could plausibly pass.

**Plaintext only, for now.** A TLS node runs with `--port 0`, so the
replication link would have to run over TLS too (`--tls-replication
yes`), and that link needs trust material a client-listener
`*ServerSecurity` does not carry: the replica must verify the primary
against a CA, and under mTLS present a client certificate the primary's
CA accepts. A TLS or mTLS profile is therefore rejected in the
constructor rather than booting replicas that spin on a failed handshake.

**Hostnames.** Every node's hostname is `valkey-<sha12(...)>` derived from
`name` plus the node's role and index, so each node in a topology gets its
own pinned hostname, two topologies with different `name`s never collide,
and a topology's nodes never collide with a `Valkey.Server` booted under
the same `name`.

**Replication is asynchronous.** A read-your-write assertion against a
replica must poll to a deadline rather than reading once — the write is
acknowledged by the primary before it reaches any replica.

`Primary()` and `Replicas()` are `+cache="session"` rather than
`"never"`: Dagger v0.21 detaches module objects returned from a
`+cache="never"` function when a consumer module reads their fields
lazily, and `tests/` is such a consumer. The `*Server` methods themselves
stay `"never"`, so no data-returning call is served stale.

Rejected inputs: `password == nil`, `clientListenerSecurity == nil`, an
incomplete or non-plaintext security profile, and `replicas < 1` (a
zero-replica topology is a single node with extra steps — use
`Valkey.Server`). Note that the generated Go binding drops an optional
argument holding its zero value, so `Replicas: 0` from Go resolves to the
`+default=1`; `--replicas=0` on the CLI reaches the guard.

## Client

Pure-Go valkey-go based client. No container image. Works against the
local node or any reachable remote Valkey.

```go
Valkey.Client(
    host string, port=6379,
    user="default",
    password *dagger.Secret,
    db=0,
    security *ClientSecurity,
) *Client

Client.Ping(ctx) error
Client.Do(ctx, args []string) (string, error)      // JSON-encoded reply; the escape hatch
Client.Get(ctx, key string) (string, error)        // errors on a missing key
Client.Set(ctx, key, value string, ttl="") error   // ttl is a Go duration string
Client.Del(ctx, keys []string) (int, error)        // number of keys actually removed
Client.Keys(ctx, pattern string) ([]string, error) // SCAN-backed, cursor walked to exhaustion
Client.ApplyFile(ctx, file *dagger.File) error     // one command per line, on one connection
Client.Info(ctx, section="") (string, error)
Client.DbSize(ctx) (int, error)
Client.FlushAll(ctx) error
```

`Do` is the escape hatch — every other method is expressible through it —
and returns the reply JSON-encoded, so the RESP type survives the round
trip:

| reply         | example command       | result       |
| ------------- | --------------------- | ------------ |
| status        | `SET k v`             | `"OK"`       |
| integer       | `INCR n`              | `1`          |
| bulk string   | `GET k`               | `"v"`        |
| array         | `LRANGE l 0 -1`       | `["a","b"]`  |
| nil           | `GET missing`         | `null`       |

It returns a `string` rather than a `*dagger.File` (diverging from
postgres' `QueryJSON`): a single command reply is small, a string keeps
`dagger call do --args=GET,foo` readable without materializing a workdir
file, and a core scalar sidesteps Dagger v0.21's detached-module-object
behaviour entirely. The SUBSCRIBE family is rejected — its replies arrive
out of band, not as a command reply.

`Get` errors on a missing key rather than returning `""`, for the same
reason postgres' `Scalar` errors on SQL NULL: an empty string is a
legitimate stored value and must stay distinguishable from absence.

`Keys` is SCAN-backed rather than `KEYS`-backed (which blocks the server
for the whole sweep) and walks the cursor to exhaustion, so the result is
the complete match set and not just SCAN's first page.

`ApplyFile` is the fixture-seeding path: one command per line in
valkey-cli syntax, run in order on a single connection. Blank lines and
`#` comments are skipped; arguments split on whitespace with single- and
double-quoted runs kept intact (`\` escapes inside double quotes). A
failing command aborts the run and reports its line number.

## Follow-ups

TLS/mTLS for the replication link (`--tls-replication yes` plus the trust
material a replica needs to verify and authenticate to its primary);
Valkey Cluster / slot sharding; `valkey-server` config passthrough
(config file, ACL file, append-only, max-memory, extra args); the
`valkey/valkey-bundle` image with module verification; keyspace
export/import via SCAN + DUMP/RESTORE.
