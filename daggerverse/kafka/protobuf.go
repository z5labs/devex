package main

import (
	"context"
	"fmt"
	"slices"

	"dagger/kafka/internal/dagger"

	"github.com/twmb/franz-go/pkg/sr"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// protobuf.go bridges the kafka module's JSON string boundary to the Protobuf
// binary wire format carried by Schema-Registry-aware Kafka.
//
// Decoding a registered Protobuf schema fundamentally needs a compiled
// FileDescriptor, and Schema Registry only stores `.proto` *text*. Compiling
// that text at runtime would mean shipping protoc (and its well-known-type
// includes) inside the module. Instead the caller compiles out-of-band —
// `protoc --descriptor_set_out=...` — and hands the module the resulting
// FileDescriptorSet as a *dagger.File plus the fully-qualified message name.
// The module loads it via protodesc/protoregistry and builds a dynamicpb
// message from the resolved descriptor. This keeps the daggerverse Go-only
// and defers compiler bootstrapping to the caller.
//
// Consequences of that tradeoff, all of which the README repeats:
//   - The registry is never consulted to resolve a Protobuf message type, so
//     PROTOBUF consume does not require a `registry` (unlike AVRO). The wire
//     schema id is still surfaced on ConsumedRecord.
//   - Nothing checks that the descriptor set actually matches the schema
//     registered under the wire id. A mismatch decodes to garbage exactly as
//     it would with any other Protobuf consumer given the wrong descriptor.
//
// Unlike Avro and JSON, the Confluent Protobuf wire format is not just the
// 5-byte `0x00 || uint32be(schemaID)` header: a *message-index array* sits
// between the header and the payload, naming which message inside the schema's
// `.proto` file the payload is an instance of. franz-go's sr.ConfluentHeader
// encodes and decodes it (AppendEncode's `index` argument / DecodeIndex), so
// records produced here are readable by stock Confluent Protobuf consumers and
// vice versa. See messageIndexPath for how the path is derived.
//
// The JSON shape is protojson's, i.e. the canonical protobuf JSON mapping:
// field names are lowerCamelCase JSON names, 64-bit integers are JSON strings,
// enums are their symbol names, and proto3 fields holding the zero value are
// omitted on output.

// protoMessages resolves a caller-supplied FileDescriptorSet down to a single
// protobuf message type and memoizes the result for the lifetime of one
// Produce / Consume call. Consume polls many records against the same
// descriptor set, and resolving it means exporting a *dagger.File and parsing
// it, so the descriptor is resolved at most once per call rather than once per
// record. One protoMessages serves one field — key and value carry independent
// descriptor sets and message names.
type protoMessages struct {
	descriptorSet *dagger.File
	messageName   string

	// md and index are the memoized resolution; md != nil means resolved.
	md    protoreflect.MessageDescriptor
	index []int
}

func newProtoMessages(descriptorSet *dagger.File, messageName string) *protoMessages {
	return &protoMessages{descriptorSet: descriptorSet, messageName: messageName}
}

// validate rejects a PROTOBUF-mode call that is missing its descriptor set or
// message name. Both checks are pure struct inspection, so callers can run
// this before touching a broker, the registry, or the *dagger.File itself.
// directive is the caller-facing parameter name ("valueSerializeAs",
// "keyDeserializeAs", ...) and field is "key" or "value".
func (p *protoMessages) validate(field, directive string) error {
	if p.descriptorSet == nil {
		return fmt.Errorf("%s=%q requires %sDescriptorSet (a precompiled protobuf FileDescriptorSet, e.g. from `protoc --descriptor_set_out`)", directive, "PROTOBUF", field)
	}
	if p.messageName == "" {
		return fmt.Errorf("%s=%q requires %sMessageName (the fully-qualified protobuf message name, e.g. \"my.pkg.MyMessage\")", directive, "PROTOBUF", field)
	}
	return nil
}

// resolve exports and parses the descriptor set on first call, looks the named
// message up in it, and caches the descriptor plus its Confluent message-index
// path.
func (p *protoMessages) resolve(ctx context.Context) (protoreflect.MessageDescriptor, []int, error) {
	if p.md != nil {
		return p.md, p.index, nil
	}
	raw, err := dagFileBytes(ctx, p.descriptorSet)
	if err != nil {
		return nil, nil, fmt.Errorf("read descriptor set: %w", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		return nil, nil, fmt.Errorf("parse descriptor set (expected a protobuf FileDescriptorSet, as produced by `protoc --descriptor_set_out`): %w", err)
	}
	// NewFiles resolves every import in the set; a set built without
	// --include_imports for a .proto that imports others fails here rather
	// than silently producing a half-resolved descriptor.
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return nil, nil, fmt.Errorf("build descriptor registry (a .proto with imports needs `protoc --include_imports`): %w", err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(p.messageName))
	if err != nil {
		return nil, nil, fmt.Errorf("message %q not found in descriptor set: %w", p.messageName, err)
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, nil, fmt.Errorf("%q resolves to a %T, not a message type", p.messageName, d)
	}
	p.md, p.index = md, messageIndexPath(md)
	return p.md, p.index, nil
}

// messageIndexPath derives the Confluent message-index path for md: the
// sequence of declaration indexes to walk from the top of its .proto file down
// to md. A first top-level message is [0]; the second is [1]; a message nested
// once inside the second is [1, 0].
//
// https://docs.confluent.io/platform/current/schema-registry/fundamentals/serdes-develop/index.html#wire-format
func messageIndexPath(md protoreflect.MessageDescriptor) []int {
	var path []int
	for cur := md; ; {
		path = append(path, cur.Index())
		parent, ok := cur.Parent().(protoreflect.MessageDescriptor)
		if !ok {
			// Parent is the FileDescriptor: cur is top-level, so the walk is
			// done and cur.Index() (already appended) is the last element.
			break
		}
		cur = parent
	}
	slices.Reverse(path)
	return path
}

// encode interprets raw as a protobuf-JSON document, unmarshals it into a
// dynamic message of the resolved type, and returns the protobuf wire bytes
// alongside the message-index path the framing step must embed.
func (p *protoMessages) encode(ctx context.Context, raw []byte) ([]byte, []int, error) {
	md, index, err := p.resolve(ctx)
	if err != nil {
		return nil, nil, err
	}
	msg := dynamicpb.NewMessage(md)
	// DiscardUnknown stays false (the default) so a typo'd or stale field name
	// in the caller's JSON is an error rather than a silently dropped value.
	if err := protojson.Unmarshal(raw, msg); err != nil {
		return nil, nil, fmt.Errorf("map json to protobuf message %q: %w", p.messageName, err)
	}
	// Deterministic keeps map-valued fields in a stable order so the same JSON
	// input always yields the same wire bytes. Protobuf does not promise a
	// canonical encoding across implementations, but within this module it
	// makes Produce reproducible.
	out, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return nil, nil, fmt.Errorf("protobuf-encode: %w", err)
	}
	return out, index, nil
}

// decode reads protobuf wire bytes bin (message-index already stripped)
// against the resolved message type and returns its canonical compact JSON
// encoding.
func (p *protoMessages) decode(ctx context.Context, bin []byte, field string) ([]byte, error) {
	md, _, err := p.resolve(ctx)
	if err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(bin, msg); err != nil {
		return nil, fmt.Errorf("protobuf-decode against %q: %w", p.messageName, err)
	}
	out, err := protojson.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("map protobuf to json: %w", err)
	}
	// protojson deliberately emits unstable whitespace (it randomly varies the
	// space after ":" and "," to discourage byte-comparing its output), so the
	// same message can marshal to two different strings within one process.
	// Re-marshalling through canonicalJSON pins a single compact form, which
	// is what makes a consumed value comparable at all — and matches what the
	// "JSON" and "AVRO" modes hand back.
	return canonicalJSON(out, field)
}

// stripIndex removes the Confluent message-index array that sits between the
// 5-byte schema-id header and the protobuf payload, and checks it names the
// message the caller asked for. A mismatch means the record holds a different
// message type from the same .proto file; decoding it against the named
// descriptor would silently yield garbage (protobuf wire bytes are not
// self-describing enough to catch it), so it is an error instead.
func (p *protoMessages) stripIndex(ctx context.Context, b []byte, field string) ([]byte, error) {
	_, want, err := p.resolve(ctx)
	if err != nil {
		return nil, err
	}
	var hdr sr.ConfluentHeader
	// maxLength 0 leaves the index length unbounded; the descriptor set, not
	// an arbitrary cap, is what decides whether the path resolves.
	got, rest, err := hdr.DecodeIndex(b, 0)
	if err != nil {
		return nil, fmt.Errorf("decode %s protobuf message-index: %w", field, err)
	}
	if !slices.Equal(got, want) {
		return nil, fmt.Errorf("%s record carries protobuf message-index %v but %q is at index %v in the descriptor set; the record holds a different message type", field, got, p.messageName, want)
	}
	return rest, nil
}
