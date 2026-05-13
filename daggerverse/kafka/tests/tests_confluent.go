package main

import (
	"context"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"
)

// freshConfluentCluster is the Confluent Platform (cp-kafka) sibling of
// freshCluster. Single-controller, single-broker, plaintext external
// listener — only the underlying image differs — so tests can verify
// the cp-kafka distro without diverging from the freshCluster flow.
func freshConfluentCluster(ctx context.Context, confluentImageTag string) (*dagger.KafkaCluster, error) {
	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, err
	}
	k := dag.Kafka()
	return k.ConfluentCluster(clusterId, k.PlaintextServerSecurity(), dagger.KafkaConfluentClusterOpts{
		Tag: confluentImageTag,
	}), nil
}

// freshTlsConfluentCluster is the cp-kafka sibling of freshTlsCluster.
func freshTlsConfluentCluster(ctx context.Context, confluentImageTag string, brokers int) (
	*dagger.KafkaCluster, *dagger.File, *dagger.Secret, error,
) {
	ca, err := freshCa(ctx, "tls-server")
	if err != nil {
		return nil, nil, nil, err
	}
	caKs := ca.KeyStore()
	serverSec := dag.Kafka().TLSServerSecurity(caKs.Pkcs12(), caKs.Password())
	ts := ca.TrustStore()

	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	cluster := dag.Kafka().ConfluentCluster(clusterId, serverSec, dagger.KafkaConfluentClusterOpts{
		Tag:     confluentImageTag,
		Brokers: brokers,
	})
	return cluster, ts.Pkcs12(), ts.Password(), nil
}

// freshMtlsConfluentCluster is the cp-kafka sibling of freshMtlsCluster.
func freshMtlsConfluentCluster(ctx context.Context, confluentImageTag string, brokers int) (
	cluster *dagger.KafkaCluster,
	serverTs *dagger.File, serverTsPwd *dagger.Secret,
	clientKs *dagger.File, clientKsPwd *dagger.Secret,
	err error,
) {
	serverCa, err := freshCa(ctx, "mtls-server")
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	clientCa, err := freshCa(ctx, "mtls-client")
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	serverCaKs := serverCa.KeyStore()
	clientCaTs := clientCa.TrustStore()
	serverSec := dag.Kafka().MtlsServerSecurity(
		serverCaKs.Pkcs12(), serverCaKs.Password(),
		clientCaTs.Pkcs12(), clientCaTs.Password(),
	)

	suffix, err := randHex(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	leafKeyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("generate client leaf key: %w", err)
	}
	leafKey := dag.SetSecret("mtls-confluent-client-leaf-key-"+suffix, leafKeyPem)

	leafPwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("generate client leaf password: %w", err)
	}
	leafPwd := dag.SetSecret("mtls-confluent-client-leaf-pwd-"+suffix, leafPwdHex)

	leafSerial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("generate client leaf serial: %w", err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	clientLeaf := clientCa.IssueClientCertificate("test-client", nb, leafSerial, leafPwd, leafKey)
	leafKs := clientLeaf.KeyStore()

	serverTrust := serverCa.TrustStore()

	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	cluster = dag.Kafka().ConfluentCluster(clusterId, serverSec, dagger.KafkaConfluentClusterOpts{
		Tag:     confluentImageTag,
		Brokers: brokers,
	})
	return cluster, serverTrust.Pkcs12(), serverTrust.Password(), leafKs.Pkcs12(), leafKs.Password(), nil
}

// ConfluentClusterProduceListTopicsRoundTrip is the PLAINTEXT happy-path
// smoke test for Kafka.ConfluentCluster (the cp-kafka image variant):
// produce a single raw record, then call ListTopics and assert the
// freshly-created topic shows up. Confluent Platform's cp-kafka image
// uses the same `KAFKA_*` Scala-wrapper contract as Apache, so this
// single test pins down "cp-kafka actually serves traffic".
func (t *Tests) ConfluentClusterProduceListTopicsRoundTrip(
	ctx context.Context,
	// +default="8.2.0"
	confluentImageTag string,
) error {
	cluster, err := freshConfluentCluster(ctx, confluentImageTag)
	if err != nil {
		return fmt.Errorf("create confluent cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return confluentClusterProduceListTopicsRoundTripOn(ctx, cluster)
}

func confluentClusterProduceListTopicsRoundTripOn(ctx context.Context, cluster *dagger.KafkaCluster) error {
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

// ConfluentClusterTlsRoundTrip is the TLS happy-path round-trip for
// Kafka.ConfluentCluster. Mirrors TlsRoundTrip but on cp-kafka to rule
// out distro-specific differences in keystore mounts, hostname
// verification, and SSL listener bring-up.
func (t *Tests) ConfluentClusterTlsRoundTrip(
	ctx context.Context,
	// +default="8.2.0"
	confluentImageTag string,
) error {
	cluster, ts, tsPwd, err := freshTlsConfluentCluster(ctx, confluentImageTag, 1)
	if err != nil {
		return fmt.Errorf("create confluent tls cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return confluentClusterTlsRoundTripOn(ctx, cluster, ts, tsPwd)
}

func confluentClusterTlsRoundTripOn(
	ctx context.Context,
	cluster *dagger.KafkaCluster,
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
		return fmt.Errorf("confluent tls round-trip mismatch: got (%q, %q), want (%q, %q)", gotKey, gotVal, wantKey, wantVal)
	}
	return nil
}

// ConfluentClusterMtlsRoundTrip is the MTLS happy-path round-trip for
// Kafka.ConfluentCluster. Mirrors MtlsRoundTrip but on cp-kafka to rule
// out distro-specific differences in how client-cert challenge is
// handled.
func (t *Tests) ConfluentClusterMtlsRoundTrip(
	ctx context.Context,
	// +default="8.2.0"
	confluentImageTag string,
) error {
	cluster, ts, tsPwd, ks, ksPwd, err := freshMtlsConfluentCluster(ctx, confluentImageTag, 1)
	if err != nil {
		return fmt.Errorf("create confluent mtls cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return confluentClusterMtlsRoundTripOn(ctx, cluster, ts, tsPwd, ks, ksPwd)
}

func confluentClusterMtlsRoundTripOn(
	ctx context.Context,
	cluster *dagger.KafkaCluster,
	ts *dagger.File,
	tsPwd *dagger.Secret,
	ks *dagger.File,
	ksPwd *dagger.Secret,
) error {
	client := cluster.Client(dag.Kafka().MtlsClientSecurity(ks, ksPwd, ts, tsPwd))

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
		return fmt.Errorf("confluent mtls round-trip mismatch: got (%q, %q), want (%q, %q)", gotKey, gotVal, wantKey, wantVal)
	}
	return nil
}
