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
//                          shared roundTripBinaryOn).
//   - tests_apache.go    — ApacheCluster (apache/kafka JVM) cluster helpers
//                          + the three Apache-JVM round-trip tests.
//   - tests_confluent.go — ConfluentCluster (confluentinc/cp-kafka) cluster
//                          helpers + the three cp-kafka round-trip tests.
//   - tests_redpanda.go  — RedpandaCluster (redpandadata/redpanda) cluster
//                          helpers + the two Redpanda round-trip tests.
package main

import (
	"context"
	"fmt"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every kafka round-trip test in parallel. Tests are grouped by
// security shape: shared-cluster eligible tests (read-only or topic-scoped
// writes that don't disturb other tests) reuse one ApacheNativeCluster per
// shape — sharedPlaintext / sharedTls / sharedMtls — constructed eagerly at
// the top of All and torn down via deferred Stop. Topology-divergent or
// state-mutating tests (multi-broker, multi-controller, RF=2 TLS) keep
// owning their own cluster via the freshCluster* helpers. The per-distro
// tests (Apache JVM, Confluent, Redpanda) likewise keep their own fresh
// clusters since they exist to exercise a distinct distro image.
//
// Sequencing: a single flat par.New() pool. Same-shape shared-cluster tests
// share the same *dagger.KafkaCluster pointer so the engine collapses their
// boots to one. Fresh-cluster tests of any shape spawn distinct clusterIds —
// distinct services, no lifetime race with shared instances. The no-broker
// test runs concurrently with anything.
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
// parallel caps how many tests run concurrently inside this suite. Defaults
// to 4 — the AC's stability minimum on the trace-recording host once cluster
// sharing reduces the number of concurrent broker services. Pass 0 to fan
// out every test with no limit, or any positive integer for a specific cap.
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
	// +default=4
	parallel int,
) error {
	sharedPlaintext, err := sharedPlaintextCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create shared plaintext cluster: %w", err)
	}
	defer sharedPlaintext.Stop(ctx)

	sharedTls, sharedTlsTs, sharedTlsTsPwd, err := sharedTlsCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create shared tls cluster: %w", err)
	}
	defer sharedTls.Stop(ctx)

	sharedMtls, sharedMtlsTs, sharedMtlsTsPwd, sharedMtlsKs, sharedMtlsKsPwd, err := sharedMtlsCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create shared mtls cluster: %w", err)
	}
	defer sharedMtls.Stop(ctx)

	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("PlaintextSecurityProfilesAreNonNil", t.PlaintextSecurityProfilesAreNonNil)
	jobs = jobs.WithJob("SingleNodeClusterStarts", func(ctx context.Context) error {
		return singleNodeClusterStartsOn(ctx, sharedPlaintext)
	})
	jobs = jobs.WithJob("ClusterClientCanListTopicsOnFreshCluster", func(ctx context.Context) error {
		return clusterClientCanListTopicsOnFreshClusterOn(ctx, sharedPlaintext)
	})
	jobs = jobs.WithJob("CreateAndDeleteTopicRoundTrip", func(ctx context.Context) error {
		return createAndDeleteTopicRoundTripOn(ctx, sharedPlaintext)
	})
	jobs = jobs.WithJob("ProduceConsumeRoundTripRaw", func(ctx context.Context) error {
		return produceConsumeRoundTripRawOn(ctx, sharedPlaintext)
	})
	jobs = jobs.WithJob("ProduceConsumeRoundTripHex", func(ctx context.Context) error {
		return roundTripBinaryOn(ctx, sharedPlaintext, "hex", "deadbeef", "00010203fffefdfc")
	})
	jobs = jobs.WithJob("ProduceConsumeRoundTripBase64", func(ctx context.Context) error {
		return roundTripBinaryOn(ctx, sharedPlaintext, "base64", "3q2+7w==", "AAECA//+/fw=")
	})
	jobs = jobs.WithJob("ProduceRejectsUnknownEncoding", func(ctx context.Context) error {
		return produceRejectsUnknownEncodingOn(ctx, sharedPlaintext)
	})
	jobs = jobs.WithJob("PropertiesFileContainsBootstrapAndSecurityProtocol", func(ctx context.Context) error {
		return propertiesFileContainsBootstrapAndSecurityProtocolOn(ctx, sharedPlaintext)
	})
	jobs = jobs.WithJob("BindBrokersExposesBothListeners", func(ctx context.Context) error {
		return t.BindBrokersExposesBothListeners(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("DedicatedControllerAndBrokerProduceConsume", func(ctx context.Context) error {
		return roundTripBinaryOn(ctx, sharedPlaintext, "raw", "k", "v")
	})
	jobs = jobs.WithJob("OneControllerTwoBrokersReplicationFactorTwo", func(ctx context.Context) error {
		return t.OneControllerTwoBrokersReplicationFactorTwo(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("MultiControllerIsRejected", func(ctx context.Context) error {
		return t.MultiControllerIsRejected(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("AutoCreateTopicsDisabled", func(ctx context.Context) error {
		return autoCreateTopicsDisabledOn(ctx, sharedPlaintext)
	})
	jobs = jobs.WithJob("ConsumerGroupOnSingleBrokerWorks", func(ctx context.Context) error {
		return consumerGroupOnSingleBrokerWorksOn(ctx, sharedPlaintext)
	})
	jobs = jobs.WithJob("TlsClusterStarts", func(ctx context.Context) error {
		return tlsClusterStartsOn(ctx, sharedTls)
	})
	jobs = jobs.WithJob("TlsRoundTrip", func(ctx context.Context) error {
		return tlsRoundTripOn(ctx, sharedTls, sharedTlsTs, sharedTlsTsPwd)
	})
	jobs = jobs.WithJob("TlsClientWithWrongCaFails", func(ctx context.Context) error {
		return tlsClientWithWrongCaFailsOn(ctx, sharedTls)
	})
	jobs = jobs.WithJob("InternalListenersAreEncrypted", func(ctx context.Context) error {
		return t.InternalListenersAreEncrypted(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("MtlsRoundTrip", func(ctx context.Context) error {
		return mtlsRoundTripOn(ctx, sharedMtls, sharedMtlsTs, sharedMtlsTsPwd, sharedMtlsKs, sharedMtlsKsPwd)
	})
	jobs = jobs.WithJob("MtlsRequiresClientCert", func(ctx context.Context) error {
		return mtlsRequiresClientCertOn(ctx, sharedMtls, sharedMtlsTs, sharedMtlsTsPwd)
	})
	jobs = jobs.WithJob("PropertiesFileContainsTlsSettings", func(ctx context.Context) error {
		return propertiesFileContainsTlsSettingsOn(ctx, sharedTls, sharedTlsTs, sharedTlsTsPwd)
	})
	jobs = jobs.WithJob("PropertiesFileContainsMtlsSettings", func(ctx context.Context) error {
		return propertiesFileContainsMtlsSettingsOn(ctx, sharedMtls, sharedMtlsTs, sharedMtlsTsPwd, sharedMtlsKs, sharedMtlsKsPwd)
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
