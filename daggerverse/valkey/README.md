# valkey

Daggerverse module that spins up a single-node [Valkey](https://valkey.io)
server (from the upstream `valkey/valkey` image) and exposes a pure-Go
client (built on
[`github.com/valkey-io/valkey-go`](https://github.com/valkey-io/valkey-go))
that can target either the local node or a remote Valkey (e.g.
ElastiCache Serverless, MemoryDB, a self-hosted node).

It is the daggerverse's first key-value store, and it targets Valkey
(BSD-3, Linux Foundation) rather than Redis (RSALv2/SSPLv1 since 7.4).

This story is plaintext-only: `requirepass` auth over an unencrypted TCP
listener. TLS / mTLS, primary/replica replication, Valkey Cluster,
`valkey-server` config passthrough, the `valkey-bundle` image, and
keyspace export/import all land in follow-ups.

## Why `Server` and not `Cluster`

Postgres calls its single node a `Cluster` because that is PostgreSQL's
own word for an instance. In Valkey, "cluster" means slot-sharded Valkey
Cluster, so the single-node type here is `Server` and `Cluster` stays
reserved for the follow-up that adds sharding.

## Security profiles

A `*ServerSecurity` configures the node's `:6379` listener; a matching
`*ClientSecurity` configures how a client connects. Only plaintext is
constructible today; both types already carry the full three-mode field
set so the TLS follow-up slots in without changing the `Server` or
`Client` signatures.

```go
// Plaintext — requirepass auth over an unencrypted TCP listener.
Valkey.PlaintextServerSecurity() *ServerSecurity
Valkey.PlaintextClientSecurity() *ClientSecurity
```

**Mode coupling.** `Server.Client(security)` validates that the client's
mode exactly matches the node's listener mode and otherwise returns an
error naming both modes (e.g. *"client uses plaintext but server
listener is TLS"*). The standalone `Valkey.Client(...)` has no server
reference, so it cannot cross-validate — a mismatched standalone client
fails at the wire instead.

**Note for the TLS follow-up.** Valkey's `tls-auth-clients` defaults to
`yes`, so a TLS-enabled node demands client certificates unless
`--tls-auth-clients no` is passed: one-way TLS is the opt-*out* here,
inverting the postgres posture where mTLS is the opt-in. TLS is also a
*second* listener (`--tls-port 6379 --port 0`), not an upgrade of the
existing one.

## Server

Single-node server listening on 6379 with `requirepass` auth over
plaintext TCP. Built from a single `<registry>/valkey/valkey:<tag>`
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
wide-open boot): `password == nil`, `clientListenerSecurity == nil`, and
any non-plaintext listener mode.

The password reaches `valkey-server` through a secret environment
variable expanded by a shell wrapper, not through a literal `argv` entry
— a plaintext `--requirepass <pw>` would be baked into the container's
args and surface in the Dagger graph and traces.

`Server()` is `+cache="session"` so a single test's chained `Client.Set`
→ `Client.Get` calls observe the same backing service and its keyspace.
The `name` argument folds into that session-cache key: pass a unique
value per parallel test to get isolated services. The hostname is
`valkey-<sha12(name)>`, derived from `name` alone so a caller can predict
it when minting a server certificate in the TLS follow-up. Every method
on `*Server` / `*Client` is `+cache="never"` so any data-returning call
re-executes per invocation.

`Endpoint()` is a pure accessor and does **not** start the service.
Reachability from a consumer container comes from `BindServer`, which
lets `WithServiceBinding` start the service as the consumer's dependency
and wire its IP into `/etc/hosts`. (Pre-starting from this module would
register the service in the module's DNS domain, which a session-domain
consumer's host-file lookup can't resolve.) For module-runtime access,
use `Server.Client`, which starts the service itself.

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

TLS + mTLS security profiles; primary/replica replication; Valkey Cluster
/ slot sharding; `valkey-server` config passthrough (config file, ACL
file, append-only, max-memory, extra args); the `valkey/valkey-bundle`
image with module verification; keyspace export/import via
SCAN + DUMP/RESTORE.
