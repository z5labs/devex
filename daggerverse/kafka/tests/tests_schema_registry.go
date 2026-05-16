package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"
)

// avroTestSchema is a minimal Avro record schema used by the Schema
// Registry round-trip test.
const avroTestSchema = `{"type":"record","name":"R","fields":[{"name":"x","type":"string"}]}`

// SchemaRegistryRegisterLookupRoundTrip is the PLAINTEXT happy-path test
// for Kafka.ConfluentSchemaRegistry: stand a cp-schema-registry up next to
// a fresh cluster, then exercise register → lookup-by-id → list-subjects →
// delete against it.
func (t *Tests) SchemaRegistryRegisterLookupRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	sr := dag.Kafka().ConfluentSchemaRegistry(cluster)
	defer sr.Stop(ctx)

	client := sr.Client()

	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	subject += "-value"

	id, err := client.RegisterSchema(ctx, subject, avroTestSchema, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "AVRO",
	})
	if err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("expected a positive schema id, got %d", id)
	}

	got := client.LookupSchemaByID(id)
	gotSubject, err := got.Subject(ctx)
	if err != nil {
		return fmt.Errorf("lookup schema by id: %w", err)
	}
	if gotSubject != subject {
		return fmt.Errorf("lookup-by-id subject mismatch: want %q, got %q", subject, gotSubject)
	}
	gotType, err := got.SchemaType(ctx)
	if err != nil {
		return fmt.Errorf("read schema type: %w", err)
	}
	if gotType != "AVRO" {
		return fmt.Errorf("lookup-by-id schemaType mismatch: want AVRO, got %q", gotType)
	}
	gotID, err := got.SchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read schema id: %w", err)
	}
	if gotID != id {
		return fmt.Errorf("lookup-by-id schemaID mismatch: want %d, got %d", id, gotID)
	}

	subjects, err := client.ListSubjects(ctx)
	if err != nil {
		return fmt.Errorf("list subjects: %w", err)
	}
	if !contains(subjects, subject) {
		return fmt.Errorf("expected subject %q in %v after register", subject, subjects)
	}

	deleted, err := client.DeleteSubject(ctx, subject)
	if err != nil {
		return fmt.Errorf("delete subject %q: %w", subject, err)
	}
	if len(deleted) == 0 {
		return fmt.Errorf("expected DeleteSubject to report at least one deleted version, got none")
	}
	return nil
}

// SchemaRegistryRejectsNonPlaintextCluster pins the PLAINTEXT-only contract
// of this story: ConfluentSchemaRegistry must reject a cluster whose client
// listener runs TLS rather than silently producing a broken registry. The
// constructor errors before any service boots, so the TLS cluster never
// has to start.
func (t *Tests) SchemaRegistryRejectsNonPlaintextCluster(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, _, _, err := freshTlsCluster(ctx, kafkaImageTag, 1)
	if err != nil {
		return fmt.Errorf("create tls cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	_, err = dag.Kafka().ConfluentSchemaRegistry(cluster).Endpoint(ctx)
	if err == nil {
		return fmt.Errorf("expected ConfluentSchemaRegistry to reject a non-PLAINTEXT cluster, got nil error")
	}
	if !strings.Contains(err.Error(), "PLAINTEXT") {
		return fmt.Errorf("expected rejection error to mention PLAINTEXT, got: %v", err)
	}
	return nil
}
