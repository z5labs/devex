// Tests for the kafka daggerverse module. Each test is exposed as a standalone
// dagger function so it can be invoked individually during TDD; All wires them
// up for parallel execution under `dagger call all`.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

// newClusterId mints a fresh KRaft-shaped cluster ID — 16 random bytes
// rendered as 22 unpadded base64-url characters — by feeding random bytes
// from the random module through the standard library.
func newClusterId(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256: %w", err)
	}
	if len(h) < 32 {
		return "", fmt.Errorf("random sha256 too short: %d", len(h))
	}
	raw, err := hex.DecodeString(h[:32])
	if err != nil {
		return "", fmt.Errorf("decode random sha256: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// freshCluster spins up the smallest split-role plaintext cluster the kafka
// module can produce — one dedicated controller container plus one broker
// container — for use by every cluster-touching test. The returned
// KafkaCluster is a lazy chain; the server-side Cluster constructor runs
// only when a leaf op (e.g. BootstrapServers) resolves.
func freshCluster(ctx context.Context, kafkaImageTag string) (*dagger.KafkaCluster, error) {
	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, err
	}
	k := dag.Kafka()
	return k.Cluster(clusterId, k.PlaintextServerSecurity(), dagger.KafkaClusterOpts{
		Tag: kafkaImageTag,
	}), nil
}

type Tests struct{}

// All runs every kafka round-trip test, capped at two concurrent jobs so
// the engine doesn't have dozens of cluster containers (controller +
// brokers per test) in flight at once on smaller CI runners.
//
// kafkaImageTag picks the apache/kafka-native tag every spawned cluster
// runs against, so callers can verify the module against a newer Kafka
// release without first changing main.go. The default matches the
// Cluster constructor's own default.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	jobs := parallel.New().
		WithLimit(2).
		WithRollupLogs(true).
		WithRollupSpans(true)

	jobs = jobs.WithJob("PlaintextSecurityProfilesAreNonNil", t.PlaintextSecurityProfilesAreNonNil)
	jobs = jobs.WithJob("SingleNodeClusterStarts", func(ctx context.Context) error {
		return t.SingleNodeClusterStarts(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ClusterClientCanListTopicsOnFreshCluster", func(ctx context.Context) error {
		return t.ClusterClientCanListTopicsOnFreshCluster(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("CreateAndDeleteTopicRoundTrip", func(ctx context.Context) error {
		return t.CreateAndDeleteTopicRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ProduceConsumeRoundTripRaw", func(ctx context.Context) error {
		return t.ProduceConsumeRoundTripRaw(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ProduceConsumeRoundTripHex", func(ctx context.Context) error {
		return t.ProduceConsumeRoundTripHex(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ProduceConsumeRoundTripBase64", func(ctx context.Context) error {
		return t.ProduceConsumeRoundTripBase64(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ProduceRejectsUnknownEncoding", func(ctx context.Context) error {
		return t.ProduceRejectsUnknownEncoding(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("PropertiesFileContainsBootstrapAndSecurityProtocol", func(ctx context.Context) error {
		return t.PropertiesFileContainsBootstrapAndSecurityProtocol(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("BindBrokersExposesBothListeners", func(ctx context.Context) error {
		return t.BindBrokersExposesBothListeners(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("DedicatedControllerAndBrokerProduceConsume", func(ctx context.Context) error {
		return t.DedicatedControllerAndBrokerProduceConsume(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("OneControllerTwoBrokersReplicationFactorTwo", func(ctx context.Context) error {
		return t.OneControllerTwoBrokersReplicationFactorTwo(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("MultiControllerIsRejected", func(ctx context.Context) error {
		return t.MultiControllerIsRejected(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("AutoCreateTopicsDisabled", func(ctx context.Context) error {
		return t.AutoCreateTopicsDisabled(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ConsumerGroupOnSingleBrokerWorks", func(ctx context.Context) error {
		return t.ConsumerGroupOnSingleBrokerWorks(ctx, kafkaImageTag)
	})

	return jobs.Run(ctx)
}

// randomTopicName mints a fresh, lower-case-alpha-prefixed topic name so
// tests don't collide and so the name is a valid Kafka topic identifier.
func randomTopicName(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256: %w", err)
	}
	if len(h) < 16 {
		return "", fmt.Errorf("random sha256 too short: %d", len(h))
	}
	return "t-" + h[:16], nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func (t *Tests) PlaintextSecurityProfilesAreNonNil(ctx context.Context) error {
	server := dag.Kafka().PlaintextServerSecurity()
	if server == nil {
		return fmt.Errorf("PlaintextServerSecurity returned nil")
	}
	client := dag.Kafka().PlaintextClientSecurity()
	if client == nil {
		return fmt.Errorf("PlaintextClientSecurity returned nil")
	}
	return nil
}

// SingleNodeClusterStarts spins up the smallest split-role cluster (one
// controller + one broker) and forces the server-side Cluster constructor
// to run by resolving BootstrapServers, asserting only that the broker
// hostname is non-empty. End-to-end reachability is covered by sibling
// tests that exercise ListTopics / produce / consume.
func (t *Tests) SingleNodeClusterStarts(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	bs, err := cluster.BootstrapServers(ctx)
	if err != nil {
		return fmt.Errorf("get bootstrap servers: %w", err)
	}
	if len(bs) == 0 {
		return fmt.Errorf("expected at least one bootstrap server, got none")
	}
	for _, b := range bs {
		if b == "" || b == ":9092" {
			return fmt.Errorf("expected non-empty bootstrap server, got %q", b)
		}
	}
	return nil
}

// ClusterClientCanListTopicsOnFreshCluster opens a franz-go-backed Client
// against a fresh cluster and asserts that ListTopics returns without error.
// A fresh KRaft cluster has no user topics, so the result may be empty —
// but the call itself must succeed, which proves module-runtime networking
// can reach the started broker service.
func (t *Tests) ClusterClientCanListTopicsOnFreshCluster(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())
	topics, err := client.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("list topics: %w", err)
	}
	if topics == nil {
		return fmt.Errorf("expected non-nil topics slice, got nil")
	}
	return nil
}

// CreateAndDeleteTopicRoundTrip exercises the create/list/delete cycle to
// confirm kadm wiring. The topic name is randomized so the test is
// repeatable against the same cluster and never collides with leftovers.
func (t *Tests) CreateAndDeleteTopicRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}

	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	listed, err := client.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("list topics after create: %w", err)
	}
	if !contains(listed, topic) {
		return fmt.Errorf("expected topic %q in %v after create", topic, listed)
	}

	if err := client.DeleteTopic(ctx, topic); err != nil {
		return fmt.Errorf("delete topic %q: %w", topic, err)
	}

	listed, err = client.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("list topics after delete: %w", err)
	}
	if contains(listed, topic) {
		return fmt.Errorf("expected topic %q absent after delete, got %v", topic, listed)
	}
	return nil
}

// ProduceConsumeRoundTripRaw produces a single record with raw-encoded key
// and value, then consumes it back and asserts byte equality. The raw
// encoding round-trips Go strings verbatim, so the assertion is direct
// string equality.
func (t *Tests) ProduceConsumeRoundTripRaw(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	const wantKey, wantVal = "k", "v"
	if err := client.Produce(ctx, topic, wantKey, wantVal, dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	records, err := client.Consume(ctx, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:   1,
		Timeout:       "10s",
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	gotKey, err := records[0].Key(ctx)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	gotVal, err := records[0].Value(ctx)
	if err != nil {
		return fmt.Errorf("read value: %w", err)
	}
	if gotKey != wantKey {
		return fmt.Errorf("key mismatch: want %q, got %q", wantKey, gotKey)
	}
	if gotVal != wantVal {
		return fmt.Errorf("value mismatch: want %q, got %q", wantVal, gotVal)
	}
	return nil
}

// ProduceConsumeRoundTripHex round-trips a binary payload through hex
// encoding. The non-UTF-8 bytes (including 0x00) verify that hex transports
// arbitrary binary safely.
func (t *Tests) ProduceConsumeRoundTripHex(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return roundTripBinary(ctx, kafkaImageTag, "hex", "deadbeef", "00010203fffefdfc")
}

// ProduceConsumeRoundTripBase64 round-trips the same kind of binary payload
// through standard base64 (with padding).
func (t *Tests) ProduceConsumeRoundTripBase64(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return roundTripBinary(ctx, kafkaImageTag, "base64", "3q2+7w==", "AAECA//+/fw=")
}

// ProduceRejectsUnknownEncoding verifies that a Produce call with a bogus
// encoding name fails fast rather than silently misbehaving.
func (t *Tests) ProduceRejectsUnknownEncoding(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return err
	}
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	err = client.Produce(ctx, topic, "k", "v", dagger.KafkaClientProduceOpts{
		KeyEncoding:   "rot13",
		ValueEncoding: "raw",
	})
	if err == nil {
		return fmt.Errorf("expected Produce to reject unknown key encoding, got nil error")
	}
	return nil
}

// PropertiesFileContainsBootstrapAndSecurityProtocol verifies that the
// rendered Java client.properties file carries the bootstrap.servers list
// and a plaintext security.protocol entry — enough for the Apache Kafka
// CLI tools to pick up the connection settings.
func (t *Tests) PropertiesFileContainsBootstrapAndSecurityProtocol(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	bs, err := cluster.BootstrapServers(ctx)
	if err != nil {
		return fmt.Errorf("get bootstrap servers: %w", err)
	}
	props, err := cluster.Client(dag.Kafka().PlaintextClientSecurity()).
		PropertiesFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("read PropertiesFile: %w", err)
	}
	want := "bootstrap.servers=" + strings.Join(bs, ",")
	if !strings.Contains(props, want) {
		return fmt.Errorf("expected properties to contain %q, got:\n%s", want, props)
	}
	if !strings.Contains(props, "security.protocol=PLAINTEXT") {
		return fmt.Errorf("expected properties to contain security.protocol=PLAINTEXT, got:\n%s", props)
	}
	return nil
}

// MultiControllerIsRejected pins the current contract: this story only
// supports a single-controller quorum (controllers=1), and the constructor
// must reject any larger value with a clear error rather than silently
// spinning up a broken topology. Multi-controller HA is gated behind a
// follow-up story; see daggerverse/kafka/README.md.
func (t *Tests) MultiControllerIsRejected(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	clusterId, err := newClusterId(ctx)
	if err != nil {
		return err
	}
	k := dag.Kafka()
	cluster := k.Cluster(clusterId, k.PlaintextServerSecurity(), dagger.KafkaClusterOpts{
		Tag:         kafkaImageTag,
		Controllers: 3,
		Brokers:     1,
	})
	if _, err := cluster.BootstrapServers(ctx); err == nil {
		return fmt.Errorf("expected Cluster(controllers=3) to fail, got nil error")
	}
	return nil
}

// OneControllerTwoBrokersReplicationFactorTwo spins up a 1+2 cluster and
// creates a replication-factor-2 topic so the produce path forces inter-
// broker replication. A successful round-trip proves brokers can reach
// each other over the engine network without explicit peer bindings.
func (t *Tests) OneControllerTwoBrokersReplicationFactorTwo(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	clusterId, err := newClusterId(ctx)
	if err != nil {
		return err
	}
	k := dag.Kafka()
	cluster := k.Cluster(clusterId, k.PlaintextServerSecurity(), dagger.KafkaClusterOpts{
		Tag:         kafkaImageTag,
		Controllers: 1,
		Brokers:     2,
	})
	client := cluster.Client(k.PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 2,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	const wantKey, wantVal = "k", "v"
	if err := client.Produce(ctx, topic, wantKey, wantVal, dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	records, err := client.Consume(ctx, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:   1,
		Timeout:       "15s",
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	gotKey, err := records[0].Key(ctx)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	gotVal, err := records[0].Value(ctx)
	if err != nil {
		return fmt.Errorf("read value: %w", err)
	}
	if gotKey != wantKey {
		return fmt.Errorf("key mismatch: want %q, got %q", wantKey, gotKey)
	}
	if gotVal != wantVal {
		return fmt.Errorf("value mismatch: want %q, got %q", wantVal, gotVal)
	}
	return nil
}

// DedicatedControllerAndBrokerProduceConsume verifies that the split
// controller+broker topology (introduced this increment) still supports a
// full produce/consume round-trip — i.e. the broker correctly joined the
// controller quorum over its WithServiceBinding alias.
func (t *Tests) DedicatedControllerAndBrokerProduceConsume(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return roundTripBinary(ctx, kafkaImageTag, "raw", "k", "v")
}

// BindBrokersExposesBothListeners binds the cluster's brokers into a
// vanilla alpine container and asserts that both the host-facing client
// port (9092) and the inter-broker port (19092) are reachable from inside
// that container — together they cover the dual-listener contract
// (PLAINTEXT_HOST:9092 for clients, PLAINTEXT:19092 for inter-broker).
func (t *Tests) BindBrokersExposesBothListeners(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	bs, err := cluster.BootstrapServers(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap servers: %w", err)
	}
	if len(bs) == 0 {
		return fmt.Errorf("expected at least one bootstrap server")
	}
	host, port, ok := strings.Cut(bs[0], ":")
	if !ok {
		return fmt.Errorf("malformed bootstrap server %q", bs[0])
	}

	out, err := cluster.BindBrokers(dag.Container().From("alpine:3.22")).
		WithExec([]string{"nc", "-z", "-w", "5", host, port}).
		WithExec([]string{"nc", "-z", "-w", "5", host, "19092"}).
		WithExec([]string{"echo", "OK"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("bound container exec: %w", err)
	}
	if !strings.Contains(out, "OK") {
		return fmt.Errorf("expected OK from nc, got: %q", out)
	}
	return nil
}

// AutoCreateTopicsDisabled produces to a topic that was never created and
// asserts the call errors out. With KAFKA_AUTO_CREATE_TOPICS_ENABLE=false on
// the broker, the produce path must surface a topic-not-found error rather
// than silently auto-creating, so producer typos can't pass tests.
func (t *Tests) AutoCreateTopicsDisabled(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}

	err = client.Produce(ctx, topic, "k", "v", dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	})
	if err == nil {
		return fmt.Errorf("expected Produce to non-existent topic %q to fail, got nil error", topic)
	}
	return nil
}

// ConsumerGroupOnSingleBrokerWorks produces one record then consumes it back
// through a consumer group on a 1-broker cluster. A successful round-trip
// proves __consumer_offsets was created at the broker's configured
// replication factor (1, after the system-topic env vars take effect).
// Without KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 the broker would refuse
// to create __consumer_offsets at the upstream default RF=3 and the group
// join would hang or error.
func (t *Tests) ConsumerGroupOnSingleBrokerWorks(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	const wantKey, wantVal = "k", "v"
	if err := client.Produce(ctx, topic, wantKey, wantVal, dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	group, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	records, err := client.Consume(ctx, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:   1,
		Timeout:       "20s",
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
		Group:         group,
	})
	if err != nil {
		return fmt.Errorf("consume with group: %w", err)
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	gotKey, err := records[0].Key(ctx)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	gotVal, err := records[0].Value(ctx)
	if err != nil {
		return fmt.Errorf("read value: %w", err)
	}
	if gotKey != wantKey {
		return fmt.Errorf("key mismatch: want %q, got %q", wantKey, gotKey)
	}
	if gotVal != wantVal {
		return fmt.Errorf("value mismatch: want %q, got %q", wantVal, gotVal)
	}
	return nil
}

// roundTripBinary is shared helper for hex/base64 tests: produce one record
// with the given encoding and assert the consumed key/value strings are
// identical to the produced ones.
func roundTripBinary(ctx context.Context, kafkaImageTag, encoding, key, value string) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}
	if err := client.Produce(ctx, topic, key, value, dagger.KafkaClientProduceOpts{
		KeyEncoding:   encoding,
		ValueEncoding: encoding,
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}
	records, err := client.Consume(ctx, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:   1,
		Timeout:       "10s",
		KeyEncoding:   encoding,
		ValueEncoding: encoding,
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	gotKey, err := records[0].Key(ctx)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	gotVal, err := records[0].Value(ctx)
	if err != nil {
		return fmt.Errorf("read value: %w", err)
	}
	if gotKey != key {
		return fmt.Errorf("%s key mismatch: want %q, got %q", encoding, key, gotKey)
	}
	if gotVal != value {
		return fmt.Errorf("%s value mismatch: want %q, got %q", encoding, value, gotVal)
	}
	return nil
}
