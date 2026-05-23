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

// All runs every kafka round-trip test as a convenience for local
// `dagger call all` invocations that want the entire suite in one shot.
// CI does NOT call All: each per-distro group below carries its own
// `+check` directive, so GH Actions schedules each onto its own runner
// in parallel — running All on top would double-bill the same work.
//
// kafkaImageTag picks the tag every spawned Apache cluster runs against —
// applied to both the apache/kafka-native image (ApacheNativeCluster) and
// the apache/kafka JVM image (ApacheCluster). confluentImageTag is the
// independent knob for the cp-kafka tests (Confluent Platform versioning
// is not aligned with Apache's release numbering). redpandaImageTag is
// the independent knob for the redpandadata/redpanda tests.
//
// parallel is the concurrency cap applied at both levels — how many
// groups run at once and how many tests run at once within each group.
// Defaults to 0 (unbounded). Pass any positive integer for a specific
// cap.
//
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
	// +default="8.2.0"
	confluentImageTag string,
	// +default="v26.1.7"
	redpandaImageTag string,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("Native", func(ctx context.Context) error {
		return t.Native(ctx, kafkaImageTag, parallel)
	})
	jobs = jobs.WithJob("ApacheJVM", func(ctx context.Context) error {
		return t.ApacheJVM(ctx, kafkaImageTag, parallel)
	})
	jobs = jobs.WithJob("Confluent", func(ctx context.Context) error {
		return t.Confluent(ctx, confluentImageTag, parallel)
	})
	jobs = jobs.WithJob("Redpanda", func(ctx context.Context) error {
		return t.Redpanda(ctx, redpandaImageTag, parallel)
	})
	jobs = jobs.WithJob("SchemaRegistry", func(ctx context.Context) error {
		return t.SchemaRegistry(ctx, kafkaImageTag, parallel)
	})

	return jobs.Run(ctx)
}

// SchemaRegistry runs the Schema Registry tests — ConfluentSchemaRegistry,
// ApicurioSchemaRegistry, and KarapaceSchemaRegistry — as one group. Each test
// owns the cluster (and, for the round-trips, the registry service) it boots,
// so the group's only lifetime guarantee is that both are torn down once it
// returns.
//
// +check
// +cache="session"
func (t *Tests) SchemaRegistry(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
	// +default=0
	parallel int,
) error {
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
	jobs = jobs.WithJob("KarapaceSchemaRegistryRegisterLookupRoundTrip", func(ctx context.Context) error {
		return t.KarapaceSchemaRegistryRegisterLookupRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("SchemaRegistryFramedProduceConsumeRoundTrip", func(ctx context.Context) error {
		return t.SchemaRegistryFramedProduceConsumeRoundTrip(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("SchemaRegistryPlaintextConsumeUnframed", func(ctx context.Context) error {
		return t.SchemaRegistryPlaintextConsumeUnframed(ctx, kafkaImageTag)
	})
	jobs = jobs.WithJob("SchemaRegistryJSONSerializeRejectsMalformedInput", func(ctx context.Context) error {
		return t.SchemaRegistryJSONSerializeRejectsMalformedInput(ctx)
	})
	jobs = jobs.WithJob("SchemaRegistryJSONFramedProduceConsumeRoundTrip", func(ctx context.Context) error {
		return t.SchemaRegistryJSONFramedProduceConsumeRoundTrip(ctx, kafkaImageTag)
	})

	return jobs.Run(ctx)
}

// Native runs every apache/kafka-native test as one group. It boots
// the three shared ApacheNativeClusters up front, fans the shared-cluster
// and fresh-cluster native tests across a par pool capped at parallel,
// and tears the shared clusters down on return.
//
// +check
// +cache="session"
func (t *Tests) Native(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
	// +default=0
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

	return jobs.Run(ctx)
}

// ApacheJVM runs the three apache/kafka JVM-image round-trip tests.
// Each test owns a fresh ApacheCluster, so the group holds no shared
// clusters of its own.
//
// +check
// +cache="session"
func (t *Tests) ApacheJVM(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
	// +default=0
	parallel int,
) error {
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

// Confluent runs the three confluentinc/cp-kafka round-trip tests.
// Each test owns a fresh ConfluentCluster.
//
// +check
// +cache="session"
func (t *Tests) Confluent(
	ctx context.Context,
	// +default="8.2.0"
	confluentImageTag string,
	// +default=0
	parallel int,
) error {
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

// Redpanda runs the redpandadata/redpanda round-trip tests — the two
// Kafka-wire round-trips, the PLAINTEXT and TLS bundled-Schema-Registry
// round-trips, and the bundled-registry Stop-is-a-no-op lifecycle test.
//
// +check
// +cache="session"
func (t *Tests) Redpanda(
	ctx context.Context,
	// +default="v26.1.7"
	redpandaImageTag string,
	// +default=0
	parallel int,
) error {
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
