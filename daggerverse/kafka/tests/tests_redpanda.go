package main

import (
	"context"
	"fmt"

	"dagger/tests/internal/dagger"
)

// freshTlsRedpandaCluster spins up a single-node Redpanda cluster with TLS
// on the external Kafka listener. Mints a fresh CA via the shared freshCa
// helper and hands its PKCS#12 archive to Kafka.RedpandaTlsServerSecurity;
// the cluster constructor extracts PEM internally for redpanda.yaml. The
// matching truststore is returned so the franz-go client can verify the
// broker leaf, plus the CA keystore so the bundled Schema Registry can be
// handed a matching TLS SchemaRegistrySecurity.
func freshTlsRedpandaCluster(ctx context.Context, redpandaImageTag string) (
	cluster *dagger.KafkaRedpandaCluster,
	trustStore *dagger.File, trustStorePwd *dagger.Secret,
	caKeyStore *dagger.File, caKeyStorePwd *dagger.Secret,
	err error,
) {
	ca, err := freshCa(ctx, "tls-redpanda")
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	caKs := ca.KeyStore()
	serverSec := dag.Kafka().RedpandaTLSServerSecurity(caKs.Pkcs12(), caKs.Password())
	ts := ca.TrustStore()

	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	cluster = dag.Kafka().RedpandaCluster(clusterId, serverSec, dagger.KafkaRedpandaClusterOpts{
		Tag: redpandaImageTag,
	})
	return cluster, ts.Pkcs12(), ts.Password(), caKs.Pkcs12(), caKs.Password(), nil
}

// freshRedpandaCluster spins up a single-node Redpanda cluster on PLAINTEXT
// for the duration of one test. Redpanda's config layer (rpk + YAML) is
// disjoint from the KAFKA_* env-var contract Apache and Confluent share,
// hence the dedicated helper.
func freshRedpandaCluster(ctx context.Context, redpandaImageTag string) (*dagger.KafkaRedpandaCluster, error) {
	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, err
	}
	k := dag.Kafka()
	return k.RedpandaCluster(clusterId, k.RedpandaPlaintextServerSecurity(), dagger.KafkaRedpandaClusterOpts{
		Tag: redpandaImageTag,
	}), nil
}

// freshRedpandaClusterMultiBroker spins up a PLAINTEXT Redpanda cluster of
// `brokers` nodes for one test. The nodes form a single Raft group via
// Redpanda's seed-driven bootstrap over the internal RPC listener.
func freshRedpandaClusterMultiBroker(ctx context.Context, redpandaImageTag string, brokers int) (*dagger.KafkaRedpandaCluster, error) {
	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, err
	}
	k := dag.Kafka()
	return k.RedpandaCluster(clusterId, k.RedpandaPlaintextServerSecurity(), dagger.KafkaRedpandaClusterOpts{
		Tag:     redpandaImageTag,
		Brokers: brokers,
	}), nil
}

// freshTlsRedpandaClusterMultiBroker is the TLS counterpart of
// freshRedpandaClusterMultiBroker: it mints a fresh CA, stands up a
// `brokers`-node Redpanda cluster with TLS on every node's external Kafka
// listener (each node's leaf SAN'd to its own hostname), and returns the
// truststore so a franz-go client can verify any broker it is routed to.
func freshTlsRedpandaClusterMultiBroker(ctx context.Context, redpandaImageTag string, brokers int) (
	cluster *dagger.KafkaRedpandaCluster,
	trustStore *dagger.File, trustStorePwd *dagger.Secret,
	err error,
) {
	ca, err := freshCa(ctx, "tls-redpanda-multi")
	if err != nil {
		return nil, nil, nil, err
	}
	caKs := ca.KeyStore()
	serverSec := dag.Kafka().RedpandaTLSServerSecurity(caKs.Pkcs12(), caKs.Password())
	ts := ca.TrustStore()

	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	cluster = dag.Kafka().RedpandaCluster(clusterId, serverSec, dagger.KafkaRedpandaClusterOpts{
		Tag:     redpandaImageTag,
		Brokers: brokers,
	})
	return cluster, ts.Pkcs12(), ts.Password(), nil
}

// RedpandaMultiBrokerControllerPolicyRejected pins the two construction-time
// policies Redpanda enforces: `controllers != 1` is rejected (Redpanda has no
// separate controller role) and `brokers < 1` is rejected. Resolving
// BootstrapServers forces the server-side constructor to run its validation
// without booting any container, so both rejections are exercised cheaply.
func (t *Tests) RedpandaMultiBrokerControllerPolicyRejected(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	k := dag.Kafka()

	// controllers != 1 must be rejected.
	for _, controllers := range []int{2, 3} {
		clusterId, err := newClusterId(ctx)
		if err != nil {
			return err
		}
		cluster := k.RedpandaCluster(clusterId, k.RedpandaPlaintextServerSecurity(), dagger.KafkaRedpandaClusterOpts{
			Tag:         redpandaImageTag,
			Controllers: controllers,
			Brokers:     1,
		})
		if _, err := cluster.BootstrapServers(ctx); err == nil {
			return fmt.Errorf("expected RedpandaCluster(controllers=%d) to fail, got nil error", controllers)
		}
	}

	// brokers < 1 must be rejected. Dagger drops the zero value and applies
	// the +default=1, so the sub-1 case uses -1.
	clusterId, err := newClusterId(ctx)
	if err != nil {
		return err
	}
	cluster := k.RedpandaCluster(clusterId, k.RedpandaPlaintextServerSecurity(), dagger.KafkaRedpandaClusterOpts{
		Tag:     redpandaImageTag,
		Brokers: -1,
	})
	if _, err := cluster.BootstrapServers(ctx); err == nil {
		return fmt.Errorf("expected RedpandaCluster(brokers=-1) to fail, got nil error")
	}
	return nil
}

// RedpandaMultiBrokerBootstrapServersListsEveryNode proves the constructor
// accepts brokers=3 and returns a cluster advertising all three broker
// bootstrap addresses. Resolving BootstrapServers forces the server-side
// constructor (validation + the full container graph, including the per-node
// seed list) to run without booting the containers, so the accept path is
// exercised cheaply — the real three-node Raft round-trip is covered by
// RedpandaThreeBrokerReplicationFactorThreeProduceConsume.
func (t *Tests) RedpandaMultiBrokerBootstrapServersListsEveryNode(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, err := freshRedpandaClusterMultiBroker(ctx, redpandaImageTag, 3)
	if err != nil {
		return fmt.Errorf("create redpanda cluster: %w", err)
	}
	bs, err := cluster.BootstrapServers(ctx)
	if err != nil {
		return fmt.Errorf("expected RedpandaCluster(brokers=3) to construct, got: %w", err)
	}
	if len(bs) != 3 {
		return fmt.Errorf("expected 3 bootstrap servers for a 3-broker cluster, got %d: %v", len(bs), bs)
	}
	return nil
}

// RedpandaThreeBrokerReplicationFactorThreeProduceConsume stands up a real
// three-node Redpanda cluster and drives a produce → consume round-trip over
// an RF=3 topic. A successful round-trip proves the three nodes formed a
// single Raft group over the internal RPC listener (seed-driven bootstrap) and
// that inter-node replication is actually exercised — an RF=3 topic can only be
// created and written if all three brokers reach each other.
func (t *Tests) RedpandaThreeBrokerReplicationFactorThreeProduceConsume(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, err := freshRedpandaClusterMultiBroker(ctx, redpandaImageTag, 3)
	if err != nil {
		return fmt.Errorf("create redpanda cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return redpandaReplicatedProduceConsumeOn(ctx, cluster, dag.Kafka().PlaintextClientSecurity(), 3)
}

// RedpandaThreeBrokerTlsReplicationFactorThreeProduceConsume is the TLS
// counterpart: a three-node Redpanda cluster with TLS on every node's external
// Kafka listener, driving a produce → consume round-trip over an RF=3 topic.
// The franz-go client verifies whichever node it is routed to against the
// cluster truststore, proving every node's leaf is SAN'd to its own hostname.
func (t *Tests) RedpandaThreeBrokerTlsReplicationFactorThreeProduceConsume(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, ts, tsPwd, err := freshTlsRedpandaClusterMultiBroker(ctx, redpandaImageTag, 3)
	if err != nil {
		return fmt.Errorf("create redpanda tls cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return redpandaReplicatedProduceConsumeOn(ctx, cluster, dag.Kafka().TLSClientSecurity(ts, tsPwd), 3)
}

// redpandaReplicatedProduceConsumeOn creates an RF=`rf` topic on the given
// cluster, produces one record, and consumes it back — the shared body behind
// the PLAINTEXT and TLS multi-broker round-trips.
func redpandaReplicatedProduceConsumeOn(
	ctx context.Context,
	cluster *dagger.KafkaRedpandaCluster,
	clientSec *dagger.KafkaClientSecurity,
	rf int,
) error {
	client := cluster.Client(clientSec)

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: rf,
	}); err != nil {
		return fmt.Errorf("create RF=%d topic %q: %w", rf, topic, err)
	}

	const wantKey, wantVal = "k", "v"
	if err := client.Produce(ctx, topic, wantKey, wantVal, dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
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
	if gotKey != wantKey || gotVal != wantVal {
		return fmt.Errorf("redpanda replicated round-trip mismatch: got (%q, %q), want (%q, %q)", gotKey, gotVal, wantKey, wantVal)
	}
	return nil
}

// RedpandaMultiBrokerSchemaRegistryRoundTrip proves Redpanda's bundled Schema
// Registry still works against a multi-node cluster: it stands up a three-node
// cluster, registers a schema against cluster.SchemaRegistry() (which points
// at node 0, whose service cascades the whole cluster online), and asserts the
// lookup-by-id round-trip. The `_schemas` topic itself is replicated across the
// cluster, so a successful round-trip proves the registry reaches a formed
// multi-node Raft group.
func (t *Tests) RedpandaMultiBrokerSchemaRegistryRoundTrip(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, err := freshRedpandaClusterMultiBroker(ctx, redpandaImageTag, 3)
	if err != nil {
		return fmt.Errorf("create redpanda cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	return redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx,
		cluster.SchemaRegistry(plaintextSchemaRegistrySecurity()),
		plaintextSchemaRegistryClientSecurity())
}

// RedpandaClusterProduceListTopicsRoundTrip is the PLAINTEXT happy-path
// round-trip for Kafka.RedpandaCluster: spin up a single-node Redpanda,
// create a topic, produce one record, then assert the freshly-created
// topic shows up in ListTopics. Pins down "redpanda actually serves
// Kafka-wire traffic on the external listener".
func (t *Tests) RedpandaClusterProduceListTopicsRoundTrip(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, err := freshRedpandaCluster(ctx, redpandaImageTag)
	if err != nil {
		return fmt.Errorf("create redpanda cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return redpandaClusterProduceListTopicsRoundTripOn(ctx, cluster)
}

func redpandaClusterProduceListTopicsRoundTripOn(ctx context.Context, cluster *dagger.KafkaRedpandaCluster) error {
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

	if err := client.Produce(ctx, topic, "k", "v", dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	topics, err := client.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("list topics: %w", err)
	}
	if !contains(topics, topic) {
		return fmt.Errorf("expected topic %q in ListTopics output, got %v", topic, topics)
	}
	return nil
}

// RedpandaClusterTlsRoundTrip is the TLS happy-path round-trip for
// Kafka.RedpandaCluster: spin up Redpanda with kafka_api_tls.enabled=true
// using PEM cert/key/CA mounted into /etc/redpanda/certs, then produce
// and consume one record over the TLS listener with the franz-go client
// verifying the broker leaf against the matching truststore.
func (t *Tests) RedpandaClusterTlsRoundTrip(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, ts, tsPwd, _, _, err := freshTlsRedpandaCluster(ctx, redpandaImageTag)
	if err != nil {
		return fmt.Errorf("create redpanda tls cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return redpandaClusterTlsRoundTripOn(ctx, cluster, ts, tsPwd)
}

func redpandaClusterTlsRoundTripOn(
	ctx context.Context,
	cluster *dagger.KafkaRedpandaCluster,
	ts *dagger.File,
	tsPwd *dagger.Secret,
) error {
	client := cluster.Client(dag.Kafka().TLSClientSecurity(ts, tsPwd))

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

	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
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
	if gotKey != wantKey || gotVal != wantVal {
		return fmt.Errorf("redpanda tls round-trip mismatch: got (%q, %q), want (%q, %q)", gotKey, gotVal, wantKey, wantVal)
	}
	return nil
}

// redpandaSchemaRegistryRegisterLookupRoundTripOn registers a fresh schema
// against the given bundled Schema Registry and asserts the full
// lookup-by-id contract — positive id, Subject, SchemaType, and SchemaID.
// Shared by the PLAINTEXT and TLS Redpanda SR tests so both paths cover the
// identical assertion set; mirrors the redpandaCluster...RoundTripOn split.
func redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx context.Context, sr *dagger.KafkaSchemaRegistry, clientSec *dagger.KafkaSchemaRegistryClientSecurity) error {
	client := sr.Client(clientSec)

	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	subject += "-value"

	id, err := client.RegisterSchema(ctx, subject, avroTestSchema, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "AVRO",
	})
	if err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("expected a positive schema id, got %d", id)
	}

	got := client.LookupSchemaByID(id)
	gotSubject, err := got.Subject(ctx)
	if err != nil {
		return fmt.Errorf("lookup schema by id: %w", err)
	}
	if gotSubject != subject {
		return fmt.Errorf("lookup-by-id subject mismatch: want %q, got %q", subject, gotSubject)
	}
	gotType, err := got.SchemaType(ctx)
	if err != nil {
		return fmt.Errorf("read schema type: %w", err)
	}
	if gotType != "AVRO" {
		return fmt.Errorf("lookup-by-id schemaType mismatch: want AVRO, got %q", gotType)
	}
	gotID, err := got.SchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read schema id: %w", err)
	}
	if gotID != id {
		return fmt.Errorf("lookup-by-id schemaID mismatch: want %d, got %d", id, gotID)
	}
	return nil
}

// RedpandaSchemaRegistryRegisterLookupRoundTrip is the PLAINTEXT happy-path
// test for RedpandaCluster.SchemaRegistry: `rpk redpanda start` runs a Schema
// Registry inside the broker process on :8081, so this registers a schema
// against cluster.SchemaRegistry() and asserts the lookup-by-id round-trip —
// proving the bundled SR is reachable and interchangeable with the
// separate-container ConfluentSchemaRegistry. The SR service is the broker
// itself, so cluster.Stop tears it down — sr.Stop is a no-op for a bundled
// registry.
func (t *Tests) RedpandaSchemaRegistryRegisterLookupRoundTrip(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, err := freshRedpandaCluster(ctx, redpandaImageTag)
	if err != nil {
		return fmt.Errorf("create redpanda cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	return redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx,
		cluster.SchemaRegistry(plaintextSchemaRegistrySecurity()),
		plaintextSchemaRegistryClientSecurity())
}

// RedpandaSchemaRegistryTlsRegisterLookupRoundTrip is the TLS-cluster
// counterpart of RedpandaSchemaRegistryRegisterLookupRoundTrip. A TLS
// Redpanda cluster terminates HTTPS on its bundled Schema Registry REST
// endpoint, reusing the broker's server leaf (configured through the
// separately-rendered redpanda.yaml schema_registry_api_tls block), so this
// exercises that YAML path end-to-end and drives register/lookup over HTTPS,
// verifying the SR cert against the cluster CA truststore.
func (t *Tests) RedpandaSchemaRegistryTlsRegisterLookupRoundTrip(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, ts, tsPwd, caKs, caKsPwd, err := freshTlsRedpandaCluster(ctx, redpandaImageTag)
	if err != nil {
		return fmt.Errorf("create redpanda tls cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	// The bundled SR reuses the broker leaf; the caller still passes a TLS
	// SchemaRegistrySecurity (its CA keystore is unused here) so the mode
	// matches the TLS cluster, and a TLS client profile built from the same
	// cluster CA truststore verifies the HTTPS endpoint.
	srSec := dag.Kafka().TLSSchemaRegistrySecurity(caKs, caKsPwd)
	clientSec := dag.Kafka().TLSSchemaRegistryClientSecurity(ts, tsPwd)
	return redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx, cluster.SchemaRegistry(srSec), clientSec)
}

// RedpandaSchemaRegistryBundledStopIsNoOp pins the bundled-registry lifecycle
// contract: a bundled *SchemaRegistry shares the broker service, so sr.Stop
// must be a no-op — the cluster owns teardown via cluster.Stop. This runs a
// register/lookup round-trip, calls sr.Stop, then exercises the registry
// again through the same handle. If sr.Stop had torn down the shared broker
// service, that follow-up call would fail.
func (t *Tests) RedpandaSchemaRegistryBundledStopIsNoOp(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, err := freshRedpandaCluster(ctx, redpandaImageTag)
	if err != nil {
		return fmt.Errorf("create redpanda cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	sr := cluster.SchemaRegistry(plaintextSchemaRegistrySecurity())
	if err := redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx, sr, plaintextSchemaRegistryClientSecurity()); err != nil {
		return fmt.Errorf("round-trip before sr.Stop: %w", err)
	}

	if err := sr.Stop(ctx); err != nil {
		return fmt.Errorf("bundled sr.Stop should be a no-op, got: %w", err)
	}

	// The registry must still be reachable after sr.Stop — register one more
	// schema and confirm its subject lands in ListSubjects. Both calls
	// round-trip to the broker, so a torn-down broker would fail here. (A
	// fresh register/lookup-by-id round-trip would not prove this: the
	// registry dedups by schema content, so re-registering avroTestSchema
	// returns the first id, whose lookup resolves to the first subject.)
	client := sr.Client(plaintextSchemaRegistryClientSecurity())
	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	subject += "-value"
	id, err := client.RegisterSchema(ctx, subject, avroTestSchema, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "AVRO",
	})
	if err != nil {
		return fmt.Errorf("register after sr.Stop (broker should still be alive): %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("register after sr.Stop: expected a positive schema id, got %d", id)
	}
	subjects, err := client.ListSubjects(ctx)
	if err != nil {
		return fmt.Errorf("list subjects after sr.Stop: %w", err)
	}
	if !contains(subjects, subject) {
		return fmt.Errorf("expected subject %q in ListSubjects output after sr.Stop, got %v", subject, subjects)
	}
	return nil
}
