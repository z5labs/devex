package main

import (
	"context"
	"fmt"

	"dagger/ci/internal/dagger"
)

// RunAgainst is the example's run-configuration chain. It codifies "where do I
// run the consumer" the way an IDE run configuration would, but in Dagger so it
// is reproducible and shareable across a team. Local() stands the whole stack up
// on the local engine (a docker-compose replacement); a future NonProd() will
// point the same consumer container at an already-deployed non-prod environment
// instead of spinning services up.
type RunAgainst struct {
	// Source is the example source tree — the app that gets built and run.
	Source *dagger.Directory
}

// RunAgainst starts the run-configuration chain. The example source is loaded as
// a contextual argument so `dagger call run-against local` needs no arguments.
func (c *Ci) RunAgainst(
	// +defaultPath="/examples/kafka-consumer"
	// +ignore=["ci"]
	source *dagger.Directory,
) *RunAgainst {
	return &RunAgainst{Source: source}
}

// Local stands up a complete local stack — a single-node Apache Kafka broker
// (KRaft) with a *separate* Confluent Schema Registry (server-TLS), plus an
// OpenTelemetry collector fronting Tempo/Mimir/Loki — produces framed Avro
// records onto a topic, then builds and runs the example consumer against it,
// returning the consumer's stdout. It is meant to be a Dagger-native replacement
// for a docker-compose "up": one command brings up every dependency and the app,
// wired together, runnable from anywhere in the example.
//
// This models exactly how a developer would run the example locally, and is the
// canonical reproduction to reference when planning the fix for kafka-module bug
// #147. It stands up the same topology as the mtls/tls-avro-consume checks —
// Apache Kafka plus a standalone Confluent Schema Registry — precisely so the
// registry is its own service reached via SchemaRegistry.BindTo. That BindTo
// advertises the registry's own DNS alias, and the consumer's WithExec fails at
// hosts-file setup with "lookup csr-… no such host": the registry hostname, which
// names exactly the detached-ModuleObject handle #147 is about. (The previous
// Redpanda build used a *bundled* registry sharing the broker host, so it instead
// failed on the broker alias "redpanda-1-… no such host" — obscuring which hop
// #147 actually breaks.) So Local currently FAILS by design; once #147 lands it
// will run green. See the example README for the full write-up.
//
// The wire + registry hops are server-TLS (trust-only) to keep a local run
// simple; the mutual-TLS posture is exercised by the mtls-avro-consume check.
// Local does not assert on telemetry — it just returns the consumer's stdout —
// but the observability backends run so a developer (or a future dashboard) can
// point a UI at them.
//
// +cache="never"
func (ra *RunAgainst) Local(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) (string, error) {
	mark, err := marker(ctx)
	if err != nil {
		return "", err
	}

	// One CA signs the broker + registry server leaves; the consumer verifies
	// both hops against its truststore. Server-TLS only — no client leaf issued.
	ca, err := freshCa(ctx, "kc-local")
	if err != nil {
		return "", fmt.Errorf("mint CA: %w", err)
	}
	caKs := ca.KeyStore()
	ts := ca.TrustStore()

	clusterID, err := newClusterId(ctx)
	if err != nil {
		return "", err
	}
	k := dag.Kafka()

	serverSec := k.TLSServerSecurity(caKs.Pkcs12(), caKs.Password())
	cluster := k.ApacheNativeCluster(clusterID, serverSec, dagger.KafkaApacheNativeClusterOpts{
		Tag:     kafkaImageTag,
		Brokers: 1,
	})
	defer cluster.Stop(ctx)

	// A separate Confluent Schema Registry container (its own csr-… service host),
	// reached by the external consumer via SchemaRegistry.BindTo — exactly the
	// cross-module service handle that detaches under #147.
	srSec := k.TLSSchemaRegistrySecurity(caKs.Pkcs12(), caKs.Password())
	sr := k.ConfluentSchemaRegistry(cluster, srSec)
	defer sr.Stop(ctx)
	registryClientSec := k.TLSSchemaRegistryClientSecurity(ts.Pkcs12(), ts.Password())
	wireClientSec := k.TLSClientSecurity(ts.Pkcs12(), ts.Password())

	// Register the writer schema and produce recordCount framed Avro records via
	// the kafka module's Avro producer (module-side register/produce works; the
	// external app's BindTo hop is what #147 breaks).
	topic, err := randomTopicName(ctx)
	if err != nil {
		return "", err
	}
	subject := topic + "-value"
	id, err := sr.Client(registryClientSec).RegisterSchema(ctx, subject, avroSchema,
		dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{SchemaType: "AVRO"})
	if err != nil {
		return "", fmt.Errorf("register schema: %w", err)
	}
	if id <= 0 {
		return "", fmt.Errorf("registry returned non-positive schema id %d", id)
	}

	producer := cluster.Client(wireClientSec)
	if err := producer.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return "", fmt.Errorf("create topic: %w", err)
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
			return "", fmt.Errorf("produce record %d: %w", i, err)
		}
	}

	// OpenTelemetry collector fanning traces/metrics/logs into Tempo/Mimir/Loki,
	// so the local stack mirrors production observability.
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
	base := dag.Z5Labs().GoApp(ra.Source).Builder().Container()
	brokers, err := cluster.BootstrapServers(ctx)
	if err != nil {
		return "", fmt.Errorf("bootstrap servers: %w", err)
	}
	if len(brokers) == 0 {
		return "", fmt.Errorf("apache kafka cluster returned no bootstrap servers")
	}
	srEndpoint, err := sr.Endpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("registry endpoint: %w", err)
	}
	serviceName := "kafka-consumer-local-" + mark

	runner := consumerRunner(consumerRunnerConfig{
		base:         base,
		brokers:      brokers,
		registryURL:  "https://" + srEndpoint,
		trustStore:   ts.Pkcs12(),
		trustStorePw: ts.Password(),
		topic:        topic,
		group:        serviceName,
		serviceName:  serviceName,
		maxRecords:   recordCount,
		timeout:      "90s",
		otelEndpoint: "http://col:4317",
	})
	runner = cluster.BindBrokers(runner)
	// BindTo advertises the registry's own csr-… alias — the #147 detach point.
	runner = sr.BindTo(runner)
	runner = runner.WithServiceBinding("col", col.Service())

	out, err := runner.WithExec([]string{}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("run consumer: %w", err)
	}
	return out, nil
}
