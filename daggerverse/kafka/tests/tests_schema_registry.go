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
// a fresh cluster, then exercise register → lookup-by-id →
// lookup-latest-by-subject → list-subjects → set/get-compatibility →
// delete against it — covering every SchemaRegistryClient operation.
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

	latest := client.LookupLatestBySubject(subject)
	latestSubject, err := latest.Subject(ctx)
	if err != nil {
		return fmt.Errorf("lookup latest by subject: %w", err)
	}
	if latestSubject != subject {
		return fmt.Errorf("lookup-latest subject mismatch: want %q, got %q", subject, latestSubject)
	}
	latestVersion, err := latest.Version(ctx)
	if err != nil {
		return fmt.Errorf("read latest version: %w", err)
	}
	if latestVersion != 1 {
		return fmt.Errorf("lookup-latest version mismatch: want 1, got %d", latestVersion)
	}
	latestID, err := latest.SchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read latest schema id: %w", err)
	}
	if latestID != id {
		return fmt.Errorf("lookup-latest schemaID mismatch: want %d, got %d", id, latestID)
	}
	latestDef, err := latest.Definition(ctx)
	if err != nil {
		return fmt.Errorf("read latest definition: %w", err)
	}
	if latestDef == "" {
		return fmt.Errorf("expected lookup-latest to return a non-empty schema definition")
	}
	latestType, err := latest.SchemaType(ctx)
	if err != nil {
		return fmt.Errorf("read latest schema type: %w", err)
	}
	if latestType != "AVRO" {
		return fmt.Errorf("lookup-latest schemaType mismatch: want AVRO, got %q", latestType)
	}

	subjects, err := client.ListSubjects(ctx)
	if err != nil {
		return fmt.Errorf("list subjects: %w", err)
	}
	if !contains(subjects, subject) {
		return fmt.Errorf("expected subject %q in %v after register", subject, subjects)
	}

	if err := client.SetCompatibility(ctx, subject, "BACKWARD"); err != nil {
		return fmt.Errorf("set compatibility: %w", err)
	}
	level, err := client.GetCompatibility(ctx, subject)
	if err != nil {
		return fmt.Errorf("get compatibility: %w", err)
	}
	if level != "BACKWARD" {
		return fmt.Errorf("compatibility round-trip mismatch: want BACKWARD, got %q", level)
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

// ApicurioSchemaRegistryRegisterLookupRoundTrip is the PLAINTEXT happy-path
// test for Kafka.ApicurioSchemaRegistry: stand an
// apicurio-registry-kafkasql up next to a fresh cluster, then exercise
// register → lookup-by-id → lookup-latest-by-subject → list-subjects →
// set/get-compatibility → delete against its Confluent-compatible REST
// surface — mirroring SchemaRegistryRegisterLookupRoundTrip to prove the
// shared *SchemaRegistryClient drives Apicurio unchanged.
func (t *Tests) ApicurioSchemaRegistryRegisterLookupRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	sr := dag.Kafka().ApicurioSchemaRegistry(cluster)
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

	latest := client.LookupLatestBySubject(subject)
	latestSubject, err := latest.Subject(ctx)
	if err != nil {
		return fmt.Errorf("lookup latest by subject: %w", err)
	}
	if latestSubject != subject {
		return fmt.Errorf("lookup-latest subject mismatch: want %q, got %q", subject, latestSubject)
	}
	latestVersion, err := latest.Version(ctx)
	if err != nil {
		return fmt.Errorf("read latest version: %w", err)
	}
	if latestVersion != 1 {
		return fmt.Errorf("lookup-latest version mismatch: want 1, got %d", latestVersion)
	}
	latestID, err := latest.SchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read latest schema id: %w", err)
	}
	if latestID != id {
		return fmt.Errorf("lookup-latest schemaID mismatch: want %d, got %d", id, latestID)
	}
	latestDef, err := latest.Definition(ctx)
	if err != nil {
		return fmt.Errorf("read latest definition: %w", err)
	}
	if latestDef == "" {
		return fmt.Errorf("expected lookup-latest to return a non-empty schema definition")
	}
	latestType, err := latest.SchemaType(ctx)
	if err != nil {
		return fmt.Errorf("read latest schema type: %w", err)
	}
	if latestType != "AVRO" {
		return fmt.Errorf("lookup-latest schemaType mismatch: want AVRO, got %q", latestType)
	}

	subjects, err := client.ListSubjects(ctx)
	if err != nil {
		return fmt.Errorf("list subjects: %w", err)
	}
	if !contains(subjects, subject) {
		return fmt.Errorf("expected subject %q in %v after register", subject, subjects)
	}

	if err := client.SetCompatibility(ctx, subject, "BACKWARD"); err != nil {
		return fmt.Errorf("set compatibility: %w", err)
	}
	level, err := client.GetCompatibility(ctx, subject)
	if err != nil {
		return fmt.Errorf("get compatibility: %w", err)
	}
	if level != "BACKWARD" {
		return fmt.Errorf("compatibility round-trip mismatch: want BACKWARD, got %q", level)
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

// KarapaceSchemaRegistryRegisterLookupRoundTrip is the PLAINTEXT happy-path
// test for Kafka.KarapaceSchemaRegistry: stand a Karapace service up next to
// a fresh cluster, then exercise register → lookup-by-id →
// lookup-latest-by-subject → list-subjects → set/get-compatibility → delete
// against its Confluent-compatible REST surface — mirroring
// SchemaRegistryRegisterLookupRoundTrip to prove the shared
// *SchemaRegistryClient drives Karapace unchanged.
func (t *Tests) KarapaceSchemaRegistryRegisterLookupRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	sr := dag.Kafka().KarapaceSchemaRegistry(cluster)
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

	latest := client.LookupLatestBySubject(subject)
	latestSubject, err := latest.Subject(ctx)
	if err != nil {
		return fmt.Errorf("lookup latest by subject: %w", err)
	}
	if latestSubject != subject {
		return fmt.Errorf("lookup-latest subject mismatch: want %q, got %q", subject, latestSubject)
	}
	latestVersion, err := latest.Version(ctx)
	if err != nil {
		return fmt.Errorf("read latest version: %w", err)
	}
	if latestVersion != 1 {
		return fmt.Errorf("lookup-latest version mismatch: want 1, got %d", latestVersion)
	}
	latestID, err := latest.SchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read latest schema id: %w", err)
	}
	if latestID != id {
		return fmt.Errorf("lookup-latest schemaID mismatch: want %d, got %d", id, latestID)
	}
	latestDef, err := latest.Definition(ctx)
	if err != nil {
		return fmt.Errorf("read latest definition: %w", err)
	}
	if latestDef == "" {
		return fmt.Errorf("expected lookup-latest to return a non-empty schema definition")
	}
	latestType, err := latest.SchemaType(ctx)
	if err != nil {
		return fmt.Errorf("read latest schema type: %w", err)
	}
	if latestType != "AVRO" {
		return fmt.Errorf("lookup-latest schemaType mismatch: want AVRO, got %q", latestType)
	}

	subjects, err := client.ListSubjects(ctx)
	if err != nil {
		return fmt.Errorf("list subjects: %w", err)
	}
	if !contains(subjects, subject) {
		return fmt.Errorf("expected subject %q in %v after register", subject, subjects)
	}

	if err := client.SetCompatibility(ctx, subject, "BACKWARD"); err != nil {
		return fmt.Errorf("set compatibility: %w", err)
	}
	level, err := client.GetCompatibility(ctx, subject)
	if err != nil {
		return fmt.Errorf("get compatibility: %w", err)
	}
	if level != "BACKWARD" {
		return fmt.Errorf("compatibility round-trip mismatch: want BACKWARD, got %q", level)
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
