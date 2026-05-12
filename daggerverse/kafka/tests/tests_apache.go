package main

import (
	"context"
	"fmt"
	"time"

	"dagger/tests/internal/dagger"
)

// freshClusterApache is the JVM-image (apache/kafka) sibling of freshCluster.
// Same single-controller-single-broker plaintext topology — only the
// underlying image differs — so it lets tests verify the JVM variant
// without otherwise diverging from the freshCluster flow.
func freshClusterApache(ctx context.Context, kafkaImageTag string) (*dagger.KafkaCluster, error) {
	clusterId, err := newClusterId(ctx)
	if err != nil {
		return nil, err
	}
	k := dag.Kafka()
	return k.ApacheCluster(clusterId, k.PlaintextServerSecurity(), dagger.KafkaApacheClusterOpts{
		Tag: kafkaImageTag,
	}), nil
}

// freshTlsClusterApache is the JVM-image sibling of freshTlsCluster.
func freshTlsClusterApache(ctx context.Context, kafkaImageTag string, brokers int) (
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
	cluster := dag.Kafka().ApacheCluster(clusterId, serverSec, dagger.KafkaApacheClusterOpts{
		Tag:     kafkaImageTag,
		Brokers: brokers,
	})
	return cluster, ts.Pkcs12(), ts.Password(), nil
}

// freshMtlsClusterApache is the JVM-image sibling of freshMtlsCluster.
func freshMtlsClusterApache(ctx context.Context, kafkaImageTag string, brokers int) (
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
	leafKey := dag.SetSecret("mtls-apache-client-leaf-key-"+suffix, leafKeyPem)

	leafPwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("generate client leaf password: %w", err)
	}
	leafPwd := dag.SetSecret("mtls-apache-client-leaf-pwd-"+suffix, leafPwdHex)

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
	cluster = dag.Kafka().ApacheCluster(clusterId, serverSec, dagger.KafkaApacheClusterOpts{
		Tag:     kafkaImageTag,
		Brokers: brokers,
	})
	return cluster, serverTrust.Pkcs12(), serverTrust.Password(), leafKs.Pkcs12(), leafKs.Password(), nil
}

// ApacheClusterProduceListTopicsRoundTrip is the PLAINTEXT happy-path
// smoke test for Kafka.ApacheCluster (the JVM image variant): produce a
// single raw record, then call ListTopics and assert the freshly-created
// topic shows up. Together these prove the JVM image's data plane and
// control plane both work; the env-var contract matches
// ApacheNativeCluster so this single test pins down "JVM image actually
// serves traffic".
func (t *Tests) ApacheClusterProduceListTopicsRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshClusterApache(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create apache cluster: %w", err)
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

// ApacheClusterTlsRoundTrip is the TLS happy-path round-trip for
// Kafka.ApacheCluster. Mirrors TlsRoundTrip but on the JVM image to
// rule out image-specific differences in keystore mounts, hostname
// verification, and SSL listener bring-up.
func (t *Tests) ApacheClusterTlsRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, ts, tsPwd, err := freshTlsClusterApache(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create apache tls cluster: %w", err)
	}
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
		return fmt.Errorf("apache tls round-trip mismatch: got (%q, %q), want (%q, %q)", gotKey, gotVal, wantKey, wantVal)
	}
	return nil
}

// ApacheClusterMtlsRoundTrip is the MTLS happy-path round-trip for
// Kafka.ApacheCluster. Mirrors MtlsRoundTrip but on the JVM image to
// rule out image-specific differences in how client-cert challenge is
// handled.
func (t *Tests) ApacheClusterMtlsRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, ts, tsPwd, ks, ksPwd, err := freshMtlsClusterApache(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create apache mtls cluster: %w", err)
	}
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
		return fmt.Errorf("apache mtls round-trip mismatch: got (%q, %q), want (%q, %q)", gotKey, gotVal, wantKey, wantVal)
	}
	return nil
}
