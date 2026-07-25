# valkey

Daggerverse module that spins up [Valkey](https://valkey.io) topologies
(from the upstream `valkey/valkey` image) — a single node, a primary with
N read replicas, or a slot-sharded Valkey Cluster — and exposes a pure-Go
client (built on
[`github.com/valkey-io/valkey-go`](https://github.com/valkey-io/valkey-go))
that can target either a local topology or a remote Valkey (e.g.
ElastiCache Serverless, MemoryDB, a self-hosted node).

It is the daggerverse's first key-value store, and it targets Valkey
(BSD-3, Linux Foundation) rather than Redis (RSALv2/SSPLv1 since 7.4).

It supports three client-facing listener modes — plaintext (`requirepass`
auth over an unencrypted TCP listener), one-way TLS, and mutual TLS. A
single node can also be booted from `valkey/valkey-bundle`, the upstream
image carrying the JSON, Bloom, and Search modules — see
[`BundleServer`](#bundleserver). TLS for the replication link and the
cluster bus, and keyspace export/import, land in follow-ups.

## Why `Server` and not `Cluster`

Postgres calls its single node a `Cluster` because that is PostgreSQL's
own word for an instance. In Valkey, "cluster" means slot-sharded Valkey
Cluster, so the single-node type here is `Server` and `Cluster` is the
sharded topology.

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
    configFile *dagger.File = nil,          // a valkey.conf, loaded before every flag
    aclFile *dagger.Secret = nil,           // an ACL file, mounted as a secret
    appendOnly bool = false,                // --appendonly yes
    maxMemory string = "",                  // --maxmemory 512mb
    maxMemoryPolicy string = "noeviction",  // --maxmemory-policy allkeys-lru
    extraArgs []string = nil,               // unsupported escape hatch, appended last
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
incomplete TLS/mTLS profile (missing cert, key, or client CA), an empty
`name` for a TLS/mTLS node, a malformed `maxMemory` or unknown
`maxMemoryPolicy`, and an `aclFile` that never mentions the `default`
user.

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

### Configuration passthrough

`configFile`, `aclFile`, `appendOnly`, `maxMemory`, `maxMemoryPolicy`,
and `extraArgs` are all optional, and a call that omits every one of them
produces exactly the node it produced before they existed. They are
constructor parameters rather than post-boot modifiers because
`valkey-server` reads each of them only while starting up. They are
`Server`-only: `Replication` and `Cluster` own their members' boot flags.

Precedence runs left to right along the command line, which Valkey
resolves last-one-wins:

```
<configFile>  <listener flags>  <passthrough flags>  <extraArgs>
```

So a flag argument always beats the same directive in `configFile`, and
`extraArgs` beats everything — including this module's own choices. A
passthrough parameter left at its default emits **no flag at all**, which
is what lets `configFile` govern the settings you did not name: were
`--appendonly no` rendered unconditionally, a config file that turned the
AOF on would be silently overridden and you would have no way to express
"leave it to the file".

`aclFile` is a `*dagger.Secret`, not a `*dagger.File` — an ACL file
carries per-user password material, so it is mounted as a secret and
never lands in an image layer or the Dagger graph. It must contain a
`user default ...` rule. Valkey loads the ACL file *after* `requirepass`
and recreates any user the file omits in its factory `on nopass` state,
so a file listing only your own users would silently drop the password
from `default` and leave the node open — `Server` rejects that outright,
the same way it rejects a nil password. Restate the credential (`user
default on >$password ~* &* +@all`) or disable the user deliberately
(`user default off`). A `configFile` carrying its own `user ...`
directives cannot be combined with an `aclFile` at all: `valkey-server`
refuses to start when both are present, and this module does not read the
config file, so that one surfaces as a node that never becomes ready.

`extraArgs` is the deliberate escape hatch: appended verbatim, last, and
completely unvalidated. It is **unsupported surface** — anything
reachable only through it may break without notice. Each element becomes
one shell word in the node's boot command, so quote values containing
whitespace yourself.

`Endpoint()` is a pure accessor and does **not** start the service.
Reachability from a consumer container comes from `BindServer`, which
lets `WithServiceBinding` start the service as the consumer's dependency
and wire its IP into `/etc/hosts`. (Pre-starting from this module would
register the service in the module's DNS domain, which a session-domain
consumer's host-file lookup can't resolve.) For module-runtime access,
use `Server.Client`, which starts the service itself.

### BundleServer

`valkey/valkey-bundle` is the upstream image carrying the module
ecosystem preinstalled — JSON (`JSON.SET` / `JSON.GET`), Bloom (`BF.*`),
and Search (`FT.*`). `BundleServer` boots a single node from it and
returns the same `*Server` with the same method set; only the image and
the readiness check differ.

```go
Valkey.BundleServer(
    ctx,
    name="",
    registry="docker.io", tag="9.1",   // <registry>/valkey/valkey-bundle:<tag>
    password *dagger.Secret,
    clientListenerSecurity *ServerSecurity,
) (*Server, error)
```

The module commands need no new API — `Client.Do(["JSON.SET", ...])`
already reaches them and their replies come back JSON-encoded like any
other. Typed sugar for JSON / Bloom / Search is deliberately out of
scope.

**Readiness asserts the modules loaded.** A bundle node is not ready
merely because it answers `PING`: after the first successful ping,
`Server.Client` runs `MODULE LIST` and fails the boot unless `json`,
`bf`, and `search` are all present, naming what is missing and what the
node did report. Those are Valkey's own module names, not the shared
object file names (`libvalkey_bloom.so` registers as `bf`, matching its
`BF.*` command prefix). The list is a floor, not an exact match: the
image also ships `ldap` and every node reports the built-in `lua`, and a
future bundle release adding a module must not fail the check.

**Why a separate constructor** rather than a `bundle bool` on `Server`:
the image choice stays legible at the call site, and readiness gets
somewhere to hang the module assertion.

**Why the modules would otherwise not load.** The bundle image ships its
own `bundle-docker-entrypoint.sh` instead of the stock
`docker-entrypoint.sh`; the modules are `.so` files under
`/usr/lib/valkey` and nothing loads them unless something composes the
`--loadmodule` flags, which is precisely what that script does. Boot the
bundle image through the stock entrypoint and you get a perfectly healthy
node with an empty module list, whose first `JSON.SET` fails as an
unknown command. The readiness assertion is what turns that into a boot
failure.

The `valkey-server` configuration passthrough is not wired up here yet —
it is a follow-up.

## Replication

Primary/replica topology: one primary plus `replicas` asynchronous read
replicas, all from the same image. A replica is asymmetric — it dials a
primary that is already up — so the whole topology is an ordinary service
binding, with none of the symmetric-peer startup problems Valkey Cluster
brings.

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

## Cluster

Slot-sharded Valkey Cluster: `shards` primaries splitting the 16384 hash
slots between them, each with `replicasPerShard` replicas, all from the
same image.

```go
Valkey.Cluster(
    ctx,
    name="",
    registry="docker.io", tag="9.1",
    shards=3,
    replicasPerShard=0,
    password *dagger.Secret,
    clientListenerSecurity *ServerSecurity,
) (*Cluster, error)

Cluster.Endpoints() []string                       // host:6379 per member, primaries first
Cluster.BindNodes(*dagger.Container) *dagger.Container
Cluster.Client(ctx, security *ClientSecurity) (*Client, error)
Cluster.Stop(ctx) error                            // tears down every member
```

**Symmetric peers, so no bindings between them.** Cluster members gossip
with each other over a bus port (client port + 10000) and none of them is
"already up" when its neighbours boot. A node-to-node
`WithServiceBinding` would therefore deadlock: binding A to B makes
Dagger fully ready B before it even boots A, while B's readiness depends
on the cluster that needs A. The nodes carry no bindings at all and are
started *concurrently* instead (`startAll`), discovering each other by
hostname over session-wide DNS — the same shape the Redpanda multi-broker
topology uses.

**Every node advertises its own pinned hostname.** Each member boots with
`--cluster-announce-ip <its WithHostname alias>`,
`--cluster-announce-port 6379`, and `--cluster-announce-bus-port 16379`.
A node that cannot self-identify falls back to announcing localhost, at
which point every peer dials itself and the cluster never forms. The
announce address is the hostname rather than an IP on purpose: the
container IP is unknown until the service starts and changes between
sessions, whereas the alias is stable and resolvable from every peer,
from the module runtime, and — via `BindNodes` — from a consumer
container.

**Bootstrap is a lazy container, not constructor work.** `Valkey.Cluster`
validates, builds the nodes, and composes (but does not run) a
`valkey-cli --cluster create --cluster-yes --cluster-replicas N` exec
that waits for every node to answer, assigns the slots, and then waits
for every node to report `cluster_state:ok` *and* the full
`cluster_known_nodes`. Whoever forces that exec — `Cluster.Client` from
the module runtime, or a consumer container's exec through `BindNodes` —
brings the node services up as dependencies of their own request. The
laziness is what makes `BindNodes` usable at all: a Dagger service is
only reachable from the client that started it, so bootstrapping eagerly
here would pin every node to the valkey module's DNS domain and a
consumer module binding them could not resolve them. `BindNodes` grafts
the bootstrap's completion marker into the consumer container, so the
container's first command meets a cluster whose slots are assigned rather
than one answering `CLUSTERDOWN`.

The flip side: `Cluster.Client` *does* start the services explicitly
(concurrently, from the module runtime) because valkey-go dials them from
there and needs the hostnames in session DNS. A cluster this module has
already dialled cannot then be bound by a consumer container — the
binding fails with `lookup valkey-<host> for hosts file: … no such host`
— so bind a fresh one. That cross-module resolution failure is a known
Dagger limitation and has been reported upstream; if it is fixed, the
lazy bootstrap can collapse back into the constructor.

**`shards < 3` is rejected.** Valkey Cluster agrees slot ownership by a
majority vote of the primaries, so two primaries can never form a quorum
once one is unreachable, and one primary is a standalone node wearing a
cluster hat. The other rejected inputs: `password == nil`,
`clientListenerSecurity == nil`, a non-plaintext profile, and
`replicasPerShard < 0`.

**Plaintext only, for now.** A TLS node runs with `--port 0`, so the
cluster bus would have to run over TLS too (`--tls-cluster yes`) and each
peer would need trust material a client-listener `*ServerSecurity` does
not carry — and the `valkey-cli` that bootstraps them would need its own
`--tls` / `--cacert` material on top of that.

**Hostnames.** Every member's hostname is `valkey-<sha12(...)>` derived
from `name` plus the node's index, so each member gets its own pinned
hostname, two clusters with different `name`s never collide, and a
member never collides with a `Valkey.Server` or `Valkey.Replication` node
booted under the same `name`.

`Endpoints()` is `+cache="session"` rather than `"never"` for the same
reason `Replication.Primary()` is: Dagger v0.21 detaches module objects
returned from a `+cache="never"` function when a consumer module reads
their fields lazily.

## Client

Pure-Go valkey-go based client. No container image. Works against the
local topology or any reachable remote Valkey.

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

**Cluster-aware `Keys` and `Del`.** A client from `Cluster.Client` seeds
from every member, lets valkey-go keep a slot map, and follows MOVED/ASK
redirects — but two methods need more than that. `Keys` scans *every*
node rather than the one it seeded from: SCAN names no key, so a cluster
client has no slot to route it by and it is answered by whichever node it
lands on, reporting that shard as if it were the whole keyspace.
(Replicas answer SCAN locally too, so the union is de-duplicated.) `Del`
groups its keys by hash slot and issues one pipelined DEL per group: a
single DEL naming two slots is refused outright with `CROSSSLOT`, because
the slots may live on different primaries. Against a standalone node both
methods behave exactly as before.

`ApplyFile` is the fixture-seeding path: one command per line in
valkey-cli syntax, run in order on a single connection. Blank lines and
`#` comments are skipped; arguments split on whitespace with single- and
double-quoted runs kept intact (`\` escapes inside double quotes). A
failing command aborts the run and reports its line number.

## Follow-ups

TLS/mTLS for the replication link (`--tls-replication yes` plus the trust
material a replica needs to verify and authenticate to its primary) and
for the cluster bus (`--tls-cluster yes`, plus `--tls`/`--cacert` for the
bootstrapping `valkey-cli`); configuration passthrough for `Replication`
and `Cluster` members and for `BundleServer`; keyspace export/import via
SCAN + DUMP/RESTORE.
