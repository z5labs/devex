// Tests for the kafka daggerverse module. Each test is exposed as a standalone
// dagger function so it can be invoked individually during TDD; All wires them
// up for grouped, parallel execution under `dagger call all`.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - main.go            — Tests struct, the All() orchestrator, and the four
//                          per-distro group functions (nativeTests,
//                          apacheJVMTests, confluentTests, redpandaTests).
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
//                          helpers + the two Redpanda Kafka-wire round-trip
//                          tests, the PLAINTEXT + TLS bundled-Schema-
//                          Registry round-trips, and the bundled-registry
//                          Stop-is-a-no-op lifecycle test.
//   - tests_schema_registry.go — ConfluentSchemaRegistry tests: the
//                          register/lookup/delete round-trip and the
//                          non-PLAINTEXT-cluster rejection.
package main

import (
	"context"
	"fmt"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every kafka round-trip test, grouped by cluster distro. The
// suite is split into four intermediate group functions — nativeTests,
// apacheJVMTests, confluentTests, redpandaTests — and parallelised at
// two levels: All fans the four groups out across one par pool, and each
// group in turn fans its own tests out across a nested par pool. Both
// pools use the same parallel cap, so peak concurrency is bounded by
// parallel groups each running up to parallel tests.
//
// Every group still boots only its own clusters and tears them down on
// return, so cluster lifetime stays bounded to the group — the fast
// Redpanda cluster is gone the moment redpandaTests returns rather than
// idling under a deferred Stop. All four groups run regardless of
// earlier failures — the par pool aggregates their errors — so one red
// group never hides another's results.
//
// nativeTests carries the bulk of the suite. It owns the three shared
// ApacheNativeClusters — sharedPlaintext / sharedTls / sharedMtls, one
// per security shape — plus the topology-divergent native tests that
// still spawn their own fresh clusters (multi-broker, multi-controller,
// RF=2 TLS, encrypted internal listeners). Same-shape shared-cluster
// tests share one *dagger.KafkaCluster pointer so the engine collapses
// their boots to a single service.
//
// apacheJVMTests, confluentTests and redpandaTests each exercise a
// distinct distro image and keep their own per-test fresh clusters.
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
// parallel is the concurrency cap applied at both levels — how many
// groups run at once and how many tests run at once within each group.
// Defaults to 4. Pass 0 to fan out with no limit at either level, or any
// positive integer for a specific cap.
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
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("nativeTests", func(ctx context.Context) error {
		return t.nativeTests(ctx, kafkaImageTag, parallel)
	})
	jobs = jobs.WithJob("apacheJVMTests", func(ctx context.Context) error {
		return t.apacheJVMTests(ctx, kafkaImageTag, parallel)
	})
	jobs = jobs.WithJob("confluentTests", func(ctx context.Context) error {
		return t.confluentTests(ctx, confluentImageTag, parallel)
	})
	jobs = jobs.WithJob("redpandaTests", func(ctx context.Context) error {
		return t.redpandaTests(ctx, redpandaImageTag, parallel)
	})
	jobs = jobs.WithJob("schemaRegistryTests", func(ctx context.Context) error {
		return t.schemaRegistryTests(ctx, kafkaImageTag, parallel)
	})

	return jobs.Run(ctx)
}

// schemaRegistryTests runs the Kafka.ConfluentSchemaRegistry tests as one
// group. Each test owns the cluster (and, for the round-trip, the
// cp-schema-registry service) it boots, so the group's only lifetime
// guarantee is that both are torn down once it returns.
func (t *Tests) schemaRegistryTests(ctx context.Context, kafkaImageTag string, parallel int) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("SchemaRegistryRegisterLookupRoundTrip", func(ctx context.Context) error {
		return t.SchemaRegistryRegisterLookupRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("SchemaRegistryRejectsNonPlaintextCluster", func(ctx context.Context) error {
		return t.SchemaRegistryRejectsNonPlaintextCluster(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ApicurioSchemaRegistryRegisterLookupRoundTrip", func(ctx context.Context) error {
		return t.ApicurioSchemaRegistryRegisterLookupRoundTrip(ctx, kafkaImageTag)
	})

	return jobs.Run(ctx)
}

// nativeTests runs every apache/kafka-native test as one group. It boots
// the three shared ApacheNativeClusters up front, fans the shared-cluster
// and fresh-cluster native tests across a par pool capped at parallel,
// and tears the shared clusters down on return — before All advances to
// the per-distro groups.
func (t *Tests) nativeTests(ctx context.Context, kafkaImageTag string, parallel int) error {
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

	return jobs.Run(ctx)
}

// apacheJVMTests runs the three apache/kafka JVM-image round-trip tests.
// Each test owns a fresh ApacheCluster, so the group holds no shared
// clusters of its own — its only lifetime guarantee is that all three
// JVM clusters are gone once it returns.
func (t *Tests) apacheJVMTests(ctx context.Context, kafkaImageTag string, parallel int) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("ApacheClusterProduceListTopicsRoundTrip", func(ctx context.Context) error {
		return t.ApacheClusterProduceListTopicsRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ApacheClusterTlsRoundTrip", func(ctx context.Context) error {
		return t.ApacheClusterTlsRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("ApacheClusterMtlsRoundTrip", func(ctx context.Context) error {
		return t.ApacheClusterMtlsRoundTrip(ctx, kafkaImageTag)
	})

	return jobs.Run(ctx)
}

// confluentTests runs the three confluentinc/cp-kafka round-trip tests.
// Each test owns a fresh ConfluentCluster; the group exists so cp-kafka's
// (slow) clusters are all torn down before redpandaTests starts.
func (t *Tests) confluentTests(ctx context.Context, confluentImageTag string, parallel int) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("ConfluentClusterProduceListTopicsRoundTrip", func(ctx context.Context) error {
		return t.ConfluentClusterProduceListTopicsRoundTrip(ctx, confluentImageTag)
	})
	jobs = jobs.WithJob("ConfluentClusterTlsRoundTrip", func(ctx context.Context) error {
		return t.ConfluentClusterTlsRoundTrip(ctx, confluentImageTag)
	})
	jobs = jobs.WithJob("ConfluentClusterMtlsRoundTrip", func(ctx context.Context) error {
		return t.ConfluentClusterMtlsRoundTrip(ctx, confluentImageTag)
	})

	return jobs.Run(ctx)
}

// redpandaTests runs the redpandadata/redpanda round-trip tests — the two
// Kafka-wire round-trips, the PLAINTEXT and TLS bundled-Schema-Registry
// round-trips, and the bundled-registry Stop-is-a-no-op lifecycle test.
// Redpanda boots and tears down far faster than the JVM-based distros,
// so running it as its own group means its clusters never linger past
// this function's return.
func (t *Tests) redpandaTests(ctx context.Context, redpandaImageTag string, parallel int) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("RedpandaClusterProduceListTopicsRoundTrip", func(ctx context.Context) error {
		return t.RedpandaClusterProduceListTopicsRoundTrip(ctx, redpandaImageTag)
	})
	jobs = jobs.WithJob("RedpandaClusterTlsRoundTrip", func(ctx context.Context) error {
		return t.RedpandaClusterTlsRoundTrip(ctx, redpandaImageTag)
	})
	jobs = jobs.WithJob("RedpandaSchemaRegistryRegisterLookupRoundTrip", func(ctx context.Context) error {
		return t.RedpandaSchemaRegistryRegisterLookupRoundTrip(ctx, redpandaImageTag)
	})
	jobs = jobs.WithJob("RedpandaSchemaRegistryTlsRegisterLookupRoundTrip", func(ctx context.Context) error {
		return t.RedpandaSchemaRegistryTlsRegisterLookupRoundTrip(ctx, redpandaImageTag)
	})
	jobs = jobs.WithJob("RedpandaSchemaRegistryBundledStopIsNoOp", func(ctx context.Context) error {
		return t.RedpandaSchemaRegistryBundledStopIsNoOp(ctx, redpandaImageTag)
	})

	return jobs.Run(ctx)
}
