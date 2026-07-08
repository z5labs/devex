package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/z5labs/avro-go"
	"github.com/z5labs/avro-go/generic"
)

// avro.go bridges the kafka module's JSON string boundary to the Avro binary
// wire format carried by Schema-Registry-aware Kafka.
//
// The Confluent wire payload is Avro *binary* (the 5-byte header laid down by
// the framing step wraps Avro binary, never Avro-JSON). github.com/z5labs/
// avro-go/generic encodes/decodes that binary, but its only input/output type
// is a closed generic.Value tree — it has no JSON or map[string]any bridge. So
// this file walks JSON <-> generic.Value *against the schema*: a JSON number
// becomes Int / Long / Float / Double depending on the schema node, a JSON
// object becomes a Record or a Map, a union picks a branch index, etc.
//
// The JSON shape follows the Avro spec's JSON encoding: a union value is bare
// `null` for the null branch or `{"<typename>": value}` for a non-null branch;
// `bytes` is a string with one character per byte (code points 0-255).
//
// Logical types, decimal, and fixed are intentionally out of scope for this
// first cut and surface a clear "unsupported in AVRO mode" error.
//
// IMPORTANT: a defect in avro-go itself (avro.ParseJSON, generic.NewEncoder /
// NewDecoder, Encode / Decode, or binary/schema-compilation correctness) must
// be filed upstream on z5labs/avro-go and work paused — it must NOT be worked
// around here. Only the JSON <-> generic.Value mapping below is ours to fix.

// avroSchemas resolves and memoizes Avro schemas (and their compiled codecs)
// by Schema Registry id for the lifetime of a single Produce / Consume call.
// The issue requires schema resolution be cached per (registry, schemaID)
// within one Consume call to avoid an HTTP round-trip per record; the same
// cache serves Produce's single record.
type avroSchemas struct {
	registry         *SchemaRegistry
	registrySecurity *SchemaRegistryClientSecurity
	byID             map[int]*avroSchema
}

// avroSchema is one resolved schema plus its lazily-built name table and
// codecs. The encoder/decoder are compiled on first use so a Produce-only or
// Consume-only path pays for just the direction it needs.
type avroSchema struct {
	schema avro.Schema
	names  *nameTable
	enc    *generic.Encoder
	dec    *generic.Decoder
}

func newAvroSchemas(registry *SchemaRegistry, security *SchemaRegistryClientSecurity) *avroSchemas {
	return &avroSchemas{registry: registry, registrySecurity: security, byID: make(map[int]*avroSchema)}
}

// get returns the resolved schema for id, fetching its text from the registry
// and parsing it on first request and serving the cached entry thereafter.
func (a *avroSchemas) get(ctx context.Context, id int) (*avroSchema, error) {
	if s, ok := a.byID[id]; ok {
		return s, nil
	}
	if a.registry == nil {
		return nil, fmt.Errorf("AVRO mode requires a schema registry to resolve schema id %d", id)
	}
	// The franz-go avro path resolves schemas over the registry's REST API.
	// registrySecurity is the client profile threaded from Produce/Consume: a
	// nil profile resolves over plaintext HTTP (today's behaviour), while a
	// TLS/mTLS profile resolves the schema over HTTPS against a secured
	// registry (#141).
	rs, err := a.registry.Client(a.registrySecurity).LookupSchemaByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("resolve schema id %d: %w", id, err)
	}
	schema, err := avro.ParseJSON([]byte(rs.Definition))
	if err != nil {
		return nil, fmt.Errorf("parse schema id %d: %w", id, err)
	}
	s := &avroSchema{schema: schema, names: buildNameTable(schema)}
	a.byID[id] = s
	return s, nil
}

func (s *avroSchema) encoder() (*generic.Encoder, error) {
	if s.enc == nil {
		enc, err := generic.NewEncoder(s.schema)
		if err != nil {
			return nil, fmt.Errorf("compile avro encoder: %w", err)
		}
		s.enc = enc
	}
	return s.enc, nil
}

func (s *avroSchema) decoder() (*generic.Decoder, error) {
	if s.dec == nil {
		dec, err := generic.NewDecoder(s.schema)
		if err != nil {
			return nil, fmt.Errorf("compile avro decoder: %w", err)
		}
		s.dec = dec
	}
	return s.dec, nil
}

// encode interprets raw as a JSON document, maps it to a generic.Value against
// the schema identified by id, and returns the Avro binary encoding.
func (a *avroSchemas) encode(ctx context.Context, id int, raw []byte) ([]byte, error) {
	s, err := a.get(ctx, id)
	if err != nil {
		return nil, err
	}
	enc, err := s.encoder()
	if err != nil {
		return nil, err
	}
	doc, err := unmarshalJSONValue(raw)
	if err != nil {
		return nil, err
	}
	val, err := valueFromJSON(doc, s.schema, s.names, "")
	if err != nil {
		return nil, fmt.Errorf("map json to avro: %w", err)
	}
	var buf bytes.Buffer
	if err := enc.Encode(&buf, val); err != nil {
		return nil, fmt.Errorf("avro-encode: %w", err)
	}
	return buf.Bytes(), nil
}

// decode reads Avro binary bin against the schema identified by id and returns
// its compact JSON encoding.
func (a *avroSchemas) decode(ctx context.Context, id int, bin []byte) ([]byte, error) {
	s, err := a.get(ctx, id)
	if err != nil {
		return nil, err
	}
	dec, err := s.decoder()
	if err != nil {
		return nil, err
	}
	val, err := dec.Decode(bytes.NewReader(bin))
	if err != nil {
		return nil, fmt.Errorf("avro-decode: %w", err)
	}
	doc, err := jsonFromValue(val, s.schema, s.names, "")
	if err != nil {
		return nil, fmt.Errorf("map avro to json: %w", err)
	}
	out, err := marshalNoEscape(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal decoded json: %w", err)
	}
	return out, nil
}

// marshalNoEscape compact-marshals v with HTML escaping disabled, matching the
// JSON-canonicalization path (see canonicalJSON in client.go): json.Marshal
// would rewrite "<", ">", and "&" in string values as < / > / &,
// mutating decoded payloads as they cross the module boundary.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder appends a trailing newline; strip it so the output matches
	// what json.Marshal would have produced.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// unmarshalJSONValue decodes raw into a Go value using json.Number so that
// integers above 2^53 and high-precision decimals are not silently coerced to
// float64 before they reach the schema-driven mapping.
func unmarshalJSONValue(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, fmt.Errorf("payload is not valid JSON: %w", err)
	}
	// Reject trailing tokens (e.g. `{} {}`) so the caller can't smuggle extra
	// documents past the validator. Decoding a second time and requiring io.EOF
	// is unambiguous, unlike Decoder.More() whose contract is about iterating
	// the *current* array/object rather than the top-level stream.
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("payload has trailing data after JSON value")
	}
	return v, nil
}

// nameTable maps an Avro named type's full name to its schema so that an
// avro.Ref encountered while walking can be resolved back to its definition,
// matching Avro's name-resolution rules (namespace inherited from the
// enclosing named type).
type nameTable struct {
	m map[string]avro.Schema
}

func buildNameTable(s avro.Schema) *nameTable {
	t := &nameTable{m: make(map[string]avro.Schema)}
	t.collect(s, "")
	return t
}

func (t *nameTable) collect(s avro.Schema, ns string) {
	switch x := s.(type) {
	case avro.Record:
		rns := nsOr(x.Namespace, ns)
		t.m[fullName(x.Name, rns)] = x
		for _, f := range x.Fields {
			if f != nil {
				t.collect(f.Type, rns)
			}
		}
	case avro.Enum:
		t.m[fullName(x.Name, nsOr(x.Namespace, ns))] = x
	case avro.Fixed:
		t.m[fullName(x.Name, nsOr(x.Namespace, ns))] = x
	case avro.Array:
		t.collect(x.Items, ns)
	case avro.Map:
		t.collect(x.Values, ns)
	case avro.Union:
		for _, b := range x.Types {
			t.collect(b, ns)
		}
	}
}

func (t *nameTable) resolve(ref avro.Ref, ns string) (avro.Schema, bool) {
	if s, ok := t.m[fullName(ref.Name, nsOr(ref.Namespace, ns))]; ok {
		return s, true
	}
	s, ok := t.m[ref.Name]
	return s, ok
}

// fullName joins a namespace and a name the way Avro does: a name already
// containing a dot is fully qualified, and an empty namespace yields the bare
// name.
func fullName(name, ns string) string {
	if ns == "" || strings.Contains(name, ".") {
		return name
	}
	return ns + "." + name
}

func nsOr(declared, inherited string) string {
	if declared != "" {
		return declared
	}
	return inherited
}

// valueFromJSON walks a JSON-decoded Go value alongside its Avro schema and
// produces the matching generic.Value. ns is the Avro namespace inherited from
// the enclosing named type, used to resolve Refs and union branch names.
func valueFromJSON(v any, s avro.Schema, t *nameTable, ns string) (generic.Value, error) {
	switch sc := s.(type) {
	case avro.Null:
		if v != nil {
			return nil, fmt.Errorf("expected null, got %T", v)
		}
		return generic.Null{}, nil
	case avro.Boolean:
		b, ok := v.(bool)
		if !ok {
			return nil, typeErr("boolean", v)
		}
		return generic.Bool(b), nil
	case avro.Int:
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		if n < math.MinInt32 || n > math.MaxInt32 {
			return nil, fmt.Errorf("value %d out of range for int", n)
		}
		return generic.Int(int32(n)), nil
	case avro.Long:
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		return generic.Long(n), nil
	case avro.Float:
		f, err := toFloat64(v)
		if err != nil {
			return nil, err
		}
		return generic.Float(float32(f)), nil
	case avro.Double:
		f, err := toFloat64(v)
		if err != nil {
			return nil, err
		}
		return generic.Double(f), nil
	case avro.Bytes:
		str, ok := v.(string)
		if !ok {
			return nil, typeErr("bytes (json string)", v)
		}
		b, err := bytesFromJSONString(str)
		if err != nil {
			return nil, err
		}
		return generic.Bytes(b), nil
	case avro.String:
		str, ok := v.(string)
		if !ok {
			return nil, typeErr("string", v)
		}
		return generic.String(str), nil
	case avro.Ref:
		resolved, ok := t.resolve(sc, ns)
		if !ok {
			return nil, fmt.Errorf("unresolved schema reference %q", sc.Name)
		}
		return valueFromJSON(v, resolved, t, ns)
	case avro.Record:
		return recordFromJSON(v, sc, t, ns)
	case avro.Enum:
		str, ok := v.(string)
		if !ok {
			return nil, typeErr(fmt.Sprintf("enum %q symbol", sc.Name), v)
		}
		if !slices.Contains(sc.Symbols, str) {
			return nil, fmt.Errorf("enum %q has no symbol %q", sc.Name, str)
		}
		return generic.Enum{Symbol: str}, nil
	case avro.Array:
		arr, ok := v.([]any)
		if !ok {
			return nil, typeErr("array", v)
		}
		out := make(generic.Array, len(arr))
		for i, item := range arr {
			iv, err := valueFromJSON(item, sc.Items, t, ns)
			if err != nil {
				return nil, fmt.Errorf("array[%d]: %w", i, err)
			}
			out[i] = iv
		}
		return out, nil
	case avro.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, typeErr("map object", v)
		}
		out := make(generic.Map, len(obj))
		for k, mv := range obj {
			vv, err := valueFromJSON(mv, sc.Values, t, ns)
			if err != nil {
				return nil, fmt.Errorf("map[%q]: %w", k, err)
			}
			out[k] = vv
		}
		return out, nil
	case avro.Union:
		return unionFromJSON(v, sc, t, ns)
	default:
		return nil, fmt.Errorf("Avro type %T is not supported in AVRO mode", s)
	}
}

func recordFromJSON(v any, rec avro.Record, t *nameTable, ns string) (generic.Value, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, typeErr(fmt.Sprintf("record %q object", rec.Name), v)
	}
	rns := nsOr(rec.Namespace, ns)
	fields := make([]generic.Field, len(rec.Fields))
	for i, f := range rec.Fields {
		if f == nil {
			return nil, fmt.Errorf("record %q has a nil field at index %d", rec.Name, i)
		}
		fv, present := obj[f.Name]
		if !present {
			if !f.HasDefault {
				return nil, fmt.Errorf("record %q is missing field %q", rec.Name, f.Name)
			}
			fv = f.Default
		}
		val, err := valueFromJSON(fv, f.Type, t, rns)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", f.Name, err)
		}
		fields[i] = generic.Field{Name: f.Name, Value: val}
	}
	return generic.Record{Fields: fields}, nil
}

// unionFromJSON applies the Avro JSON encoding for unions: a bare null selects
// the null branch; any other value must be a single-key object
// {"<typename>": value} naming the branch.
func unionFromJSON(v any, u avro.Union, t *nameTable, ns string) (generic.Value, error) {
	if v == nil {
		for i, b := range u.Types {
			if _, ok := b.(avro.Null); ok {
				return generic.Union{Index: i, Value: generic.Null{}}, nil
			}
		}
		return nil, fmt.Errorf("null value but union has no null branch")
	}
	obj, ok := v.(map[string]any)
	if !ok || len(obj) != 1 {
		return nil, fmt.Errorf(`union value must be a single-key {"<type>": value} object (Avro JSON encoding), got %T`, v)
	}
	var key string
	var inner any
	for k, vv := range obj {
		key, inner = k, vv
	}
	for i, b := range u.Types {
		if unionBranchName(b, ns) == key {
			val, err := valueFromJSON(inner, b, t, ns)
			if err != nil {
				return nil, err
			}
			return generic.Union{Index: i, Value: val}, nil
		}
	}
	return nil, fmt.Errorf("union branch %q is not present in the schema", key)
}

// jsonFromValue walks a decoded generic.Value alongside its schema and returns
// a Go value ready for json.Marshal, emitting the same Avro JSON encoding that
// valueFromJSON consumes.
func jsonFromValue(v generic.Value, s avro.Schema, t *nameTable, ns string) (any, error) {
	switch sc := s.(type) {
	case avro.Null:
		return nil, nil
	case avro.Boolean:
		b, ok := v.(generic.Bool)
		if !ok {
			return nil, decodeTypeErr("boolean", v)
		}
		return bool(b), nil
	case avro.Int:
		i, ok := v.(generic.Int)
		if !ok {
			return nil, decodeTypeErr("int", v)
		}
		return int32(i), nil
	case avro.Long:
		l, ok := v.(generic.Long)
		if !ok {
			return nil, decodeTypeErr("long", v)
		}
		return int64(l), nil
	case avro.Float:
		f, ok := v.(generic.Float)
		if !ok {
			return nil, decodeTypeErr("float", v)
		}
		return float32(f), nil
	case avro.Double:
		d, ok := v.(generic.Double)
		if !ok {
			return nil, decodeTypeErr("double", v)
		}
		return float64(d), nil
	case avro.Bytes:
		b, ok := v.(generic.Bytes)
		if !ok {
			return nil, decodeTypeErr("bytes", v)
		}
		return bytesToJSONString(b), nil
	case avro.String:
		str, ok := v.(generic.String)
		if !ok {
			return nil, decodeTypeErr("string", v)
		}
		return string(str), nil
	case avro.Ref:
		resolved, ok := t.resolve(sc, ns)
		if !ok {
			return nil, fmt.Errorf("unresolved schema reference %q", sc.Name)
		}
		return jsonFromValue(v, resolved, t, ns)
	case avro.Record:
		rec, ok := v.(generic.Record)
		if !ok {
			return nil, decodeTypeErr(fmt.Sprintf("record %q", sc.Name), v)
		}
		if len(rec.Fields) != len(sc.Fields) {
			return nil, fmt.Errorf("record %q: decoded %d fields, schema has %d", sc.Name, len(rec.Fields), len(sc.Fields))
		}
		rns := nsOr(sc.Namespace, ns)
		out := make(map[string]any, len(sc.Fields))
		for i, f := range sc.Fields {
			fv, err := jsonFromValue(rec.Fields[i].Value, f.Type, t, rns)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", f.Name, err)
			}
			out[f.Name] = fv
		}
		return out, nil
	case avro.Enum:
		en, ok := v.(generic.Enum)
		if !ok {
			return nil, decodeTypeErr(fmt.Sprintf("enum %q", sc.Name), v)
		}
		return en.Symbol, nil
	case avro.Array:
		arr, ok := v.(generic.Array)
		if !ok {
			return nil, decodeTypeErr("array", v)
		}
		out := make([]any, len(arr))
		for i, item := range arr {
			iv, err := jsonFromValue(item, sc.Items, t, ns)
			if err != nil {
				return nil, fmt.Errorf("array[%d]: %w", i, err)
			}
			out[i] = iv
		}
		return out, nil
	case avro.Map:
		mp, ok := v.(generic.Map)
		if !ok {
			return nil, decodeTypeErr("map", v)
		}
		out := make(map[string]any, len(mp))
		for k, mv := range mp {
			vv, err := jsonFromValue(mv, sc.Values, t, ns)
			if err != nil {
				return nil, fmt.Errorf("map[%q]: %w", k, err)
			}
			out[k] = vv
		}
		return out, nil
	case avro.Union:
		un, ok := v.(generic.Union)
		if !ok {
			return nil, decodeTypeErr("union", v)
		}
		if un.Index < 0 || un.Index >= len(sc.Types) {
			return nil, fmt.Errorf("union branch index %d out of range", un.Index)
		}
		branch := sc.Types[un.Index]
		if _, isNull := branch.(avro.Null); isNull {
			return nil, nil
		}
		inner, err := jsonFromValue(un.Value, branch, t, ns)
		if err != nil {
			return nil, err
		}
		return map[string]any{unionBranchName(branch, ns): inner}, nil
	default:
		return nil, fmt.Errorf("Avro type %T is not supported in AVRO mode", s)
	}
}

// unionBranchName returns the type name used as the key in the Avro JSON
// encoding of a non-null union branch.
func unionBranchName(s avro.Schema, ns string) string {
	switch x := s.(type) {
	case avro.Null:
		return "null"
	case avro.Boolean:
		return "boolean"
	case avro.Int:
		return "int"
	case avro.Long:
		return "long"
	case avro.Float:
		return "float"
	case avro.Double:
		return "double"
	case avro.Bytes:
		return "bytes"
	case avro.String:
		return "string"
	case avro.Array:
		return "array"
	case avro.Map:
		return "map"
	case avro.Record:
		return fullName(x.Name, nsOr(x.Namespace, ns))
	case avro.Enum:
		return fullName(x.Name, nsOr(x.Namespace, ns))
	case avro.Fixed:
		return fullName(x.Name, nsOr(x.Namespace, ns))
	case avro.Ref:
		return fullName(x.Name, nsOr(x.Namespace, ns))
	default:
		return fmt.Sprintf("%T", s)
	}
}

// ---- small helpers ----

func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		return n.Int64()
	case float64:
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("expected an integer, got %v", n)
		}
		return int64(n), nil
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	default:
		return 0, fmt.Errorf("expected a JSON number, got %T", v)
	}
}

func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case json.Number:
		return n.Float64()
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("expected a JSON number, got %T", v)
	}
}

// bytesFromJSONString decodes the Avro JSON encoding of bytes: a string whose
// characters are the byte values (code points 0-255).
func bytesFromJSONString(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			return nil, fmt.Errorf("bytes JSON string contains code point %U > 0xFF", r)
		}
		out = append(out, byte(r))
	}
	return out, nil
}

func bytesToJSONString(b []byte) string {
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs)
}

func typeErr(want string, got any) error {
	return fmt.Errorf("expected JSON %s, got %T", want, got)
}

func decodeTypeErr(want string, got generic.Value) error {
	return fmt.Errorf("decoded value is %T, expected %s", got, want)
}
