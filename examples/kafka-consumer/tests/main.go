// Package main is the kafka-consumer-example tests Dagger module. It exercises
// the runnable example under examples/kafka-consumer/ end to end:
//
//   - GoAppCi builds it through the z5labs GoApp archetype (fmt/vet/lint/test
//     -race + multi-arch build). This is the only +check — it runs in CI.
//   - MtlsAvroConsume / TlsAvroConsume stand up a TLS (or mTLS) Kafka cluster, a
//     TLS/mTLS Schema Registry, and an OpenTelemetry collector wired to Tempo/
//     Mimir/Loki, produce framed Avro records, run the example consumer against
//     the stack, and assert it both decoded the records and exported telemetry.
//
// The integration tests are BLOCKED by a known kafka-module bug — #147:
// SchemaRegistry.BindTo's advertised alias is not resolvable from a WithExec
// process (the service handle detaches when it rides on the cross-module
// SchemaRegistry object). MtlsAvroConsume fails at exactly `KafkaSchemaRegistry.
// bindTo`, so it is deliberately NOT a +check (it would be red-by-#147, not
// red-by-#150). It is kept runnable via `dagger call mtls-avro-consume` as a
// faithful reproduction of the end-to-end user experience that triggers #147,
// and should be promoted back to +check once #147 lands. See the example's
// README for details.
//
// The example source is loaded as a contextual argument (+defaultPath), so the
// +check function runs under `dagger check` with no CLI arguments.
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/tests/internal/dagger"
)

type Tests struct{}

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
func (t *Tests) GoAppCi(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["tests"]
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

// MtlsAvroConsume is the recommended-posture integration test: the whole stack
// runs with mutual TLS on both the broker and the Schema Registry hops.
//
// NOT a +check: it reproduces #147 (SchemaRegistry.BindTo alias unresolvable
// from WithExec) and fails at `KafkaSchemaRegistry.bindTo`. Run it on demand
// with `dagger call mtls-avro-consume`; promote back to +check once #147 lands.
//
// +cache="never"
func (t *Tests) MtlsAvroConsume(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["tests"]
	source *dagger.Directory,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return avroConsume(ctx, source, kafkaImageTag, true)
}

// TlsAvroConsume is the server-TLS (trust-only) variant, runnable on demand.
// Like MtlsAvroConsume it currently reproduces #147 and is not a +check.
//
// +cache="never"
func (t *Tests) TlsAvroConsume(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["tests"]
	source *dagger.Directory,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return avroConsume(ctx, source, kafkaImageTag, false)
}

// All runs the suite sequentially, for local `dagger call all`. CI runs only
// GoAppCi (the sole +check); the integration round-trip is blocked by #147.
func (t *Tests) All(
	ctx context.Context,
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["tests"]
	source *dagger.Directory,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	if err := t.GoAppCi(ctx, source); err != nil {
		return err
	}
	return t.MtlsAvroConsume(ctx, source, kafkaImageTag)
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

	// Build the example through the GoApp archetype (Builder needs no .git) and
	// run it in a minimal base image against the bound services.
	bin := dag.Z5Labs().GoApp(source).Builder().Binary()
	brokers, err := cluster.BootstrapServers(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap servers: %w", err)
	}
	srEndpoint, err := sr.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("registry endpoint: %w", err)
	}
	serviceName := "kafka-consumer-" + mark

	runner := dag.Container().From(alpineImage).
		WithFile("/usr/local/bin/consumer", bin).
		WithFile("/certs/truststore.p12", ts.Pkcs12()).
		WithSecretVariable("TRUSTSTORE_PASSWORD", ts.Password()).
		WithEnvVariable("BROKERS", strings.Join(brokers, ",")).
		WithEnvVariable("REGISTRY_URL", "https://"+srEndpoint).
		WithEnvVariable("TRUSTSTORE", "/certs/truststore.p12").
		WithEnvVariable("TOPIC", topic).
		WithEnvVariable("GROUP", serviceName).
		WithEnvVariable("MAX_RECORDS", strconv.Itoa(recordCount)).
		WithEnvVariable("TIMEOUT", "90s").
		WithEnvVariable("OTEL_EXPORTER_OTLP_ENDPOINT", "http://col:4317").
		WithEnvVariable("OTEL_EXPORTER_OTLP_INSECURE", "true").
		WithEnvVariable("OTEL_SERVICE_NAME", serviceName)
	if mtls {
		consumerKs, consumerPwd, err := issueClientKeystore(ctx, ca, "kafka-consumer")
		if err != nil {
			return err
		}
		runner = runner.
			WithFile("/certs/keystore.p12", consumerKs).
			WithSecretVariable("KEYSTORE_PASSWORD", consumerPwd).
			WithEnvVariable("KEYSTORE", "/certs/keystore.p12")
	}
	runner = cluster.BindBrokers(runner)
	runner = sr.BindTo(runner)
	runner = runner.WithServiceBinding("col", col.Service())

	out, err := runner.WithExec([]string{"consumer"}).Stdout(ctx)
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
