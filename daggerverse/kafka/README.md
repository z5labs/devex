# kafka

A Dagger module that spins up a KRaft Kafka cluster from
`apache/kafka-native` and exposes a pure-Go franz-go client that targets
either the local cluster or any reachable remote cluster (AWS MSK,
Confluent Cloud, etc.).

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

Image is built as `<registry>/apache/kafka-native:<tag>`. The
`apache/kafka-native` portion is fixed; only `registry` (default
`docker.io`) and `tag` (default `4.2.0`) are caller-overridable.
