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

// SchemaRegistryFramedProduceConsumeRoundTrip exercises the data path of the
// Client with Confluent wire-format framing: register a schema to get an ID,
// produce a record whose value is framed with that ID, then consume with
// schemaRegistryAware=true and assert the parsed ID matches and the value
// bytes are stripped back to the original payload.
func (t *Tests) SchemaRegistryFramedProduceConsumeRoundTrip(
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

	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	topic := subject
	subject += "-value"

	id, err := sr.Client().RegisterSchema(ctx, subject, avroTestSchema, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "AVRO",
	})
	if err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("expected a positive schema id, got %d", id)
	}

	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	const wantKey, wantVal = "k", "hello-world"
	if err := client.Produce(ctx, topic, wantKey, wantVal, dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
		ValueSchemaID: id,
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	gotKey, err := records[0].Key(ctx)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	gotVal, err := records[0].Value(ctx)
	if err != nil {
		return fmt.Errorf("read value: %w", err)
	}
	gotValID, err := records[0].ValueSchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read value schema id: %w", err)
	}
	gotKeyID, err := records[0].KeySchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read key schema id: %w", err)
	}
	if gotKey != wantKey {
		return fmt.Errorf("key mismatch: want %q, got %q", wantKey, gotKey)
	}
	if gotVal != wantVal {
		return fmt.Errorf("value mismatch: want %q, got %q", wantVal, gotVal)
	}
	if gotValID != id {
		return fmt.Errorf("value schema id mismatch: want %d, got %d", id, gotValID)
	}
	if gotKeyID != 0 {
		return fmt.Errorf("expected key schema id 0 (unframed), got %d", gotKeyID)
	}
	return nil
}

// SchemaRegistryPlaintextConsumeUnframed verifies the negative path: a record
// produced without framing, consumed with schemaRegistryAware=true, must
// surface ValueSchemaID=0 and pass the value bytes through unchanged.
func (t *Tests) SchemaRegistryPlaintextConsumeUnframed(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	const wantKey, wantVal = "k", "plain"
	if err := client.Produce(ctx, topic, wantKey, wantVal, dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	gotVal, err := records[0].Value(ctx)
	if err != nil {
		return fmt.Errorf("read value: %w", err)
	}
	gotValID, err := records[0].ValueSchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read value schema id: %w", err)
	}
	gotKeyID, err := records[0].KeySchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read key schema id: %w", err)
	}
	if gotVal != wantVal {
		return fmt.Errorf("value mismatch: want %q, got %q", wantVal, gotVal)
	}
	if gotValID != 0 {
		return fmt.Errorf("expected ValueSchemaID=0 for unframed record, got %d", gotValID)
	}
	if gotKeyID != 0 {
		return fmt.Errorf("expected KeySchemaID=0 for unframed record, got %d", gotKeyID)
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

// SchemaRegistryJSONSerializeRejectsMalformedInput pins the up-front
// validation contract of valueSerializeAs="JSON": Produce must reject a
// malformed JSON payload before any broker I/O. dag.Kafka().Client(...)
// builds without I/O, so no cluster boots — the failure is purely a
// payload-validation failure on the canonicalising serializer.
func (t *Tests) SchemaRegistryJSONSerializeRejectsMalformedInput(
	ctx context.Context,
) error {
	client := dag.Kafka().Client(
		[]string{"127.0.0.1:1"},
		dag.Kafka().PlaintextClientSecurity(),
	)
	err := client.Produce(ctx, "any-topic", "k", "{not-json", dagger.KafkaClientProduceOpts{
		KeyEncoding:      "raw",
		ValueEncoding:    "raw",
		ValueSerializeAs: "JSON",
	})
	if err == nil {
		return fmt.Errorf("expected Produce to reject malformed JSON value, got nil error")
	}
	if !strings.Contains(err.Error(), "value is not valid JSON") {
		return fmt.Errorf("expected error to mention JSON validation failure, got: %v", err)
	}
	return nil
}

// SchemaRegistryJSONFramedProduceConsumeRoundTrip composes the framing
// primitive (valueSchemaID) with the JSON serde (valueSerializeAs /
// valueDeserializeAs). A JSON document is registered as a JSON-schema-typed
// subject, produced with both opts on, then consumed with both opts on; the
// asserted invariant is byte-equality of the consumed value to the canonical
// JSON form of the original input — proving frame strip and JSON validation
// compose without corrupting the payload.
func (t *Tests) SchemaRegistryJSONFramedProduceConsumeRoundTrip(
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

	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	topic := subject
	subject += "-value"

	const jsonSchema = `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`
	id, err := sr.Client().RegisterSchema(ctx, subject, jsonSchema, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "JSON",
	})
	if err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("expected a positive schema id, got %d", id)
	}

	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	// Whitespace and extra spacing chosen so canonicalisation visibly
	// changes the bytes — catches a regression where Produce skips
	// re-marshal and only does json.Valid.
	const inputJSON = `{ "x" :  "hello-world" }`
	const wantCanonical = `{"x":"hello-world"}`
	const wantKey = "k"

	if err := client.Produce(ctx, topic, wantKey, inputJSON, dagger.KafkaClientProduceOpts{
		KeyEncoding:      "raw",
		ValueEncoding:    "raw",
		ValueSchemaID:    id,
		ValueSerializeAs: "JSON",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
		ValueDeserializeAs:  "JSON",
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	gotKey, err := records[0].Key(ctx)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	gotVal, err := records[0].Value(ctx)
	if err != nil {
		return fmt.Errorf("read value: %w", err)
	}
	gotValID, err := records[0].ValueSchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read value schema id: %w", err)
	}
	if gotKey != wantKey {
		return fmt.Errorf("key mismatch: want %q, got %q", wantKey, gotKey)
	}
	if gotVal != wantCanonical {
		return fmt.Errorf("value mismatch: want canonical %q, got %q", wantCanonical, gotVal)
	}
	if gotValID != id {
		return fmt.Errorf("value schema id mismatch: want %d, got %d", id, gotValID)
	}
	return nil
}

// AvroSerializeRequiresSchemaID pins the up-front validation contract of
// valueSerializeAs="AVRO": Produce must reject a zero schema id before any
// broker or registry I/O. dag.Kafka().Client(...) builds without I/O, so no
// cluster boots — the failure is purely the missing-id guard on the AVRO
// serializer.
func (t *Tests) AvroSerializeRequiresSchemaID(
	ctx context.Context,
) error {
	client := dag.Kafka().Client(
		[]string{"127.0.0.1:1"},
		dag.Kafka().PlaintextClientSecurity(),
	)
	err := client.Produce(ctx, "any-topic", "k", `{"x":"hello"}`, dagger.KafkaClientProduceOpts{
		KeyEncoding:      "raw",
		ValueEncoding:    "raw",
		ValueSerializeAs: "AVRO",
		ValueSchemaID:    0,
	})
	if err == nil {
		return fmt.Errorf("expected Produce to reject AVRO with a zero schema id, got nil error")
	}
	if !strings.Contains(err.Error(), "valueSchemaID") {
		return fmt.Errorf("expected error to mention the required valueSchemaID, got: %v", err)
	}
	return nil
}

// AvroConsumeUnframedErrors pins the negative consume path: a record produced
// without a Confluent wire header, consumed with valueDeserializeAs="AVRO" +
// schemaRegistryAware=true, must error out pointing at the missing header. The
// header check fires before any schema lookup, so the registry service never
// has to start.
func (t *Tests) AvroConsumeUnframedErrors(
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

	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	if err := client.Produce(ctx, topic, "k", "plain", dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	_, err = client.Consume(ctx, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
		ValueDeserializeAs:  "AVRO",
		Registry:            sr,
	})
	if err == nil {
		return fmt.Errorf("expected Consume to reject an unframed record under AVRO, got nil error")
	}
	if !strings.Contains(err.Error(), "wire header") {
		return fmt.Errorf("expected error to point at the missing wire header, got: %v", err)
	}
	return nil
}

// AvroFramedProduceConsumeRoundTrip is the happy-path data round-trip for AVRO
// serde: register an Avro record schema to get an id, Produce a JSON document
// with valueSerializeAs="AVRO" + valueSchemaID=id (so it is Avro-binary-encoded
// then framed), and Consume it back with valueDeserializeAs="AVRO" +
// schemaRegistryAware=true. The asserted invariant is byte-equality of the
// consumed value to the canonical JSON form of the original input, proving the
// JSON->Avro-binary->JSON pipeline preserves the datum.
func (t *Tests) AvroFramedProduceConsumeRoundTrip(
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

	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	topic := subject
	subject += "-value"

	id, err := sr.Client().RegisterSchema(ctx, subject, avroTestSchema, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "AVRO",
	})
	if err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("expected a positive schema id, got %d", id)
	}

	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	// Whitespace differs from the canonical form so the round-trip proves the
	// payload is genuinely re-encoded, not passed through.
	const inputJSON = `{ "x" :  "hello-world" }`
	const wantCanonical = `{"x":"hello-world"}`
	const wantKey = "k"

	if err := client.Produce(ctx, topic, wantKey, inputJSON, dagger.KafkaClientProduceOpts{
		KeyEncoding:      "raw",
		ValueEncoding:    "raw",
		ValueSchemaID:    id,
		ValueSerializeAs: "AVRO",
		Registry:         sr,
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
		ValueDeserializeAs:  "AVRO",
		Registry:            sr,
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 record, got %d", len(records))
	}
	gotKey, err := records[0].Key(ctx)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	gotVal, err := records[0].Value(ctx)
	if err != nil {
		return fmt.Errorf("read value: %w", err)
	}
	gotValID, err := records[0].ValueSchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read value schema id: %w", err)
	}
	if gotKey != wantKey {
		return fmt.Errorf("key mismatch: want %q, got %q", wantKey, gotKey)
	}
	if gotVal != wantCanonical {
		return fmt.Errorf("value mismatch: want canonical %q, got %q", wantCanonical, gotVal)
	}
	if gotValID != id {
		return fmt.Errorf("value schema id mismatch: want %d, got %d", id, gotValID)
	}
	return nil
}
