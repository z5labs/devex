// Package main is the kafka-consumer-example `ci` Dagger module. It is rooted at
// the example root (dagger.json lives at examples/kafka-consumer/, source "ci")
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
//   - GoAppCi builds it through the z5labs GoApp archetype (fmt/vet/lint/test
//     -race + multi-arch build).
//   - MtlsAvroConsume / TlsAvroConsume stand up a TLS (or mTLS) Apache Kafka
//     cluster, a Confluent Schema Registry, and an OpenTelemetry collector wired
//     to Tempo/Mimir/Loki, produce framed Avro records, run the example consumer
//     against the stack, and assert it both decoded the records and exported
//     telemetry.
//
// The end-to-end integration is BLOCKED by a known kafka-module bug — #147:
// SchemaRegistry.BindTo's advertised alias is not resolvable from a WithExec
// process (the service handle detaches when it rides on the cross-module
// SchemaRegistry object). MtlsAvroConsume fails at exactly `KafkaSchemaRegistry.
// bindTo`, where the error names the registry's own DNS alias (`lookup csr-… no
// such host`). It is intentionally kept as a +check so CI carries a live red
// signal that tracks #147 — the check turns green once #147 lands. GoAppCi (the
// build check) stays green throughout. TlsAvroConsume and RunAgainst().Local()
// are the same reproduction in a server-TLS posture / run-configuration shape,
// runnable on demand. See the example's README for details.
//
// The example source is loaded as a contextual argument (+defaultPath), so the
// +check function runs under `dagger check` with no CLI arguments.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/ci/internal/dagger"
)

type Ci struct{}

// recordCount is how many framed Avro records the harness produces and the
// consumer is asked to decode before it flushes telemetry and exits.
const recordCount = 3

// avroSchema is the Avro writer schema registered for the test subject.
const avroSchema = `{"type":"record","name":"Event","namespace":"com.z5labs.devex.example","fields":[{"name":"message","type":"string"},{"name":"sequence","type":"long"}]}`

// GoAppCi builds the example through the z5labs GoApp archetype: fmt, vet,
// golangci-lint, `go test -race`, and a multi-arch build. GoApp.Ci requires a
// git working tree, so the loaded source is wrapped with gitFixture first.
//
// +check
// +cache="never"
func (c *Ci) GoAppCi(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["ci"]
	source *dagger.Directory,
) error {
	src, err := gitFixture(ctx, source, "main")
	if err != nil {
		return fmt.Errorf("gitFixture: %w", err)
	}
	if err := dag.Z5Labs().GoApp(src).Ci(ctx); err != nil {
		return fmt.Errorf("GoApp.Ci: %w", err)
	}
	return nil
}

// MtlsAvroConsume is the recommended-posture integration check: the whole stack
// runs with mutual TLS on both the broker and the Schema Registry hops.
//
// It is a +check that is currently RED by design: it reproduces #147
// (SchemaRegistry.BindTo alias unresolvable from WithExec) and fails at
// `KafkaSchemaRegistry.bindTo` with `lookup csr-… no such host` — the Confluent
// Schema Registry's own DNS alias. Keeping it a +check makes CI a live tracker
// for #147; it turns green automatically once #147 lands.
//
// +check
// +cache="never"
func (c *Ci) MtlsAvroConsume(
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
// reproduces the same #147 `bindTo` failure as MtlsAvroConsume but is not a
// +check — MtlsAvroConsume is the single tracking check, to avoid a duplicate red.
//
// +cache="never"
func (c *Ci) TlsAvroConsume(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["ci"]
	source *dagger.Directory,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return avroConsume(ctx, source, kafkaImageTag, false)
}

// All runs the suite sequentially, for local `dagger call all`. In CI, GoAppCi
// (build) and MtlsAvroConsume (integration) both run as +checks; MtlsAvroConsume
// is red until #147 lands.
func (c *Ci) All(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["ci"]
	source *dagger.Directory,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	if err := c.GoAppCi(ctx, source); err != nil {
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

	// Run the SAME container GoApp CI builds and publishes (Builder needs no
	// .git) against the bound services.
	base := dag.Z5Labs().GoApp(source).Builder().Container()
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
