// Tests for the kafka daggerverse module. Each test is exposed as a standalone
// dagger function so it can be invoked individually during TDD; All wires them
// up for parallel execution under `dagger call all`.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - main.go            — Tests struct + the All() orchestrator.
//   - helpers.go         — cross-cutting scaffolding shared across distros:
//                          newClusterId, freshCa, randHex, randomTopicName,
//                          contains.
//   - tests_native.go    — ApacheNativeCluster (apache/kafka-native) cluster
//                          helpers (freshCluster / freshTlsCluster /
//                          freshMtlsCluster) + every test that drives the
//                          GraalVM image (the bulk of the suite, including
//                          shared roundTripBinary).
//   - tests_apache.go    — ApacheCluster (apache/kafka JVM) cluster helpers
//                          + the three Apache-JVM round-trip tests.
//   - tests_confluent.go — ConfluentCluster (confluentinc/cp-kafka) cluster
//                          helpers + the three cp-kafka round-trip tests.
//   - tests_redpanda.go  — RedpandaCluster (redpandadata/redpanda) cluster
//                          helpers + the two Redpanda round-trip tests.
package main

import (
	"context"

	"github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every kafka round-trip test, capped at two concurrent jobs so
// the engine doesn't have dozens of cluster containers (controller +
// brokers per test) in flight at once on smaller CI runners.
//
// kafkaImageTag picks the tag every spawned Apache cluster runs against —
// applied to both the apache/kafka-native image (ApacheNativeCluster) and
// the apache/kafka JVM image (ApacheCluster) — so callers can verify the
// module against a newer Kafka release without first changing main.go.
// The default matches the Apache constructors' own default.
//
// confluentImageTag is the independent knob for the cp-kafka tests
// (ConfluentCluster). Confluent Platform versioning is not aligned with
// Apache's release numbering (CP 8.2.0 bundles Kafka 4.2.0), so this
// gets its own argument and its own default.
//
// redpandaImageTag is the independent knob for the redpandadata/redpanda
// tests (RedpandaCluster). Redpanda has its own release cadence with no
// alignment to Apache or Confluent numbering, so it gets its own argument
// and its own default.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
	// +default="8.2.0"
	confluentImageTag string,
	// +default="v26.1.7"
	redpandaImageTag string,
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
	jobs = jobs.WithJob("ApacheClusterProduceListTopicsRoundTrip", func(ctx context.Context) error {
		return t.ApacheClusterProduceListTopicsRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ApacheClusterTlsRoundTrip", func(ctx context.Context) error {
		return t.ApacheClusterTlsRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ApacheClusterMtlsRoundTrip", func(ctx context.Context) error {
		return t.ApacheClusterMtlsRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ConfluentClusterProduceListTopicsRoundTrip", func(ctx context.Context) error {
		return t.ConfluentClusterProduceListTopicsRoundTrip(ctx, confluentImageTag)
	})
	jobs = jobs.WithJob("ConfluentClusterTlsRoundTrip", func(ctx context.Context) error {
		return t.ConfluentClusterTlsRoundTrip(ctx, confluentImageTag)
	})
	jobs = jobs.WithJob("ConfluentClusterMtlsRoundTrip", func(ctx context.Context) error {
		return t.ConfluentClusterMtlsRoundTrip(ctx, confluentImageTag)
	})
	jobs = jobs.WithJob("RedpandaClusterProduceListTopicsRoundTrip", func(ctx context.Context) error {
		return t.RedpandaClusterProduceListTopicsRoundTrip(ctx, redpandaImageTag)
	})
	jobs = jobs.WithJob("RedpandaClusterTlsRoundTrip", func(ctx context.Context) error {
		return t.RedpandaClusterTlsRoundTrip(ctx, redpandaImageTag)
	})

	return jobs.Run(ctx)
}
