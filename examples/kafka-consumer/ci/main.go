// Package main is the kafka-consumer example's Dagger module. It is rooted at
// the example root (dagger-module.toml lives at examples/kafka-consumer/, source "ci")
// so `dagger call` works from anywhere in the example, and it codifies the
// example's run configuration alongside its checks:
//
//   - RunAgainst().Local() stands up the whole stack locally (a single-node
//     Apache Kafka broker plus a separate Confluent Schema Registry over TLS, and
//     an OpenTelemetry collector) and runs the example consumer against it — a
//     Dagger-native replacement for make+compose.
//
// It also exercises the runnable example under examples/kafka-consumer/ end to end:
//
//   - GoCi runs it through the z5labs Go chain's standardized check stages:
//     gofmt, go vet, golangci-lint, and `go test ./...` with the race detector.
//   - MtlsAvroConsume / TlsAvroConsume stand up a TLS (or mTLS) Apache Kafka
//     cluster, a Confluent Schema Registry, and an OpenTelemetry collector wired
//     to Tempo/Mimir/Loki, produce framed Avro records, run the example consumer
//     against the stack, and assert it both decoded the records and exported
//     telemetry.
//
// On v0.21.x the end-to-end integration was blocked by #147: a service given a
// custom hostname is namespaced into the DNS domain of whichever module first
// *starts* it, so the consumer died at hosts-file setup with `lookup <alias> … no
// such host` on either the Cluster.BindBrokers or the SchemaRegistry.BindTo hop.
// dagger/dagger#13751 fixes it in v1.0.0-beta.12 and later, and this module is
// pinned to v1.0.0-beta.15, so both binds resolve.
//
// MtlsAvroConsume is still a +check that is RED by design, for a different
// reason: it gets past the binds and consumes every record, then fails in
// assertTelemetry — which had never executed before, because #147 stopped every
// run short of it (#441). GoCi (the build check) stays green. TlsAvroConsume
// shares that telemetry assertion and is runnable on demand; RunAgainst().Local()
// asserts no telemetry and passes. See the example's README for details.
//
// The example source is loaded as a contextual argument (+defaultPath), so the
// +check function runs under `dagger check` with no CLI arguments.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/kafka-consumer/internal/dagger"
)

type KafkaConsumer struct{}

// recordCount is how many framed Avro records the harness produces and the
// consumer is asked to decode before it flushes telemetry and exits.
const recordCount = 3

// avroSchema is the Avro writer schema registered for the test subject.
const avroSchema = `{"type":"record","name":"Event","namespace":"com.z5labs.devex.example","fields":[{"name":"message","type":"string"},{"name":"sequence","type":"long"}]}`

// GoCi runs the example through the z5labs Go chain's standardized check
// stages: gofmt, go vet, golangci-lint, and `go test ./...` with the race
// detector. Go.Ci needs no git metadata — only the App terminal does — but the
// loaded source still goes through gitFixture so this check and the
// integration checks compile the identical tree.
//
// +check
// +cache="never"
func (c *KafkaConsumer) GoCi(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["ci"]
	source *dagger.Directory,
) error {
	src, err := gitFixture(ctx, source, "main")
	if err != nil {
		return fmt.Errorf("gitFixture: %w", err)
	}
	if err := dag.Z5Labs().Go(src).Ci(ctx); err != nil {
		return fmt.Errorf("Go.Ci: %w", err)
	}
	return nil
}

// MtlsAvroConsume is the recommended-posture integration check: the whole stack
// runs with mutual TLS on both the broker and the Schema Registry hops.
//
// It is a +check that is RED by design: it consumes every record and then fails
// in assertTelemetry, an assertion #147 had always masked on v0.21.x (#441).
// Keeping this a +check makes CI a live tracker for that bug.
//
// +check
// +cache="never"
func (c *KafkaConsumer) MtlsAvroConsume(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["ci"]
	source *dagger.Directory,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return avroConsume(ctx, source, kafkaImageTag, true)
}

// TlsAvroConsume is the server-TLS (trust-only) variant, runnable on demand. It
// shares MtlsAvroConsume's assertTelemetry (#441) but is not a +check —
// MtlsAvroConsume is the single tracking check, to avoid a duplicate red.
//
// +cache="never"
func (c *KafkaConsumer) TlsAvroConsume(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["ci"]
	source *dagger.Directory,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return avroConsume(ctx, source, kafkaImageTag, false)
}

// All runs the suite sequentially, for local `dagger call all`. In CI, GoCi
// (build) and MtlsAvroConsume (integration) both run as +checks; MtlsAvroConsume
// is red until #441 is fixed.
func (c *KafkaConsumer) All(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["ci"]
	source *dagger.Directory,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	if err := c.GoCi(ctx, source); err != nil {
		return err
	}
	return c.MtlsAvroConsume(ctx, source, kafkaImageTag)
}

// avroConsume stands up the integration stack, produces framed Avro records,
// runs the example consumer against it, and asserts (a) it decoded and received
// the records and (b) its traces + metrics + logs reached the collector. When
// mtls is true both hops use mutual TLS; otherwise server-TLS (trust-only).
// There is never a plaintext hop.
func avroConsume(ctx context.Context, source *dagger.Directory, kafkaImageTag string, mtls bool) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	label := "kc-tls"
	if mtls {
		label = "kc-mtls"
	}

	// One CA signs the broker + registry server leaves and (for mTLS) the
	// client leaves. The consumer verifies both hops against its truststore.
	ca, err := freshCa(ctx, label)
	if err != nil {
		return fmt.Errorf("mint CA: %w", err)
	}
	caKs := ca.KeyStore()
	ts := ca.TrustStore()

	clusterID, err := newClusterId(ctx)
	if err != nil {
		return err
	}
	k := dag.Kafka()

	var (
		serverSec         *dagger.KafkaServerSecurity
		srSec             *dagger.KafkaSchemaRegistrySecurity
		registryClientSec *dagger.KafkaSchemaRegistryClientSecurity
		wireClientSec     *dagger.KafkaClientSecurity
	)
	if mtls {
		serverSec = k.MtlsServerSecurity(caKs.Pkcs12(), caKs.Password(), ts.Pkcs12(), ts.Password())
		srSec = k.MtlsSchemaRegistrySecurity(caKs.Pkcs12(), caKs.Password(), ts.Pkcs12(), ts.Password())
		restKs, restPwd, err := issueClientKeystore(ctx, ca, "sr-rest-client")
		if err != nil {
			return err
		}
		registryClientSec = k.MtlsSchemaRegistryClientSecurity(restKs, restPwd, ts.Pkcs12(), ts.Password())
		wireKs, wirePwd, err := issueClientKeystore(ctx, ca, "kafka-wire-client")
		if err != nil {
			return err
		}
		wireClientSec = k.MtlsClientSecurity(wireKs, wirePwd, ts.Pkcs12(), ts.Password())
	} else {
		serverSec = k.TLSServerSecurity(caKs.Pkcs12(), caKs.Password())
		srSec = k.TLSSchemaRegistrySecurity(caKs.Pkcs12(), caKs.Password())
		registryClientSec = k.TLSSchemaRegistryClientSecurity(ts.Pkcs12(), ts.Password())
		wireClientSec = k.TLSClientSecurity(ts.Pkcs12(), ts.Password())
	}

	cluster := k.ApacheNativeCluster(clusterID, serverSec, dagger.KafkaApacheNativeClusterOpts{
		Tag:     kafkaImageTag,
		Brokers: 1,
	})
	defer cluster.Stop(ctx)
	sr := k.ConfluentSchemaRegistry(cluster, srSec)
	defer sr.Stop(ctx)

	// Register the writer schema and produce recordCount framed Avro records
	// via the kafka module's Avro producer (the #141 TLS-registry path).
	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	subject := topic + "-value"
	id, err := sr.Client(registryClientSec).RegisterSchema(ctx, subject, avroSchema,
		dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{SchemaType: "AVRO"})
	if err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("registry returned non-positive schema id %d", id)
	}

	producer := cluster.Client(wireClientSec)
	if err := producer.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic: %w", err)
	}
	for i := 0; i < recordCount; i++ {
		value := fmt.Sprintf(`{"message":"hello-%s-%d","sequence":%d}`, mark, i, i)
		if err := producer.Produce(ctx, topic, "k", value, dagger.KafkaClientProduceOpts{
			ValueEncoding:    "raw",
			ValueSchemaID:    id,
			ValueSerializeAs: "AVRO",
			Registry:         sr,
			RegistrySecurity: registryClientSec,
		}); err != nil {
			return fmt.Errorf("produce record %d: %w", i, err)
		}
	}

	// OpenTelemetry collector fanning the three signals into Tempo (traces),
	// Mimir (metrics), and Loki (logs). No batch processor, so the collector
	// forwards promptly once the consumer flushes.
	tempo := dag.GrafanaStack().Tempo(dagger.GrafanaStackTempoOpts{Tag: tempoTag})
	mimir := dag.GrafanaStack().Mimir(dagger.GrafanaStackMimirOpts{Tag: mimirTag})
	loki := dag.GrafanaStack().Loki(dagger.GrafanaStackLokiOpts{Tag: lokiTag})
	o := dag.Otel()
	recv := o.OtlpReceiver("in")
	col := o.Core(dagger.OtelCoreOpts{Tag: collectorTag}).
		WithServiceBinding("tempo", tempo.Service()).
		WithServiceBinding("mimir", mimir.Service()).
		WithServiceBinding("loki", loki.Service()).
		WithPipeline(o.Pipeline("traces", "traces").WithReceiver(recv).WithExporter(o.OtlpExporter("tempo", "tempo:4317"))).
		WithPipeline(o.Pipeline("metrics", "metrics").WithReceiver(recv).WithExporter(o.OtlpHTTPExporter("mimir", "http://mimir:9009/otlp"))).
		WithPipeline(o.Pipeline("logs", "logs").WithReceiver(recv).WithExporter(o.OtlpHTTPExporter("loki", "http://loki:3100/otlp")))

	// Run the SAME image the z5labs Go chain's App terminal builds and
	// publishes, against the bound services. App stamps main.commit from HEAD,
	// so its source must be a git working tree — hence gitFixture. Only one
	// platform is built, and Container has to name that same platform.
	src, err := gitFixture(ctx, source, "main")
	if err != nil {
		return fmt.Errorf("gitFixture: %w", err)
	}
	base := dag.Z5Labs().Go(src).
		App("v0.0.0", dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{"linux/amd64"}}).
		Container("linux/amd64")
	brokers, err := cluster.BootstrapServers(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap servers: %w", err)
	}
	srEndpoint, err := sr.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("registry endpoint: %w", err)
	}
	serviceName := "kafka-consumer-" + mark

	// For mTLS the consumer also presents a client leaf on both hops.
	var (
		consumerKs  *dagger.File
		consumerPwd *dagger.Secret
	)
	if mtls {
		consumerKs, consumerPwd, err = issueClientKeystore(ctx, ca, "kafka-consumer")
		if err != nil {
			return err
		}
	}
	runner := consumerRunner(consumerRunnerConfig{
		base:         base,
		brokers:      brokers,
		registryURL:  "https://" + srEndpoint,
		trustStore:   ts.Pkcs12(),
		trustStorePw: ts.Password(),
		keyStore:     consumerKs,
		keyStorePw:   consumerPwd,
		topic:        topic,
		group:        serviceName,
		serviceName:  serviceName,
		maxRecords:   recordCount,
		timeout:      "90s",
		otelEndpoint: "http://col:4317",
	})
	runner = cluster.BindBrokers(runner)
	runner = sr.BindTo(runner)
	runner = runner.WithServiceBinding("col", col.Service())

	out, err := runner.WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("run consumer: %w", err)
	}
	// Functional assertion: the consumer resolved the schema and decoded every
	// produced record (its unique marker appears in the printed values).
	for i := 0; i < recordCount; i++ {
		want := fmt.Sprintf("hello-%s-%d", mark, i)
		if !strings.Contains(out, want) {
			return fmt.Errorf("consumer stdout missing decoded record %q:\n%s", want, out)
		}
	}

	// Telemetry assertion: traces + metrics + logs reached the collector.
	return assertTelemetry(ctx, tempo.Service(), mimir.Service(), loki.Service(), serviceName)
}
