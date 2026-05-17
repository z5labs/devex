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
// broker leaf.
func freshTlsRedpandaCluster(ctx context.Context, redpandaImageTag string) (
	*dagger.KafkaRedpandaCluster, *dagger.File, *dagger.Secret, error,
) {
	ca, err := freshCa(ctx, "tls-redpanda")
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
	cluster := dag.Kafka().RedpandaCluster(clusterId, serverSec, dagger.KafkaRedpandaClusterOpts{
		Tag: redpandaImageTag,
	})
	return cluster, ts.Pkcs12(), ts.Password(), nil
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
	cluster, ts, tsPwd, err := freshTlsRedpandaCluster(ctx, redpandaImageTag)
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
func redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx context.Context, sr *dagger.KafkaSchemaRegistry) error {
	client := sr.Client()

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

	return redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx, cluster.SchemaRegistry())
}

// RedpandaSchemaRegistryTlsRegisterLookupRoundTrip is the TLS-cluster
// counterpart of RedpandaSchemaRegistryRegisterLookupRoundTrip. A TLS
// Redpanda cluster configures its bundled Schema Registry through a
// separately-rendered redpanda.yaml (the schema_registry_api block) rather
// than the PLAINTEXT path's --schema-registry-addr flag, so this exercises
// that YAML path end-to-end. The SR REST endpoint is plain HTTP regardless
// of the Kafka listener's TLS mode, so the truststore is unused here.
func (t *Tests) RedpandaSchemaRegistryTlsRegisterLookupRoundTrip(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
) error {
	cluster, _, _, err := freshTlsRedpandaCluster(ctx, redpandaImageTag)
	if err != nil {
		return fmt.Errorf("create redpanda tls cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	return redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx, cluster.SchemaRegistry())
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

	sr := cluster.SchemaRegistry()
	if err := redpandaSchemaRegistryRegisterLookupRoundTripOn(ctx, sr); err != nil {
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
	client := sr.Client()
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
