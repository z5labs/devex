# kafka

A Dagger module that spins up a Kafka-wire-compatible cluster from one
of four upstream images — `apache/kafka-native` (GraalVM),
`apache/kafka` (JVM), `confluentinc/cp-kafka` (Confluent Platform), or
`redpandadata/redpanda` (Redpanda, a from-scratch C++ implementation)
— and exposes a pure-Go franz-go client that targets either the local
cluster or any reachable remote cluster (AWS MSK, Confluent Cloud, etc.).

The first three share the `KAFKA_*` Scala-wrapper env-var contract and
return the same `*Cluster`. Redpanda's configuration layer is disjoint
(`rpk redpanda start`, a `redpanda.yaml` config, PEM keys instead of
PKCS#12), so `RedpandaCluster` returns its own type and accepts its own
security profile — see the dedicated section below.

The module supports plaintext, TLS, and mTLS on the external client-facing
listener. For the `*Cluster` constructors (Apache/Confluent) the internal
listeners (inter-broker and controller-quorum) are **always mTLS**, secured by
a per-cluster CA the module mints internally and never surfaces on the API.
Redpanda has no equivalent env-var PKI, so its internal RPC listener is
plaintext — see the "RedpandaCluster" section for the rationale.

## Security profiles

```go
// External listener: PLAINTEXT
serverSec := dag.Kafka().PlaintextServerSecurity()
clientSec := dag.Kafka().PlaintextClientSecurity()

// External listener: TLS
serverSec := dag.Kafka().TLSServerSecurity(caKeystore, caKeystorePwd)
clientSec := dag.Kafka().TLSClientSecurity(truststore, truststorePwd)

// External listener: mTLS
serverSec := dag.Kafka().MtlsServerSecurity(
    caKeystore, caKeystorePwd,         // signs per-broker server leaves
    clientTruststore, clientTruststorePwd, // CAs the broker accepts client certs from
)
clientSec := dag.Kafka().MtlsClientSecurity(
    clientKeystore, clientKeystorePwd, // client's own leaf cert + key
    truststore, truststorePwd,         // CA that signs the broker leaves
)
```

The CA `keystore`/`truststore` arguments are PKCS#12 archives produced by
the [`certificate-management`](../certificate-management) module (or any
PKCS#12 source). The cluster mints per-broker leaf certificates from the
supplied CA at `ApacheNativeCluster()` time, with each broker's stable hostname
(`broker-100-<suffix>`, `broker-101-<suffix>`, ...) bound as a DNS SAN.
Clients dialing the bootstrap address verify the broker's cert against
the matching truststore.

## ApacheNativeCluster

```go
cluster := dag.Kafka().ApacheNativeCluster(
    clusterId,        // 22-char base64-url Kafka cluster ID
    serverSec,
    dagger.KafkaApacheNativeClusterOpts{
        Tag:         "4.2.0",
        Controllers: 3,    // odd voter count: 1, 3, 5, ... (see below)
        Brokers:     2,
        Registry:    "docker.io",
    },
)
```

`ApacheNativeCluster` is a session-cached lazy chain — the server-side constructor
runs at most once per (clusterId, args) within an engine session, so
chained method calls (`cluster.Client().Produce(...) → Consume(...)`) all
observe the same underlying broker services and the same internal CA.

- `Cluster.BootstrapServers(ctx) ([]string, error)` — broker `host:port`
  pairs the client (and `BindBrokers` consumers) connect to. Returns
  `["broker-100-<suffix>:9092", "broker-101-<suffix>:9092", ...]` where
  `<suffix>` is a short DNS-safe hash derived from `clusterId` (see
  "Listener layout" below).
- `Cluster.BindBrokers(c *dagger.Container) *dagger.Container` — chains
  `WithServiceBinding` for every broker so a caller's container resolves
  the same hostnames `BootstrapServers` reports.
- `Cluster.Client(security *KafkaClientSecurity) *KafkaClient` — starts
  every broker service and returns a franz-go-backed Client wired to dial
  them.

### Listener layout

| Listener   | Port  | Role          | Security                              |
|------------|-------|---------------|---------------------------------------|
| EXTERNAL   | 9092  | client-facing | PLAINTEXT, SSL (TLS), or SSL (mTLS)   |
| INTERNAL   | 19092 | inter-broker  | always SSL with mTLS (internal CA)    |
| CONTROLLER | 9093  | KRaft quorum  | always SSL with mTLS (internal CA)    |

Stable hostnames (`controller-1-<suffix>`, `controller-2-<suffix>`, ...,
`broker-100-<suffix>`, ...) are assigned via `Service.WithHostname` — the
suffix is a short hash derived from `clusterId` so parallel cluster spawns
within one engine session don't collide on alias names.

### Controller quorum (HA)

`controllers` sets the size of the KRaft controller quorum: `1` is a
single-node quorum, `3` or `5` a highly-available one that tolerates
`floor((N-1)/2)` controller failures. Each controller runs in its own
container with a unique `KAFKA_NODE_ID` (`1..N`).

The count **must be odd** (`1, 3, 5, ...`). An even count is rejected with
an error: it buys no extra fault tolerance over the next-lower odd count
while enlarging the majority every commit must reach.

Multi-controller HA works without any controller-to-controller
`WithServiceBinding` — which would be an unresolvable build cycle, since
each controller would have to reference every other as a service before any
of them exists. Instead, controller hostnames are deterministic
(`controller-<n>-<suffix>`, derived from `clusterId`), so the full
quorum-voters string
(`1@controller-1-<suffix>:9093,2@controller-2-<suffix>:9093,...`) is
computed **before** any container is built and pinned identically onto
every controller and broker. At runtime each controller resolves its peers
by hostname over the engine's session-wide DNS (populated by
`WithHostname`), and the quorum forms over the always-mTLS CONTROLLER
listeners — the internal CA mints a verifying leaf for every controller
host's SAN.

## ConfluentCluster

```go
cluster := dag.Kafka().ConfluentCluster(
    clusterId,
    serverSec,
    dagger.KafkaConfluentClusterOpts{
        Tag:         "8.2.0",
        Controllers: 1,
        Brokers:     2,
        Registry:    "docker.io",
    },
)
```

`ConfluentCluster` uses the `confluentinc/cp-kafka` image and is
otherwise API-identical to the Apache constructors — same topology,
same `ServerSecurity` / `*Cluster` types, same listener layout. Confluent
Platform 8.x bundles Apache Kafka 4.x with the same minor version (CP
8.2.0 ships Kafka 4.2.0), so callers swap distros by changing the
constructor name alone.

The constructor silently sets `KAFKA_CONFLUENT_SUPPORT_METRICS_ENABLE=false`
on each broker to disable Confluent's phone-home telemetry, matching
the Apache variants' startup behavior.

## Schema Registry

A schema registry sits alongside the brokers as a separate service: it
stores schemas in the cluster's `_schemas` Kafka topic and exposes a REST
API for registering and looking up Avro / JSON Schema / Protobuf schemas by
subject. `ConfluentSchemaRegistry` runs the `confluentinc/cp-schema-registry`
image.

```go
cluster := dag.Kafka().ConfluentCluster(clusterId, dag.Kafka().PlaintextServerSecurity())

sr := dag.Kafka().ConfluentSchemaRegistry(
    cluster,
    dag.Kafka().PlaintextSchemaRegistrySecurity(),
    dagger.KafkaConfluentSchemaRegistryOpts{
        Tag:      "8.2.0",
        Registry: "docker.io",
    },
)
client := sr.Client(dag.Kafka().PlaintextSchemaRegistryClientSecurity())
```

The registry composes on top of **any** `*Cluster` — it talks the Kafka
wire protocol to the brokers for its `_schemas` topic and exposes its own
REST API on top, so the cluster distro is orthogonal. `cp-schema-registry`
simply pairs most naturally with a `cp-kafka` `ConfluentCluster`.

`ConfluentSchemaRegistry` is session-cached, so chained client calls all
observe the same underlying service. It binds every broker via
`cluster.BindBrokers` and wires `SCHEMA_REGISTRY_KAFKASTORE_BOOTSTRAP_SERVERS`
from `cluster.BootstrapServers()`.

**TLS / mTLS** — the required `security *SchemaRegistrySecurity` argument (from
`Kafka.{Plaintext,TLS,Mtls}SchemaRegistrySecurity`) must **match the cluster's
mode**: a PLAINTEXT registry pairs with a PLAINTEXT cluster (today's behaviour),
and a TLS/mTLS registry pairs with a same-mode cluster so its kafka-storage
connection authenticates against the broker. A mismatch returns an error naming
both modes. Under TLS/mTLS the constructor mints a per-registry REST server leaf
(DNS SAN = the registry's service hostname) from the supplied CA and derives the
broker-facing truststore from the same CA — so pass the **same CA** to the
cluster, the registry, and the client (the single-CA convention, see the
[TLS / mTLS notes](#tls--mtls-notes)). See `SchemaRegistryClient` for the
matching client profile.

- `SchemaRegistry.Endpoint(ctx) (string, error)` — the `host:port` other
  containers (and the module runtime) reach the REST API on.
- `SchemaRegistry.BindTo(c *dagger.Container) *dagger.Container` —
  `WithServiceBinding` so a caller's container resolves the registry at the
  same hostname `Endpoint` reports.
- `SchemaRegistry.Client(security *SchemaRegistryClientSecurity) *SchemaRegistryClient`
  — a pure-Go `net/http` admin client (no helper containers); the client
  profile must match the registry's mode (HTTP vs HTTPS, plus a client cert for
  mTLS).
- `SchemaRegistry.Stop(ctx) error` — tears the registry service down.

### ApicurioSchemaRegistry

`ApicurioSchemaRegistry` is a sibling constructor backed by the
`apicurio/apicurio-registry-kafkasql` image — Apicurio Registry's
Kafka-storage build. It takes the **same parameters** as
`ConfluentSchemaRegistry` (`cluster`, plus optional `registry` /  `tag`) and
returns the **same `*SchemaRegistry` type**, so `Client()`, `Endpoint()`,
`BindTo()`, and `Stop()` are all shared code.

```go
cluster := dag.Kafka().ConfluentCluster(clusterId, dag.Kafka().PlaintextServerSecurity())

sr := dag.Kafka().ApicurioSchemaRegistry(
    cluster,
    dagger.KafkaApicurioSchemaRegistryOpts{
        Tag:      "2.6.13.Final",
        Registry: "docker.io",
    },
)
client := sr.Client()
```

Apicurio is a more permissively licensed alternative to `cp-schema-registry`.
It stores schemas in its own Kafka topic (`KAFKA_BOOTSTRAP_SERVERS` is wired
from `cluster.BootstrapServers()`) and exposes a **Confluent-Schema-Registry-
compatible** REST API under `/apis/ccompat/v7`, so the same
`SchemaRegistryClient` drives it unchanged.

**CSR-compat caveat.** Apicurio's native catalogue spans Avro, JSON Schema,
Protobuf, OpenAPI, AsyncAPI, GraphQL, WSDL, and XSD, but the
Confluent-compatible surface only speaks `AVRO`, `JSON`, and `PROTOBUF` (the
`schemaType` values `SchemaRegistryClient` already accepts). Apicurio's other
artifact types are not reachable through this constructor.

**TLS / mTLS**, matching `ConfluentSchemaRegistry`: pass a
`security *SchemaRegistrySecurity` that matches the cluster's mode. Apicurio is
a Quarkus app, so TLS terminates on the Quarkus HTTP layer
(`QUARKUS_HTTP_SSL_*`, PKCS#12 keystore) while its kafkasql storage connection
uses the Kafka client SSL config (`KAFKA_SECURITY_PROTOCOL=SSL` + PKCS#12
truststore, fed PKCS#12 to avoid a PEM-truststore-plus-password startup bug).

### KarapaceSchemaRegistry

`KarapaceSchemaRegistry` is a sibling constructor backed by the
`ghcr.io/aiven-open/karapace` image. Karapace is Aiven's drop-in Python
reimplementation of the Confluent Schema Registry — identical wire surface,
different runtime. It takes the **same parameters** as `ConfluentSchemaRegistry`
(`cluster`, the required `security *SchemaRegistrySecurity`, plus optional
`registry` / `tag`) and returns the **same `*SchemaRegistry` type**, so
`Client()`, `Endpoint()`, `BindTo()`, and `Stop()` are all shared code.

```go
cluster := dag.Kafka().ConfluentCluster(clusterId, dag.Kafka().PlaintextServerSecurity())

sr := dag.Kafka().KarapaceSchemaRegistry(
    cluster,
    dag.Kafka().PlaintextSchemaRegistrySecurity(),
    dagger.KafkaKarapaceSchemaRegistryOpts{
        Tag:      "6.1.4",
        Registry: "ghcr.io",
    },
)
client := sr.Client(dag.Kafka().PlaintextSchemaRegistryClientSecurity())
```

Unlike `ConfluentSchemaRegistry` and `ApicurioSchemaRegistry`, `registry`
**defaults to `ghcr.io`** — Karapace publishes to GitHub Container Registry, not
Docker Hub. This also keeps CI clear of Docker Hub rate limits and Confluent's
image licensing. Karapace stores schemas in the cluster's `_schemas` topic
(`KARAPACE_BOOTSTRAP_URI` is wired from `cluster.BootstrapServers()`) and serves
its Confluent-compatible REST API **at the root**, so the same
`SchemaRegistryClient` drives it unchanged.

**TLS / mTLS**, matching `ConfluentSchemaRegistry`: pass a matching-mode
`security *SchemaRegistrySecurity`. Karapace is a Python service that consumes
**PEM** (not PKCS#12) for both its REST listener (`KARAPACE_SERVER_TLS_*`) and
its aiokafka storage connection (`KARAPACE_SECURITY_PROTOCOL=SSL` +
`KARAPACE_SSL_CAFILE`); the module extracts PEM from the supplied CA
internally, so callers still pass the same PKCS#12 profile shape as the other
registries.

### SchemaRegistryClient

```go
id, err := client.RegisterSchema(ctx, "my-subject-value", avroSchema,
    dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{SchemaType: "AVRO"})

schema, err := client.LookupSchemaByID(ctx, id)        // RegisteredSchema
latest, err := client.LookupLatestBySubject(ctx, "my-subject-value")
subjects, err := client.ListSubjects(ctx)              // []string
deleted, err := client.DeleteSubject(ctx, "my-subject-value") // []int versions

err = client.SetCompatibility(ctx, "my-subject-value", "BACKWARD")
level, err := client.GetCompatibility(ctx, "my-subject-value")
```

`schemaType` accepts `AVRO`, `JSON`, or `PROTOBUF`. Compatibility `level`
accepts `NONE`, `BACKWARD`, `BACKWARD_TRANSITIVE`, `FORWARD`,
`FORWARD_TRANSITIVE`, `FULL`, or `FULL_TRANSITIVE`.

`sr.Client(security)` picks its URL scheme from the registry's mode: a TLS/mTLS
registry serves HTTPS and needs a matching `SchemaRegistryClientSecurity` (a
truststore for the registry's cert, plus a client keystore for mTLS); the
underlying `*http.Client` materialises a `tls.Config` from those PKCS#12
archives the same way the franz-go `Client` does. A scheme/mode mismatch
(HTTPS registry with a PLAINTEXT client, or vice versa) fails with a clear
error on the first request.

`RegisteredSchema` carries `Subject`, `Version`, `SchemaID`, `Definition`
(the schema text), and `SchemaType`. The `SchemaID` / `Definition` names
diverge from the REST API's `id` / `schema` JSON keys: a Dagger object
already has a synthetic `id` field and `schema` is a GraphQL keyword, so
both would break consumer-module codegen.

## RedpandaCluster

```go
serverSec := dag.Kafka().RedpandaPlaintextServerSecurity()
// or, for TLS termination on the external Kafka listener:
serverSec = dag.Kafka().RedpandaTLSServerSecurity(caKeystore, caKeystorePwd)

cluster := dag.Kafka().RedpandaCluster(
    clusterId,
    serverSec,
    dagger.KafkaRedpandaClusterOpts{
        Tag:      "v26.1.7",
        Registry: "docker.io",
        Brokers:  3, // omit (or 1) for a single node
    },
)
client := cluster.Client(dag.Kafka().PlaintextClientSecurity()) // or TLSClientSecurity(...)
```

`RedpandaCluster` runs a genuine multi-broker cluster: `brokers` nodes
(node IDs `0..N-1`, hostnames `redpanda-<n>-<suffix>`) form a single
Raft group over the internal RPC listener (`:33145`). Redpanda runs
broker and Raft roles in the same process, so there is no separate
controller container — every node is a full broker that also
participates in Raft. Each broker is started via `rpk redpanda start
--mode dev-container` (which bundles `--overprovisioned`,
`--reserve-memory 0M`, `--check=false`, and `--unsafe-bypass-fsync`).

Node hostnames are deterministic (derived from `clusterId`), so the seed
list is computed before any container is built and pinned onto every
node via `WithHostname` + session-wide DNS. A multi-node cluster uses
Redpanda's **seed-driven bootstrap**: every node shares the identical
`seed_servers` list (all N nodes) with `empty_seed_starts_cluster=false`,
so the nodes deterministically form one Raft group with **no**
node-to-node `WithServiceBinding`. The nodes are symmetric Raft peers, so
a binding can't be used to bring them up (binding A→B forces Dagger to
fully ready B before it boots A, but B can't ready its ports until the
cluster forms, which needs A — a deadlock); instead `Client` /
`SchemaRegistry` start every node **concurrently** so they boot together
and discover each other by hostname over session-wide DNS. A single-broker
cluster keeps the legacy `empty_seed_starts_cluster=true` self-bootstrap.

### Controllers policy

`controllers` **must be 1** — Redpanda has no separate controller role
(broker and Raft duties share one process), so a controller count is not
a meaningful concept. Any other value is rejected at construction time
with a Redpanda-specific error; size the cluster with `brokers` instead.
`brokers < 1` is likewise rejected.

### Inter-node RPC security

The internal RPC listener (`:33145`) that carries Raft traffic between
nodes is **PLAINTEXT and unauthenticated even on a TLS cluster**.
Redpanda's RPC-listener TLS would need its own internal CA + per-node
leaves + mutual trust — a whole parallel PKI — for traffic that never
leaves the Dagger engine's isolated per-session network. This
deliberately differs from the Apache path (which always mTLS-encrypts its
internal + controller listeners) because Redpanda has no equivalent
`KAFKA_*` PKCS#12 env-var contract to reuse. TLS here is scoped to the
client-facing Kafka listener + bundled Schema Registry REST endpoint,
which is what external clients actually verify; every node's external
leaf is SAN'd to its own hostname so a client routed to any broker
verifies successfully.

- `RedpandaCluster.BootstrapServers(ctx) ([]string, error)` — one
  `host:9092` per broker.
- `RedpandaCluster.BindBrokers(c *dagger.Container) *dagger.Container`
  — binds every broker, same shape as `Cluster.BindBrokers`.
- `RedpandaCluster.Client(ctx, security *KafkaClientSecurity)
  (*KafkaClient, error)` — starts every broker (bringing the Raft group
  online) and returns the same `*Client` the Apache constructors return.
  The Kafka wire protocol matches, so producer/consumer code is shared.
- `RedpandaCluster.SchemaRegistry(ctx, security) (*SchemaRegistry, error)`
  — the bundled in-broker Schema Registry (on node 0) as the shared
  `*SchemaRegistry` type — see "Bundled Schema Registry" below.
- `RedpandaCluster.Stop(ctx) error` — tears down every node service.

### Security profile

`*RedpandaServerSecurity` is a separate type from `*ServerSecurity` so
the Go compiler stops a caller from accidentally handing an Apache
profile (e.g. `MtlsServerSecurity`, not supported here yet) to
`RedpandaCluster`. Only `Plaintext` and `TLS` constructors exist in
this story; mTLS lands in a follow-up.

### Cert format: PEM, not PKCS#12

Redpanda reads PEM (`server.crt`, `server.key`) from its
`redpanda.yaml` rather than the PKCS#12 keystores the Apache
constructors hand to the JVM. The API surface still accepts the same
PKCS#12 CA you'd hand to `TLSServerSecurity` — the constructor loads
the CA via `certificate-management`'s existing PKCS#12 loader, issues
**one server leaf per node** (each SAN'd to that node's own hostname),
then mounts the PEM cert/key files into each broker container at
`/etc/redpanda/certs/server.{crt,key}` and points the per-node rendered
`redpanda.yaml` at those paths (the YAML never embeds PEM material
itself). Callers don't have to convert between formats. Each node's
private key crosses into its container as a `*dagger.Secret` (mounted
via `WithMountedSecret`) so its plaintext never lands in the module
workdir. The CA cert *is* mounted (shared across nodes) at
`/etc/redpanda/certs/ca.crt` — it is the truststore for each node's
bundled Schema Registry / pandaproxy broker client, which must dial the
TLS-only Kafka listener to persist `_schemas` (see "Bundled Schema
Registry" below). mTLS on the external listener (client-auth) is a
follow-up; this path is server-side TLS only.

### Client

The `*KafkaClient` returned by `RedpandaCluster.Client()` is the same
type the Apache constructors return — bring whichever
`*ClientSecurity` you already use. PKCS#12 truststores work as-is.

### Bundled Schema Registry

`rpk redpanda start` already runs a Schema Registry inside the broker
process on `:8081` — there is no separate container. `cluster.SchemaRegistry()`
surfaces it as the **same** `*SchemaRegistry` type
`Kafka.ConfluentSchemaRegistry` returns, so the bundled and
separate-container registries are interchangeable from a caller's
perspective:

```go
sr := cluster.SchemaRegistry(dag.Kafka().PlaintextSchemaRegistrySecurity())
client := sr.Client(dag.Kafka().PlaintextSchemaRegistryClientSecurity())

id, err := client.RegisterSchema(ctx, "my-subject-value", avroSchema,
    dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{SchemaType: "AVRO"})

// LookupSchemaByID returns a lazy RegisteredSchema object; its fields
// resolve with context.
schema := client.LookupSchemaByID(id)
subject, err := schema.Subject(ctx)
```

`sr.Stop()` is a safe no-op on the bundled registry — its service is the
broker itself, so `cluster.Stop()` owns teardown. Calling `Stop()` uniformly
over the shared `*SchemaRegistry` type therefore never kills the cluster.

Unlike `ConfluentSchemaRegistry` — which boots a separate
`confluentinc/cp-schema-registry` service alongside the brokers —
Redpanda's registry needs no extra image: the constructor exposes port
8081 on the broker container and passes `--schema-registry-addr` (TLS
mode renders the equivalent `schema_registry` directive in
`redpanda.yaml`). It speaks the Confluent Schema Registry REST API, so
the whole `*SchemaRegistry` surface behaves identically to the
`ConfluentSchemaRegistry` variant: `Endpoint()` and `BindTo()` on the
`*SchemaRegistry`, and the `*SchemaRegistryClient` returned by
`Client()`.

`SchemaRegistry(security)` takes a `security *SchemaRegistrySecurity` matching
the cluster's mode (Redpanda supports PLAINTEXT or TLS — it has no cluster-level
mTLS). On a TLS cluster the bundled SR REST endpoint terminates **HTTPS**,
reusing the broker's own server leaf (whose DNS SAN is the broker/registry host)
via the `schema_registry_api_tls` block rendered into `redpanda.yaml` at
cluster-build time — so no per-registry leaf is minted and the profile's CA
keystore is unused (required only for API uniformity). Pass a matching TLS
`SchemaRegistryClientSecurity` (a truststore from the cluster CA) to `Client()`
to drive the HTTPS endpoint. SR-REST mTLS is not reachable, as it would require
a cluster-level mTLS mode Redpanda does not have.

## Client

`dag.Kafka().Client(bootstrapServers, security)` constructs a franz-go-backed
Client without any I/O. `cluster.Client(security)` does the same but also
guarantees the local cluster is started.

```go
client := cluster.Client(clientSec)

// admin
err := client.CreateTopic(ctx, "my-topic", dagger.KafkaClientCreateTopicOpts{
    Partitions: 1, ReplicationFactor: 2,
})
topics, err := client.ListTopics(ctx)
err = client.DeleteTopic(ctx, "my-topic")

// data
err := client.Produce(ctx, "my-topic", "k", "v", dagger.KafkaClientProduceOpts{
    KeyEncoding: "raw", ValueEncoding: "raw",
})
records, err := client.Consume(ctx, "my-topic", dagger.KafkaClientConsumeOpts{
    MaxMessages: 10, Timeout: "10s",
    KeyEncoding: "raw", ValueEncoding: "raw",
    Group: "", // group-less direct consume; pass "my-group" to consume as a group member
})

// Schema-Registry-framed data: register a schema via the Confluent /
// Apicurio / Karapace SR client to get an id, then produce / consume
// with the Confluent wire format (0x00 || uint32be(id) || payload).
id, err := srClient.RegisterSchema(ctx, "my-topic-value", schema, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
    SchemaType: "AVRO",
})
err = client.Produce(ctx, "my-topic", "k", "v", dagger.KafkaClientProduceOpts{
    KeyEncoding: "raw", ValueEncoding: "raw",
    ValueSchemaID: id, // optional KeySchemaID does the same for the key
})
framed, err := client.Consume(ctx, "my-topic", dagger.KafkaClientConsumeOpts{
    MaxMessages: 1, Timeout: "10s",
    KeyEncoding: "raw", ValueEncoding: "raw",
    SchemaRegistryAware: true,
})
gotID, err := framed[0].ValueSchemaID(ctx)   // == id (the registered schema id)
payload, err := framed[0].Value(ctx)         // original bytes, 5-byte header stripped
// Records without 0x00 framing surface ValueSchemaID(ctx) == 0 and
// Value(ctx) passes through unchanged.

// JSON wire-format enforcement: parse-and-canonicalise on Produce,
// json.Valid-check on Consume. Composes with the framing opts above —
// a single Produce call can canonicalise then frame, a single Consume
// call can unframe then validate.
err = client.Produce(ctx, "my-topic", "k", `{"x":"hello"}`, dagger.KafkaClientProduceOpts{
    KeyEncoding: "raw", ValueEncoding: "raw",
    ValueSchemaID:    id,
    ValueSerializeAs: "JSON", // "" pass-through; "JSON" parses + re-marshals canonically
})
validated, err := client.Consume(ctx, "my-topic", dagger.KafkaClientConsumeOpts{
    MaxMessages: 1, Timeout: "10s",
    KeyEncoding: "raw", ValueEncoding: "raw",
    SchemaRegistryAware: true,
    ValueDeserializeAs:  "JSON", // "" pass-through; "JSON" rejects non-parseable payloads
})

// Avro wire format: the JSON input is Avro-binary-encoded against the
// registered schema and framed on Produce, and framed Avro-binary bytes
// are decoded back to JSON on Consume. Both sides take a SchemaRegistry so
// the schema text is resolved by id; Produce requires a positive
// ValueSchemaID (it both selects the schema and frames the record).
id, err = srClient.RegisterSchema(ctx, "my-topic-value", avroSchema, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
    SchemaType: "AVRO",
})
err = client.Produce(ctx, "my-topic", "k", `{"x":"hello"}`, dagger.KafkaClientProduceOpts{
    KeyEncoding: "raw", ValueEncoding: "raw",
    ValueSchemaID:    id,
    ValueSerializeAs: "AVRO", // JSON -> Avro binary, then frame with id
    Registry:         sr,     // *SchemaRegistry: resolves schema text by id
    // RegistrySecurity: for a TLS/mTLS registry pass the matching client
    // profile (Kafka.{TLS,Mtls}SchemaRegistryClientSecurity); omit / nil
    // resolves over plaintext HTTP.
    RegistrySecurity: srClientSec,
})
decoded, err := client.Consume(ctx, "my-topic", dagger.KafkaClientConsumeOpts{
    MaxMessages: 1, Timeout: "10s",
    KeyEncoding: "raw", ValueEncoding: "raw",
    SchemaRegistryAware: true,        // required: supplies the wire id
    ValueDeserializeAs:  "AVRO",      // Avro binary -> JSON
    Registry:            sr,
    RegistrySecurity:    srClientSec, // same client profile as Produce
})

// java client.properties (+ p12 sidecars in TLS / mTLS modes) for the
// Apache Kafka CLI tools — export the parent directory so the relative
// truststore.p12 / keystore.p12 references resolve.
props := client.PropertiesFile() // *dagger.File — resolve via .Contents(ctx) / .Export(ctx)
```

`keyEncoding` / `valueEncoding` accept `"raw"` (literal UTF-8 bytes),
`"hex"`, or `"base64"` (standard padding). Anything else is rejected.

The serde opts (`keySerializeAs` / `valueSerializeAs` on `Produce`,
`keyDeserializeAs` / `valueDeserializeAs` on `Consume`) accept `""`
(pass-through, the default), `"JSON"`, or `"AVRO"`.

`"JSON"` is wire-format enforcement only — it does not fetch or apply a
registered schema. The producer side canonicalises (parse + re-marshal
via `encoding/json`) so what hits the wire is independent of caller-side
whitespace; the consumer side validates with `json.Valid` after any frame
strip.

`"AVRO"` is schema-bound. The caller's string is treated as a JSON
document and **Avro-binary-encoded** against the registered schema on
`Produce`, then framed; on `Consume` the framed Avro-binary payload is
decoded and re-serialised back to JSON. Both directions resolve the
schema text by id through the supplied `Registry` (`*SchemaRegistry`),
caching per id for the duration of the call. Against a **TLS / mTLS**
registry, also pass the matching `RegistrySecurity`
(`*SchemaRegistryClientSecurity` from
`Kafka.{TLS,Mtls}SchemaRegistryClientSecurity`) so the resolution client
speaks HTTPS (and, for mTLS, presents its client leaf); a nil / omitted
profile resolves over plaintext HTTP. `Produce` requires a
positive `…SchemaID` (it both names the schema and frames the record) and
errors before any I/O on a zero id; `Consume` requires
`schemaRegistryAware=true` and errors on an unframed record. The JSON
shape follows the Avro spec's JSON encoding (unions as
`{"<type>": value}`, bare `null` for the null branch, `bytes` as a
one-char-per-byte string); logical types, `decimal`, and `fixed` are not
yet supported.

Composition order is decode → serialize → frame on `Produce`,
unframe → deserialize → encode on `Consume`.

Topic auto-creation is disabled on the broker — call `CreateTopic` before
`Produce` / `Consume`.

### Introspection

Three live-cluster introspection primitives return their richer payloads as a
`*dagger.File` of JSON (a core type, so it crosses the module boundary; export
it and unmarshal the bytes). All three carry `+cache="never"` so they always
reflect current cluster state.

```go
// Per-topic metadata: partition layout (leader / replicas / ISR), the derived
// partition count and replication factor, and the full topic-level config set
// (retention.ms, cleanup.policy, ...), configs sorted by key.
topicJSON, err := client.DescribeTopic(ctx, "my-topic").Contents(ctx)

// Consumer group names (sorted). Empty on a fresh cluster; a group appears
// once a consumer has joined and persists while it retains committed offsets.
groups, err := client.ListConsumerGroups(ctx)

// Per-group detail: coordinator / state / protocol, members with their
// per-topic partition assignments, and per-partition committed-offset lag
// (with the total).
groupJSON, err := client.DescribeConsumerGroup(ctx, "my-group").Contents(ctx)
```

`DescribeConsumerGroup` only reports lag for partitions the group has
**committed** offsets for. `Consume` does not commit by default (it stays
idempotent under `+cache="never"`); pass `CommitOffsets: true` alongside a
`Group` to commit exactly the records returned, so the group persists in the
`Empty` state with reportable lag afterwards:

```go
_, err := client.Consume(ctx, "my-topic", dagger.KafkaClientConsumeOpts{
    MaxMessages: 3, Timeout: "10s",
    KeyEncoding: "raw", ValueEncoding: "raw",
    Group: "my-group", CommitOffsets: true,
})
```

## TLS / mTLS notes

- The internal CA is fresh per `ApacheNativeCluster()` invocation. Cert material
  never crosses the module boundary.
- Caller-supplied CAs are loaded via
  `dag.CertificateManagement().LoadCertificateAuthority(file, secret)`
  inside the kafka module — `*dagger.File` + `*dagger.Secret` are the
  only types crossing the API boundary.
- The mTLS API splits server-side trust deliberately: the CA used to
  sign broker server certs is independent from the CA used to validate
  incoming client certs. Pass the same CA to both args for symmetric
  setups, or split for asymmetric trust.
- `PropertiesFile()` writes plaintext passwords into the rendered
  Java `client.properties` — the Apache Kafka CLI tools require
  plaintext password values. The context is provided when you resolve
  the returned `*dagger.File` (`.Contents(ctx)` / `.Export(ctx)`).
  Export the resulting directory with restrictive permissions if you
  persist it.

### Schema Registry TLS / mTLS

- A TLS/mTLS Schema Registry secures **two** surfaces: its REST endpoint
  (registry as server) and its kafka-storage connection to the broker (registry
  as client). The constructor mints a **per-registry REST server leaf** from the
  supplied CA — DNS SAN = the registry's service hostname (`csr-`/`asr-`/`ksr-`
  + a per-cluster suffix) — exactly as the brokers mint their external leaves
  (`mintServiceLeaf`, shared with the broker path). Redpanda's bundled registry
  is the exception: it reuses the broker's own leaf, so no per-registry leaf is
  minted.
- **Security-mode coupling.** The registry's `SchemaRegistrySecurity` mode must
  match the backing cluster's client-listener mode; a mismatch is rejected with
  an error naming both modes. This guarantees the registry's kafka-storage
  connection uses the same protocol the broker's client listener speaks.
- **Single-CA convention.** Because the cluster does not carry its CA across the
  API, the registry derives its broker-facing truststore from the CA the caller
  re-supplies. Pass the **same CA** to the cluster (`TLSServerSecurity`), the
  registry (`TLSSchemaRegistrySecurity`), and the client
  (`TLSSchemaRegistryClientSecurity`) so every handshake chains to one root. For
  mTLS the registry also mints its own client leaf from that CA to present to
  the broker.
- **PKCS#12 vs PEM per image.** Confluent (`cp-schema-registry`, Java) and
  Apicurio (Quarkus) consume **PKCS#12** keystores/truststores via env vars;
  Karapace (Python) and Redpanda consume **PEM** — the module extracts PEM from
  the supplied PKCS#12 CA internally, so callers always pass the same profile
  shape. Feed Apicurio's kafkasql storage PKCS#12 (not PEM) to avoid a
  PEM-truststore-plus-password startup bug.

## Image source

The module exposes four cluster constructors. The first three speak
the same `KAFKA_*` env-var contract and return the same `*Cluster`;
`RedpandaCluster` returns its own `*RedpandaCluster` type — with
`BootstrapServers`, `BindBrokers`, and `Client` mirroring `*Cluster`,
plus its own `SchemaRegistry` — but a different config layer
underneath:

- `ApacheNativeCluster` → `<registry>/apache/kafka-native:<tag>`
  (default `docker.io/apache/kafka-native:4.2.0`). GraalVM-compiled;
  fastest cold start.
- `ApacheCluster` → `<registry>/apache/kafka:<tag>` (default
  `docker.io/apache/kafka:4.2.0`). Stock JVM image; slower cold start
  but does not share the native image's AOT-compiled `Pwd.getpwuid`
  substitution (`SystemPropertiesSupport.userHomeValue`) that has been
  observed to segfault during broker startup under Dagger Cloud trace
  `377f2e176c4f0e9844cb7f958c1e911b`. Prefer this constructor when
  startup robustness matters more than cold-start latency.
- `ConfluentCluster` → `<registry>/confluentinc/cp-kafka:<tag>` (default
  `docker.io/confluentinc/cp-kafka:8.2.0`). Confluent Platform 8.x
  bundles Apache Kafka 4.x at the same minor version, so CP 8.2.0
  ships Kafka 4.2.0. The constructor silently disables Confluent's
  phone-home telemetry (`KAFKA_CONFLUENT_SUPPORT_METRICS_ENABLE=false`).
- `RedpandaCluster` → `<registry>/redpandadata/redpanda:<tag>` (default
  `docker.io/redpandadata/redpanda:v26.1.7`). Multi-broker (Raft) via
  the `brokers` parameter; separate `*RedpandaCluster` /
  `*RedpandaServerSecurity` types because the config layer doesn't share
  the `KAFKA_*` env-var contract — see the "RedpandaCluster" section.

`registry` (default `docker.io`) and `tag` are the only caller-
overridable parts; the `apache/kafka{-native,}`, `confluentinc/cp-kafka`,
or `redpandadata/redpanda` portion is fixed per constructor.
