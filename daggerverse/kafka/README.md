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
listener. The internal listeners (inter-broker and controller-quorum) are
**always mTLS**, secured by a per-cluster CA the module mints internally and
never surfaces on the API.

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
        Controllers: 1,    // multi-controller is rejected; see below
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

Stable hostnames (`controller-1-<suffix>`, `broker-100-<suffix>`, ...) are
assigned via `Service.WithHostname` — the suffix is a short hash derived
from `clusterId` so parallel cluster spawns within one engine session
don't collide on alias names.

### Topology limit

`controllers > 1` is **rejected**: a true HA quorum needs every
controller to know every other controller at static config time, which
Dagger's `WithServiceBinding` model can't express without an unresolvable
cycle. Multi-controller HA lands in a follow-up.

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
    dagger.KafkaConfluentSchemaRegistryOpts{
        Tag:      "8.2.0",
        Registry: "docker.io",
    },
)
client := sr.Client()
```

The registry composes on top of **any** `*Cluster` — it talks the Kafka
wire protocol to the brokers for its `_schemas` topic and exposes its own
REST API on top, so the cluster distro is orthogonal. `cp-schema-registry`
simply pairs most naturally with a `cp-kafka` `ConfluentCluster`.

`ConfluentSchemaRegistry` is session-cached, so chained client calls all
observe the same underlying service. It binds every broker via
`cluster.BindBrokers` and wires `SCHEMA_REGISTRY_KAFKASTORE_BOOTSTRAP_SERVERS`
from `cluster.BootstrapServers()`.

**PLAINTEXT only** in this story — the constructor rejects a cluster whose
client listener runs TLS or mTLS. TLS / mTLS Schema Registry is a follow-up.

- `SchemaRegistry.Endpoint(ctx) (string, error)` — the `host:port` other
  containers (and the module runtime) reach the REST API on.
- `SchemaRegistry.BindTo(c *dagger.Container) *dagger.Container` —
  `WithServiceBinding` so a caller's container resolves the registry at the
  same hostname `Endpoint` reports.
- `SchemaRegistry.Client() *SchemaRegistryClient` — a pure-Go `net/http`
  admin client; no helper containers.
- `SchemaRegistry.Stop(ctx) error` — tears the registry service down.

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
    },
)
client := cluster.Client(dag.Kafka().PlaintextClientSecurity()) // or TLSClientSecurity(...)
```

`RedpandaCluster` is single-node only — `controllers != 1` or `brokers
!= 1` are rejected. Redpanda runs broker and Raft roles in the same
process, so there is no separate controller container. The broker is
started via `rpk redpanda start --mode dev-container` (which bundles
`--overprovisioned`, `--reserve-memory 0M`, `--check=false`, and
`--unsafe-bypass-fsync`).

- `RedpandaCluster.BootstrapServers(ctx) ([]string, error)` —
  `[host:9092]`.
- `RedpandaCluster.BindBrokers(c *dagger.Container) *dagger.Container`
  — same shape as `Cluster.BindBrokers`.
- `RedpandaCluster.Client(ctx, security *KafkaClientSecurity)
  (*KafkaClient, error)` — returns the same `*Client` the Apache
  constructors return. The Kafka wire protocol matches, so
  producer/consumer code is shared.
- `RedpandaCluster.SchemaRegistry(ctx) *SchemaRegistry` — the bundled
  in-broker Schema Registry as the shared `*SchemaRegistry` type — see
  "Bundled Schema Registry" below.

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
the per-cluster server leaf, then mounts the PEM cert/key files into
the broker container at `/etc/redpanda/certs/server.{crt,key}` and
points the rendered `redpanda.yaml` at those paths (the YAML never
embeds PEM material itself). Callers don't have to convert between
formats. The server private key crosses into the broker container as
a `*dagger.Secret` (mounted via `WithMountedSecret`) so its plaintext
never lands in the module workdir. The CA cert *is* mounted, at
`/etc/redpanda/certs/ca.crt` — it is the truststore for the bundled
Schema Registry's internal broker client, which must dial the TLS-only
Kafka listener to persist `_schemas` (see "Bundled Schema Registry"
below). mTLS on the external listener (client-auth) is a follow-up;
this story is server-side TLS only.

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
sr := cluster.SchemaRegistry()
client := sr.Client()

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

The registry's REST endpoint is plain HTTP regardless of the cluster's
Kafka-listener security mode, so `SchemaRegistry()` works on both
PLAINTEXT and TLS Redpanda clusters. (Securing the SR endpoint itself
with TLS is a follow-up, matching `ConfluentSchemaRegistry`.)

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

// java client.properties (+ p12 sidecars in TLS / mTLS modes) for the
// Apache Kafka CLI tools — export the parent directory so the relative
// truststore.p12 / keystore.p12 references resolve.
props := client.PropertiesFile() // *dagger.File — resolve via .Contents(ctx) / .Export(ctx)
```

`keyEncoding` / `valueEncoding` accept `"raw"` (literal UTF-8 bytes),
`"hex"`, or `"base64"` (standard padding). Anything else is rejected.

Topic auto-creation is disabled on the broker — call `CreateTopic` before
`Produce` / `Consume`.

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
  `docker.io/redpandadata/redpanda:v26.1.7`). Single-node only;
  separate `*RedpandaCluster` / `*RedpandaServerSecurity` types
  because the config layer doesn't share the `KAFKA_*` env-var
  contract — see the "RedpandaCluster" section.

`registry` (default `docker.io`) and `tag` are the only caller-
overridable parts; the `apache/kafka{-native,}`, `confluentinc/cp-kafka`,
or `redpandadata/redpanda` portion is fixed per constructor.
