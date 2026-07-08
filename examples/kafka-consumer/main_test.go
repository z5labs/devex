package main

import (
	"bytes"
	"testing"

	"github.com/twmb/franz-go/pkg/sr"
	"github.com/z5labs/avro-go"
	"github.com/z5labs/avro-go/generic"
)

// TestConfluentHeaderRoundTrip verifies that the Confluent wire framing the
// consumer relies on (magic byte 0x00 + 4-byte big-endian schema id + body)
// parses back to the id and body we framed, using the same franz-go sr codec
// the consumer uses.
func TestConfluentHeaderRoundTrip(t *testing.T) {
	var hdr sr.ConfluentHeader
	const wantID = 42
	body := []byte("avro-binary-body")

	// The Confluent Avro framing is a 5-byte header (magic + id, no protobuf
	// message index) followed by the Avro binary body — exactly how the kafka
	// module frames it: AppendEncode(nil, id, nil) then append the body.
	framed, err := hdr.AppendEncode(nil, wantID, nil)
	if err != nil {
		t.Fatalf("AppendEncode: %v", err)
	}
	framed = append(framed, body...)
	if framed[0] != 0x00 {
		t.Fatalf("magic byte = %#x, want 0x00", framed[0])
	}

	gotID, gotBody, err := hdr.DecodeID(framed)
	if err != nil {
		t.Fatalf("DecodeID: %v", err)
	}
	if gotID != wantID {
		t.Errorf("schema id = %d, want %d", gotID, wantID)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body = %q, want %q", gotBody, body)
	}
}

// TestJSONFromValueRecord exercises the ported avro-go decode walker end to end:
// encode a record to Avro binary with avro-go, decode it, then map the
// generic.Value back to a JSON-ready map and assert the fields survive. This
// keeps the walker honest without any Kafka or registry.
func TestJSONFromValueRecord(t *testing.T) {
	const schemaJSON = `{
		"type": "record",
		"name": "Event",
		"fields": [
			{"name": "id", "type": "long"},
			{"name": "name", "type": "string"},
			{"name": "active", "type": "boolean"}
		]
	}`
	schema, err := avro.ParseJSON([]byte(schemaJSON))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}

	enc, err := generic.NewEncoder(schema)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	var buf bytes.Buffer
	rec := generic.Record{Fields: []generic.Field{
		{Name: "id", Value: generic.Long(7)},
		{Name: "name", Value: generic.String("hello-world")},
		{Name: "active", Value: generic.Bool(true)},
	}}
	if err := enc.Encode(&buf, rec); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	dec, err := generic.NewDecoder(schema)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	val, err := dec.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	out, err := jsonFromValue(val, schema, buildNameTable(schema), "")
	if err != nil {
		t.Fatalf("jsonFromValue: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("decoded value is %T, want map[string]any", out)
	}
	if got := m["id"]; got != int64(7) {
		t.Errorf("id = %v (%T), want int64(7)", got, got)
	}
	if got := m["name"]; got != "hello-world" {
		t.Errorf("name = %v, want hello-world", got)
	}
	if got := m["active"]; got != true {
		t.Errorf("active = %v, want true", got)
	}
}

// TestLoadConfigRejectsPlaintextRegistry asserts the no-plaintext-path guard:
// a non-https registry URL is refused.
func TestLoadConfigRejectsPlaintextRegistry(t *testing.T) {
	t.Setenv("BROKERS", "broker:9092")
	t.Setenv("TOPIC", "events")
	t.Setenv("REGISTRY_URL", "http://registry:8081")
	t.Setenv("TRUSTSTORE", "/certs/truststore.p12")
	t.Setenv("TRUSTSTORE_PASSWORD", "pw")

	if _, err := loadConfig(nil); err == nil {
		t.Fatal("loadConfig accepted a plaintext http:// registry URL; want error")
	}
}
