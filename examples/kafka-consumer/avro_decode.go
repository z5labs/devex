package main

// avro_decode.go bridges github.com/z5labs/avro-go's decoded generic.Value tree
// back to a json.Marshal-ready Go value.
//
// avro-go decodes the Confluent wire body (Avro *binary*) into a closed
// generic.Value tree; it has no JSON or map[string]any bridge of its own. So we
// walk the generic.Value alongside its schema and emit the Avro-spec JSON
// encoding: a record becomes a map, a union non-null branch becomes a
// single-key {"<typename>": value} object, `bytes` becomes a string with one
// character per byte, and so on. This is the decode-only half of the walker the
// kafka module carries in daggerverse/kafka/avro.go — the reference consumer
// only reads, so the JSON→generic.Value encode direction is omitted.
//
// IMPORTANT: a defect in avro-go itself (avro.ParseJSON, generic.NewDecoder,
// Decode, or binary/schema-compilation correctness) must be filed upstream on
// z5labs/avro-go — it must NOT be worked around here. Only the generic.Value →
// JSON mapping below is ours to fix.

import (
	"fmt"
	"strings"

	"github.com/z5labs/avro-go"
	"github.com/z5labs/avro-go/generic"
)

// nameTable maps an Avro named type's full name to its schema so an avro.Ref
// encountered while walking resolves back to its definition, following Avro's
// name-resolution rules (namespace inherited from the enclosing named type).
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

// jsonFromValue walks a decoded generic.Value alongside its schema and returns
// a Go value ready for json.Marshal, emitting the Avro JSON encoding. ns is the
// Avro namespace inherited from the enclosing named type.
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
		return nil, fmt.Errorf("Avro type %T is not supported", s)
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

// bytesToJSONString renders Avro bytes as the Avro JSON encoding: a string with
// one character per byte (code points 0-255).
func bytesToJSONString(b []byte) string {
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs)
}

func decodeTypeErr(want string, got generic.Value) error {
	return fmt.Errorf("decoded value is %T, expected %s", got, want)
}
