package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"
)

// The PROTOBUF serde tests drive the kafka module's third Schema-Registry
// payload format. Unlike AVRO, the module never resolves a Protobuf message
// type through the registry — Schema Registry stores `.proto` *text* and
// decoding needs a compiled FileDescriptor, so the caller compiles one
// out-of-band and hands it over as a *dagger.File. See
// fixtures/protobuf/README.md for how the checked-in descriptor set was built.

const (
	// protobufUserMessage is the first message declared in user.proto, so its
	// Confluent message-index path is [0] — the single-zero-byte shortcut.
	protobufUserMessage = "devex.kafka.test.User"
	// protobufEventMessage is the second top-level message, at index path [1],
	// which forces the length-prefixed varint form of the index array.
	protobufEventMessage = "devex.kafka.test.Event"
)

// protobufDescriptorSet returns the checked-in FileDescriptorSet fixture as a
// *dagger.File, which is exactly how a caller would supply their own after
// running `protoc --descriptor_set_out`.
func protobufDescriptorSet() *dagger.File {
	return dag.CurrentModule().Source().File("fixtures/protobuf/user.desc")
}

// protobufSchemaText returns the `.proto` source the descriptor set was
// compiled from. It is what gets registered with the Schema Registry so the
// registered schema and the descriptor set genuinely describe the same
// messages, even though the module only ever reads the descriptor set.
func protobufSchemaText(ctx context.Context) (string, error) {
	text, err := dag.CurrentModule().Source().File("fixtures/protobuf/user.proto").Contents(ctx)
	if err != nil {
		return "", fmt.Errorf("read user.proto fixture: %w", err)
	}
	return text, nil
}

// ProtobufSerializeRequiresDescriptorSet pins the up-front validation contract
// of valueSerializeAs="PROTOBUF": Produce must reject a missing descriptor set
// before any broker, registry, or file I/O. dag.Kafka().Client(...) builds
// without I/O and the bootstrap address is unroutable, so a nil error here
// would mean the guard never fired.
func (t *Tests) ProtobufSerializeRequiresDescriptorSet(
	ctx context.Context,
) error {
	client := dag.Kafka().Client(
		[]string{"127.0.0.1:1"},
		dag.Kafka().PlaintextClientSecurity(),
	)
	err := client.Produce(ctx, "any-topic", "k", `{"name":"ada"}`, dagger.KafkaClientProduceOpts{
		KeyEncoding:      "raw",
		ValueEncoding:    "raw",
		ValueSerializeAs: "PROTOBUF",
		ValueSchemaID:    1,
		ValueMessageName: protobufUserMessage,
	})
	if err == nil {
		return fmt.Errorf("expected Produce to reject PROTOBUF without a descriptor set, got nil error")
	}
	if !strings.Contains(err.Error(), "valueDescriptorSet") {
		return fmt.Errorf("expected error to mention the required valueDescriptorSet, got: %v", err)
	}
	return nil
}

// ProtobufSerializeRequiresMessageName is the sibling guard: a descriptor set
// alone is not enough, because a FileDescriptorSet can hold many message types
// and nothing in it says which one the payload is.
func (t *Tests) ProtobufSerializeRequiresMessageName(
	ctx context.Context,
) error {
	client := dag.Kafka().Client(
		[]string{"127.0.0.1:1"},
		dag.Kafka().PlaintextClientSecurity(),
	)
	err := client.Produce(ctx, "any-topic", "k", `{"name":"ada"}`, dagger.KafkaClientProduceOpts{
		KeyEncoding:        "raw",
		ValueEncoding:      "raw",
		ValueSerializeAs:   "PROTOBUF",
		ValueSchemaID:      1,
		ValueDescriptorSet: protobufDescriptorSet(),
	})
	if err == nil {
		return fmt.Errorf("expected Produce to reject PROTOBUF without a message name, got nil error")
	}
	if !strings.Contains(err.Error(), "valueMessageName") {
		return fmt.Errorf("expected error to mention the required valueMessageName, got: %v", err)
	}
	return nil
}

// ProtobufSerializeRequiresSchemaID pins that PROTOBUF mode also demands a
// positive schema id. The id is not optional the way it arguably is for a bare
// JSON payload: the Confluent message-index array lives inside the frame, so
// an unframed Protobuf record has nowhere to record which message type it
// holds and no consumer could decode it.
func (t *Tests) ProtobufSerializeRequiresSchemaID(
	ctx context.Context,
) error {
	client := dag.Kafka().Client(
		[]string{"127.0.0.1:1"},
		dag.Kafka().PlaintextClientSecurity(),
	)
	err := client.Produce(ctx, "any-topic", "k", `{"name":"ada"}`, dagger.KafkaClientProduceOpts{
		KeyEncoding:        "raw",
		ValueEncoding:      "raw",
		ValueSerializeAs:   "PROTOBUF",
		ValueSchemaID:      0,
		ValueDescriptorSet: protobufDescriptorSet(),
		ValueMessageName:   protobufUserMessage,
	})
	if err == nil {
		return fmt.Errorf("expected Produce to reject PROTOBUF with a zero schema id, got nil error")
	}
	if !strings.Contains(err.Error(), "valueSchemaID") {
		return fmt.Errorf("expected error to mention the required valueSchemaID, got: %v", err)
	}
	return nil
}

// ProtobufDeserializeRequiresDescriptorSet mirrors the produce-side guard on
// the consume path. It must fail before a broker connection is opened, which
// the unroutable bootstrap address proves: a missing guard would surface as a
// dial timeout rather than a validation error.
func (t *Tests) ProtobufDeserializeRequiresDescriptorSet(
	ctx context.Context,
) error {
	client := dag.Kafka().Client(
		[]string{"127.0.0.1:1"},
		dag.Kafka().PlaintextClientSecurity(),
	)
	_, err := client.Consume(ctx, "any-topic", dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
		ValueDeserializeAs:  "PROTOBUF",
		ValueMessageName:    protobufUserMessage,
	})
	if err == nil {
		return fmt.Errorf("expected Consume to reject PROTOBUF without a descriptor set, got nil error")
	}
	if !strings.Contains(err.Error(), "valueDescriptorSet") {
		return fmt.Errorf("expected error to mention the required valueDescriptorSet, got: %v", err)
	}
	return nil
}

// ProtobufDeserializeRequiresSchemaRegistryAware pins that PROTOBUF consume
// needs schemaRegistryAware=true: without it the 5-byte header is never
// stripped, so neither the wire id nor the message-index array that follows it
// is reachable.
func (t *Tests) ProtobufDeserializeRequiresSchemaRegistryAware(
	ctx context.Context,
) error {
	client := dag.Kafka().Client(
		[]string{"127.0.0.1:1"},
		dag.Kafka().PlaintextClientSecurity(),
	)
	_, err := client.Consume(ctx, "any-topic", dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: false,
		ValueDeserializeAs:  "PROTOBUF",
		ValueDescriptorSet:  protobufDescriptorSet(),
		ValueMessageName:    protobufUserMessage,
	})
	if err == nil {
		return fmt.Errorf("expected Consume to reject PROTOBUF without schemaRegistryAware, got nil error")
	}
	if !strings.Contains(err.Error(), "schemaRegistryAware") {
		return fmt.Errorf("expected error to mention the required schemaRegistryAware, got: %v", err)
	}
	return nil
}

// ProtobufFramedProduceConsumeRoundTrip is the happy-path data round-trip for
// PROTOBUF serde: register user.proto to get a schema id, Produce a JSON
// document with valueSerializeAs="PROTOBUF" + the descriptor set + the message
// name (so it is protobuf-encoded, then framed with the header *and* the
// message-index array), and Consume it back with valueDeserializeAs="PROTOBUF"
// + schemaRegistryAware=true.
//
// The asserted invariant is byte-equality of the consumed value to the
// canonical JSON form of the original input, proving the
// JSON->protobuf-binary->JSON pipeline preserves the datum. devex.kafka.test.User
// is the first message in the file, so this exercises the single-zero-byte
// shortcut form of the message-index array.
func (t *Tests) ProtobufFramedProduceConsumeRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return protobufRoundTrip(ctx, kafkaImageTag, protobufUserMessage,
		// Whitespace differs from the canonical form so the round-trip proves
		// the payload is genuinely re-encoded, not passed through.
		`{ "name" :  "ada" , "age": 36, "tags": ["lovelace", "analytical-engine"] }`,
		// protojson renders int32 as a JSON number and single-word field names
		// unchanged; the module then re-marshals through encoding/json, which
		// sorts object keys, so the canonical form is alphabetical rather than
		// in proto field-number order.
		`{"age":36,"name":"ada","tags":["lovelace","analytical-engine"]}`,
	)
}

// ProtobufNonZeroMessageIndexRoundTrip drives the same pipeline against
// devex.kafka.test.Event, the *second* top-level message in user.proto. Its
// Confluent message-index path is [1] rather than [0], so the frame carries
// the length-prefixed varint form of the index array instead of the
// single-zero-byte shortcut. Without a correctly written and re-read index the
// consume side would either mis-parse the payload or reject the record, so a
// green round-trip here is what proves the index round-trips rather than being
// accidentally skipped.
func (t *Tests) ProtobufNonZeroMessageIndexRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	return protobufRoundTrip(ctx, kafkaImageTag, protobufEventMessage,
		`{"id":"evt-1","nested":{"note":"hello"}}`,
		`{"id":"evt-1","nested":{"note":"hello"}}`,
	)
}

// ProtobufKeyFramedProduceConsumeRoundTrip drives PROTOBUF serde on the *key*
// rather than the value. Key and value are plumbed through independent
// protoMessages instances, independent validation calls, and independent
// framing branches, so the value round-trip alone would not catch a
// copy-paste slip on the key side. Both fields are encoded here — with
// different message types, so a crossed wire between them fails rather than
// coincidentally passing.
func (t *Tests) ProtobufKeyFramedProduceConsumeRoundTrip(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshClusterApache(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	sr := dag.Kafka().ConfluentSchemaRegistry(cluster, plaintextSchemaRegistrySecurity())
	defer sr.Stop(ctx)

	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	topic := subject

	schemaText, err := protobufSchemaText(ctx)
	if err != nil {
		return err
	}
	srClient := sr.Client(plaintextSchemaRegistryClientSecurity())
	keyID, err := srClient.RegisterSchema(ctx, subject+"-key", schemaText, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "PROTOBUF",
	})
	if err != nil {
		return fmt.Errorf("register key schema: %w", err)
	}
	valueID, err := srClient.RegisterSchema(ctx, subject+"-value", schemaText, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "PROTOBUF",
	})
	if err != nil {
		return fmt.Errorf("register value schema: %w", err)
	}

	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	// The key is an Event (message-index [1]) and the value a User
	// (message-index [0]), so each field must carry its own index — swapping
	// them would trip the index check on consume.
	const (
		keyInput      = `{"id":"evt-7"}`
		wantKey       = `{"id":"evt-7"}`
		valueInput    = `{ "name": "grace" , "age": 45 }`
		wantCanonical = `{"age":45,"name":"grace"}`
	)

	if err := client.Produce(ctx, topic, keyInput, valueInput, dagger.KafkaClientProduceOpts{
		KeyEncoding:        "raw",
		ValueEncoding:      "raw",
		KeySchemaID:        keyID,
		KeySerializeAs:     "PROTOBUF",
		KeyDescriptorSet:   protobufDescriptorSet(),
		KeyMessageName:     protobufEventMessage,
		ValueSchemaID:      valueID,
		ValueSerializeAs:   "PROTOBUF",
		ValueDescriptorSet: protobufDescriptorSet(),
		ValueMessageName:   protobufUserMessage,
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
		KeyDeserializeAs:    "PROTOBUF",
		KeyDescriptorSet:    protobufDescriptorSet(),
		KeyMessageName:      protobufEventMessage,
		ValueDeserializeAs:  "PROTOBUF",
		ValueDescriptorSet:  protobufDescriptorSet(),
		ValueMessageName:    protobufUserMessage,
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
	gotKeyID, err := records[0].KeySchemaID(ctx)
	if err != nil {
		return fmt.Errorf("read key schema id: %w", err)
	}
	if gotKey != wantKey {
		return fmt.Errorf("key mismatch: want canonical %q, got %q", wantKey, gotKey)
	}
	if gotVal != wantCanonical {
		return fmt.Errorf("value mismatch: want canonical %q, got %q", wantCanonical, gotVal)
	}
	if gotKeyID != keyID {
		return fmt.Errorf("key schema id mismatch: want %d, got %d", keyID, gotKeyID)
	}
	return nil
}

// ProtobufConsumeUnframedErrors pins the negative consume path: a record
// produced without a Confluent wire header, consumed with
// valueDeserializeAs="PROTOBUF" + schemaRegistryAware=true, must error out
// pointing at the missing header rather than trying to parse arbitrary bytes
// as a protobuf message. No registry is started — PROTOBUF decoding never
// needs one, and the header check fires before the descriptor set is even
// read.
func (t *Tests) ProtobufConsumeUnframedErrors(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshClusterApache(ctx, kafkaImageTag)
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
		ValueDeserializeAs:  "PROTOBUF",
		ValueDescriptorSet:  protobufDescriptorSet(),
		ValueMessageName:    protobufUserMessage,
	})
	if err == nil {
		return fmt.Errorf("expected Consume to reject an unframed record under PROTOBUF, got nil error")
	}
	if !strings.Contains(err.Error(), "wire header") {
		return fmt.Errorf("expected error to point at the missing wire header, got: %v", err)
	}
	return nil
}

// ProtobufConsumeMessageIndexMismatchErrors pins the guard that makes the
// message-index array load-bearing rather than decorative. A record produced
// as devex.kafka.test.User (index [0]) but consumed as devex.kafka.test.Event
// (index [1]) must be rejected: protobuf wire bytes are not self-describing,
// so decoding one message type's bytes against another's descriptor would
// otherwise succeed and hand back silent garbage.
func (t *Tests) ProtobufConsumeMessageIndexMismatchErrors(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshClusterApache(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	sr := dag.Kafka().ConfluentSchemaRegistry(cluster, plaintextSchemaRegistrySecurity())
	defer sr.Stop(ctx)

	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	topic := subject
	subject += "-value"

	schemaText, err := protobufSchemaText(ctx)
	if err != nil {
		return err
	}
	id, err := sr.Client(plaintextSchemaRegistryClientSecurity()).RegisterSchema(ctx, subject, schemaText, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "PROTOBUF",
	})
	if err != nil {
		return fmt.Errorf("register schema: %w", err)
	}

	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	if err := client.Produce(ctx, topic, "k", `{"name":"ada","age":36}`, dagger.KafkaClientProduceOpts{
		KeyEncoding:        "raw",
		ValueEncoding:      "raw",
		ValueSchemaID:      id,
		ValueSerializeAs:   "PROTOBUF",
		ValueDescriptorSet: protobufDescriptorSet(),
		ValueMessageName:   protobufUserMessage,
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	_, err = client.Consume(ctx, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
		ValueDeserializeAs:  "PROTOBUF",
		ValueDescriptorSet:  protobufDescriptorSet(),
		ValueMessageName:    protobufEventMessage,
	})
	if err == nil {
		return fmt.Errorf("expected Consume to reject a message-index mismatch, got nil error")
	}
	if !strings.Contains(err.Error(), "message-index") {
		return fmt.Errorf("expected error to point at the message-index mismatch, got: %v", err)
	}
	return nil
}

// protobufRoundTrip is the shared body of the PROTOBUF round-trip tests: stand
// up a cluster and registry, register user.proto, produce inputJSON as
// messageName, consume it back, and assert the consumed value equals
// wantCanonical and carries the registered schema id.
func protobufRoundTrip(ctx context.Context, kafkaImageTag, messageName, inputJSON, wantCanonical string) error {
	cluster, err := freshClusterApache(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)

	sr := dag.Kafka().ConfluentSchemaRegistry(cluster, plaintextSchemaRegistrySecurity())
	defer sr.Stop(ctx)

	subject, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	topic := subject
	subject += "-value"

	schemaText, err := protobufSchemaText(ctx)
	if err != nil {
		return err
	}
	// The module never reads this registration back — the descriptor set is
	// what drives encoding and decoding. Registering the matching .proto is
	// what makes the produced records legible to a stock Confluent Protobuf
	// consumer, which *does* resolve the id, and it is what mints the id the
	// frame carries.
	id, err := sr.Client(plaintextSchemaRegistryClientSecurity()).RegisterSchema(ctx, subject, schemaText, dagger.KafkaSchemaRegistryClientRegisterSchemaOpts{
		SchemaType: "PROTOBUF",
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

	const wantKey = "k"
	if err := client.Produce(ctx, topic, wantKey, inputJSON, dagger.KafkaClientProduceOpts{
		KeyEncoding:        "raw",
		ValueEncoding:      "raw",
		ValueSchemaID:      id,
		ValueSerializeAs:   "PROTOBUF",
		ValueDescriptorSet: protobufDescriptorSet(),
		ValueMessageName:   messageName,
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	// No Registry is passed: PROTOBUF decoding resolves the message type from
	// the descriptor set alone, so a green consume here also proves the mode
	// does not secretly depend on registry resolution the way AVRO does.
	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:         1,
		Timeout:             "10s",
		KeyEncoding:         "raw",
		ValueEncoding:       "raw",
		SchemaRegistryAware: true,
		ValueDeserializeAs:  "PROTOBUF",
		ValueDescriptorSet:  protobufDescriptorSet(),
		ValueMessageName:    messageName,
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
