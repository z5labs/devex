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
	"time"

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

// freshCa mints a fresh per-test root CA via the certificate-management
// module. Each call uses fresh inputs (random key, random password,
// random serial, time.Now() notBefore) so the resulting CA is unique per
// test. Returns the CA itself (lazy chain) for further leaf signing.
func freshCa(ctx context.Context, label string) (*dagger.CertificateManagementCertificateAuthority, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	key := dag.SetSecret(label+"-ca-key-"+suffix, keyPem)

	pwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca password: %w", label, err)
	}
	pwd := dag.SetSecret(label+"-ca-pwd-"+suffix, pwdHex)

	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	return dag.CertificateManagement().CreateCertificateAuthority(nb, serial, pwd, key,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   "Test CA " + label,
			ValidityDays: 30,
		}), nil
}

// freshTlsCluster mints a fresh CA, hands its keystore to TlsServerSecurity,
// and returns the cluster + the CA's truststore (file + password) so the
// caller can build a matching TlsClientSecurity. Single-broker by default;
// callers needing more brokers can extend the opts before calling.
func freshTlsCluster(ctx context.Context, kafkaImageTag string, brokers int) (
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
	cluster := dag.Kafka().Cluster(clusterId, serverSec, dagger.KafkaClusterOpts{
		Tag:     kafkaImageTag,
		Brokers: brokers,
	})
	return cluster, ts.Pkcs12(), ts.Password(), nil
}

// freshMtlsCluster mints two fresh CAs (one for the server-side leaf
// signing, one for the client trust path), wires them into MtlsServerSecurity,
// and additionally issues a client leaf signed by the client-side CA so the
// caller can build a working MtlsClientSecurity. Returns the cluster, the
// server-CA truststore (file + password) for the client to trust the broker,
// and the client leaf keystore (file + password) for the broker to trust the
// client.
func freshMtlsCluster(ctx context.Context, kafkaImageTag string, brokers int) (
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
	leafKey := dag.SetSecret("mtls-client-leaf-key-"+suffix, leafKeyPem)

	leafPwdHex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("generate client leaf password: %w", err)
	}
	leafPwd := dag.SetSecret("mtls-client-leaf-pwd-"+suffix, leafPwdHex)

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
	cluster = dag.Kafka().Cluster(clusterId, serverSec, dagger.KafkaClusterOpts{
		Tag:     kafkaImageTag,
		Brokers: brokers,
	})
	return cluster, serverTrust.Pkcs12(), serverTrust.Password(), leafKs.Pkcs12(), leafKs.Password(), nil
}

// randHex returns a fresh hex suffix, used to disambiguate Dagger secret
// names across concurrent test invocations within the same engine session.
func randHex(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("randHex: %w", err)
	}
	if len(h) < 16 {
		return "", fmt.Errorf("randHex too short: %d", len(h))
	}
	return h[:16], nil
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
	jobs = jobs.WithJob("TlsClusterStarts", func(ctx context.Context) error {
		return t.TlsClusterStarts(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("TlsRoundTrip", func(ctx context.Context) error {
		return t.TlsRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("TlsClientWithWrongCaFails", func(ctx context.Context) error {
		return t.TlsClientWithWrongCaFails(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("InternalListenersAreEncrypted", func(ctx context.Context) error {
		return t.InternalListenersAreEncrypted(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("MtlsRoundTrip", func(ctx context.Context) error {
		return t.MtlsRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("MtlsRequiresClientCert", func(ctx context.Context) error {
		return t.MtlsRequiresClientCert(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("PropertiesFileContainsTlsSettings", func(ctx context.Context) error {
		return t.PropertiesFileContainsTlsSettings(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("PropertiesFileContainsMtlsSettings", func(ctx context.Context) error {
		return t.PropertiesFileContainsMtlsSettings(ctx, kafkaImageTag)
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

// PropertiesFileContainsTlsSettings verifies the rendered Java
// client.properties carries security.protocol=SSL plus an ssl.truststore.*
// triple referencing a sidecar PKCS#12 file by basename.
func (t *Tests) PropertiesFileContainsTlsSettings(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, ts, tsPwd, err := freshTlsCluster(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create tls cluster: %w", err)
	}
	props, err := cluster.Client(dag.Kafka().TLSClientSecurity(ts, tsPwd)).
		PropertiesFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("read PropertiesFile: %w", err)
	}
	for _, want := range []string{
		"security.protocol=SSL",
		"ssl.truststore.location=truststore.p12",
		"ssl.truststore.type=PKCS12",
	} {
		if !strings.Contains(props, want) {
			return fmt.Errorf("expected properties to contain %q, got:\n%s", want, props)
		}
	}
	return nil
}

// PropertiesFileContainsMtlsSettings verifies that mTLS mode also renders
// the ssl.keystore.* triple referencing a keystore.p12 sidecar.
func (t *Tests) PropertiesFileContainsMtlsSettings(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, ts, tsPwd, ks, ksPwd, err := freshMtlsCluster(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create mtls cluster: %w", err)
	}
	props, err := cluster.Client(dag.Kafka().MtlsClientSecurity(ks, ksPwd, ts, tsPwd)).
		PropertiesFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("read PropertiesFile: %w", err)
	}
	for _, want := range []string{
		"security.protocol=SSL",
		"ssl.truststore.location=truststore.p12",
		"ssl.truststore.type=PKCS12",
		"ssl.keystore.location=keystore.p12",
		"ssl.keystore.type=PKCS12",
	} {
		if !strings.Contains(props, want) {
			return fmt.Errorf("expected properties to contain %q, got:\n%s", want, props)
		}
	}
	return nil
}

// MtlsRequiresClientCert points a TLS-only client (no keystore) at an
// MTLS broker and asserts the handshake fails. Confirms the broker's
// client.auth=required setting is actually being honoured.
func (t *Tests) MtlsRequiresClientCert(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, ts, tsPwd, _, _, err := freshMtlsCluster(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create mtls cluster: %w", err)
	}
	client := cluster.Client(dag.Kafka().TLSClientSecurity(ts, tsPwd))
	if _, err := client.ListTopics(ctx); err == nil {
		return fmt.Errorf("expected ListTopics to fail without a client cert against MTLS broker, got nil error")
	}
	return nil
}

// MtlsRoundTrip produces and consumes a single record over a mutual-TLS
// external listener. The broker presents its cert (signed by the server
// CA) and demands a client cert in return; the test client presents one
// signed by an independent client CA the broker is configured to trust.
func (t *Tests) MtlsRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, ts, tsPwd, ks, ksPwd, err := freshMtlsCluster(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create mtls cluster: %w", err)
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
		return fmt.Errorf("mtls round-trip mismatch: got (%q, %q), want (%q, %q)", gotKey, gotVal, wantKey, wantVal)
	}
	return nil
}

// TlsClientWithWrongCaFails verifies that pointing the client at a
// truststore for an unrelated CA fails the handshake — i.e. the broker is
// genuinely presenting a cert chained to its own CA, not skipping
// verification.
func (t *Tests) TlsClientWithWrongCaFails(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, _, _, err := freshTlsCluster(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create tls cluster: %w", err)
	}
	wrongCa, err := freshCa(ctx, "wrong")
	if err != nil {
		return fmt.Errorf("create unrelated ca: %w", err)
	}
	wrongTs := wrongCa.TrustStore()
	client := cluster.Client(dag.Kafka().TLSClientSecurity(wrongTs.Pkcs12(), wrongTs.Password()))
	if _, err := client.ListTopics(ctx); err == nil {
		return fmt.Errorf("expected ListTopics to fail with wrong-CA truststore, got nil error")
	}
	return nil
}

// InternalListenersAreEncrypted spins up a 1+2 cluster with TLS on the
// external listener and creates an RF=2 topic. A successful produce →
// consume round-trip proves replication traffic flowed over the (always
// mTLS) INTERNAL inter-broker listener: without working internal mTLS,
// the second broker would never become an in-sync replica and the produce
// (with default acks=all-isr) would stall.
func (t *Tests) InternalListenersAreEncrypted(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, ts, tsPwd, err := freshTlsCluster(ctx, kafkaImageTag, 2)
	if err != nil {
		return fmt.Errorf("create tls cluster: %w", err)
	}
	client := cluster.Client(dag.Kafka().TLSClientSecurity(ts, tsPwd))

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
		Timeout:       "20s",
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
		return fmt.Errorf("rf=2 round-trip mismatch: got (%q, %q), want (%q, %q)", gotKey, gotVal, wantKey, wantVal)
	}
	return nil
}

// TlsRoundTrip produces and consumes a single record over a TLS-only
// external listener with TlsClientSecurity holding the CA's truststore.
// Exercises: SAN matching the bootstrap address, kgo dialer + TLS, broker
// SSL listener, end-to-end encryption.
func (t *Tests) TlsRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, ts, tsPwd, err := freshTlsCluster(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create tls cluster: %w", err)
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
	if gotKey != wantKey {
		return fmt.Errorf("key mismatch: want %q, got %q", wantKey, gotKey)
	}
	if gotVal != wantVal {
		return fmt.Errorf("value mismatch: want %q, got %q", wantVal, gotVal)
	}
	return nil
}

// TlsClusterStarts forces the lazy Cluster construction to run under
// TlsServerSecurity and confirms BootstrapServers reports a non-empty,
// non-zero-port broker address. No client connection attempted — this
// proves caller's CA loads, leaf signing succeeds, the keystore mounts,
// and the broker doesn't crash on startup.
func (t *Tests) TlsClusterStarts(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, _, _, err := freshTlsCluster(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create tls cluster: %w", err)
	}
	bs, err := cluster.BootstrapServers(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap servers: %w", err)
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
