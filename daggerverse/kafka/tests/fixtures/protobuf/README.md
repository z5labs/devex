# Protobuf serde fixtures

`user.desc` is a precompiled [`FileDescriptorSet`][fds] covering `user.proto`.
It is the fixture the kafka module's `PROTOBUF` serde tests hand to
`Client.Produce` / `Client.Consume` as `valueDescriptorSet` /
`keyDescriptorSet`.

It is checked in as a binary artifact on purpose: the kafka module resolves
Protobuf message types via `protoreflect` / `protoregistry` and never invokes
`protoc` at runtime, so compiling `.proto` text is the caller's job. Shipping
the compiled descriptor set keeps the test suite free of a `protoc` toolchain.

## Regenerating

From this directory, with any reasonably recent `protoc` on `PATH`:

```sh
protoc --descriptor_set_out=user.desc --proto_path=. user.proto
```

`user.desc` was last generated with `libprotoc 35.0-dev`. The output is stable
across protoc versions for a file this simple — it is just the serialized
`FileDescriptorProto` for `user.proto` — so a regeneration on a different
protoc should produce an identical (or near-identical) file. Re-run the
`protobuf-*` tests after regenerating.

If the message type lived in a `.proto` that imported others, every transitive
import would need to be in the same set; add `--include_imports` in that case.
`user.proto` imports nothing, so the flag is unnecessary here.

## What the messages cover

| Message | Index path | Why it exists |
| --- | --- | --- |
| `devex.kafka.test.User` | `[0]` | Happy-path round-trip. First message in the file, so the Confluent message-index array uses the single-zero-byte shortcut. |
| `devex.kafka.test.Event` | `[1]` | Second top-level message, so the index array takes the length-prefixed varint form instead of the shortcut. |
| `devex.kafka.test.Event.Nested` | `[1, 0]` | A nested message, so the index path is more than one element deep. |

Between them the three messages exercise both encodings of the Confluent
Protobuf message-index array plus a multi-element index path.

[fds]: https://protobuf.com/docs/descriptors#file-descriptor-set
