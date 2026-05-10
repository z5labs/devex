# kafka

A Dagger module that spins up a KRaft Kafka cluster from
`apache/kafka-native` and exposes a pure-Go franz-go client that targets
either the local cluster or any reachable remote cluster (AWS MSK,
Confluent Cloud, etc.).

Plaintext is the only security mechanism supported in this story; TLS /
mTLS lands in a follow-up.

## Security profiles

```go
serverSec := dag.Kafka().PlaintextServerSecurity()
clientSec := dag.Kafka().PlaintextClientSecurity()
```

## Cluster

```go
cluster := dag.Kafka().Cluster(
    clusterId,        // 22-char base64-url Kafka cluster ID
    "4.2.0",          // apache/kafka-native tag
    serverSec,
    dagger.KafkaClusterOpts{
        Controllers: 1,    // multi-controller is rejected; see below
        Brokers:     2,
        Registry:    "docker.io",
    },
)
```

`Cluster` is a lazy chain — the server-side constructor only runs when a
leaf op (e.g. `BootstrapServers`) resolves.

- `Cluster.BootstrapServers(ctx) ([]string, error)` — the broker `host:port`
  pairs the client (and `BindBrokers` consumers) connect to.
- `Cluster.BindBrokers(c *dagger.Container) *dagger.Container` — chains
  `WithServiceBinding` for every broker so a caller's container resolves
  the same hostnames `BootstrapServers` reports.
- `Cluster.Client(security *KafkaClientSecurity) *KafkaClient` — starts every
  broker service and returns a franz-go-backed Client wired to dial them.

### Topology limit

`controllers > 1` is **rejected** in this story: a true HA quorum needs every
controller to know every other controller at static config time, which
Dagger's `WithServiceBinding` model can't express without an unresolvable
cycle (controllers don't have a runtime `$(hostname)` escape hatch the way
brokers do, because each controller's `KAFKA_CONTROLLER_QUORUM_VOTERS`
must reference *every* peer up-front). Multi-controller HA lands in a
follow-up alongside TLS / mTLS.

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
})

// java client.properties for the apache kafka CLI tools
props := client.PropertiesFile() // *dagger.File
```

`keyEncoding` / `valueEncoding` accept `"raw"` (literal UTF-8 bytes),
`"hex"`, or `"base64"` (standard padding). Anything else is rejected.

## Image source

Image is built as `<registry>/apache/kafka-native:<tag>`. The
`apache/kafka-native` portion is fixed; only `registry` (default
`docker.io`) and `tag` are caller-overridable.
