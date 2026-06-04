// Package skill is the deterministic, gen-AI-free rendering engine behind the
// skill-gen module. It imports nothing from dagger, so the determinism-critical
// logic — the introspection model, top-table ranking, markdown rendering, and
// verification — is unit-testable with `go test -race` and no engine.
//
// The introspection queries (run by the parent module against a live database)
// return one JSON array of row objects per topic. Parse* turns those bytes into
// the typed Model; Render turns the Model into the generated skill's file tree.
package skill

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Model is the complete introspection snapshot plus the connection coordinates
// baked into the generated skill's runtime scripts. It carries no run-varying
// data (no timestamps), so an unchanged schema renders byte-identical output.
type Model struct {
	// Connection coordinates, baked into scripts/query.sh and .env.example.
	DBName string
	Host   string
	Port   int
	User   string

	// Raw introspection rows, each in the deterministic order the queries
	// imposed (ORDER BY schema, table, ordinal — see the parent module).
	Columns     []Column
	PrimaryKeys []ColumnRef
	ForeignKeys []ForeignKey
	Indexes     []Index
	Enums       []EnumValue
	Views       []View
	Comments    []Comment
}

// Column is one row of the tables introspection query.
type Column struct {
	Schema   string `json:"schema"`
	Table    string `json:"table"`
	Name     string `json:"column"`
	Type     string `json:"type"`
	Nullable string `json:"nullable"` // "YES" / "NO" (information_schema spelling)
	Default  string `json:"default_value"`
}

// ColumnRef names a single (schema, table, column) — used for primary keys.
type ColumnRef struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Column string `json:"column"`
}

// ForeignKey is one referencing column paired with its referenced column.
// Composite FKs yield multiple rows (one per column position).
type ForeignKey struct {
	Schema    string `json:"schema"`
	Table     string `json:"table"`
	Column    string `json:"column"`
	RefSchema string `json:"ref_schema"`
	RefTable  string `json:"ref_table"`
	RefColumn string `json:"ref_column"`
}

// Index is one row of the indexes introspection query.
type Index struct {
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Name       string `json:"index"`
	Definition string `json:"definition"`
}

// EnumValue is one (schema, enum type, label) row, in enumsortorder.
type EnumValue struct {
	Schema string `json:"schema"`
	Type   string `json:"type"`
	Label  string `json:"label"`
}

// View is one view with its full definition.
type View struct {
	Schema     string `json:"schema"`
	Name       string `json:"view"`
	Definition string `json:"definition"`
}

// Comment is a table-level (Column == "") or column-level COMMENT.
type Comment struct {
	Schema   string `json:"schema"`
	Relation string `json:"relation"`
	Column   string `json:"column"`
	Text     string `json:"comment"`
}

// parseRows unmarshals a JSON array of row objects into dst (a *[]T). It is the
// single seam between the dagger-side QueryJSON output and this pure package.
func parseRows[T any](topic string, data []byte, dst *[]T) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("parse %s introspection: %w", topic, err)
	}
	return nil
}

// ParseColumns parses the tables introspection result.
func (m *Model) ParseColumns(data []byte) error { return parseRows("tables", data, &m.Columns) }

// ParsePrimaryKeys parses the primary_keys introspection result.
func (m *Model) ParsePrimaryKeys(data []byte) error {
	return parseRows("primary_keys", data, &m.PrimaryKeys)
}

// ParseForeignKeys parses the foreign_keys introspection result.
func (m *Model) ParseForeignKeys(data []byte) error {
	return parseRows("foreign_keys", data, &m.ForeignKeys)
}

// ParseIndexes parses the indexes introspection result.
func (m *Model) ParseIndexes(data []byte) error { return parseRows("indexes", data, &m.Indexes) }

// ParseEnums parses the enums introspection result.
func (m *Model) ParseEnums(data []byte) error { return parseRows("enums", data, &m.Enums) }

// ParseViews parses the views introspection result.
func (m *Model) ParseViews(data []byte) error { return parseRows("views", data, &m.Views) }

// ParseComments parses the comments introspection result.
func (m *Model) ParseComments(data []byte) error { return parseRows("comments", data, &m.Comments) }

// Table is a derived, render-ready view of one base table: its columns in
// ordinal order plus its primary-key column list.
type Table struct {
	Schema  string
	Name    string
	Columns []Column
	PK      []string
}

// key is the schema-qualified identity used for sorting and lookups.
func (t Table) key() string { return t.Schema + "\x00" + t.Name }

// Tables groups the raw Column rows into per-table structs sorted by
// (schema, table) ASC, with primary-key columns attached. Column order within
// a table is preserved from the introspection result (ordinal_position).
func (m *Model) Tables() []Table {
	byKey := map[string]*Table{}
	var order []string
	for _, c := range m.Columns {
		tk := c.Schema + "\x00" + c.Table
		t, ok := byKey[tk]
		if !ok {
			t = &Table{Schema: c.Schema, Name: c.Table}
			byKey[tk] = t
			order = append(order, tk)
		}
		t.Columns = append(t.Columns, c)
	}
	for _, pk := range m.PrimaryKeys {
		tk := pk.Schema + "\x00" + pk.Table
		if t, ok := byKey[tk]; ok {
			t.PK = append(t.PK, pk.Column)
		}
	}
	tables := make([]Table, 0, len(order))
	for _, tk := range order {
		tables = append(tables, *byKey[tk])
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].key() < tables[j].key() })
	return tables
}

// fkNotes maps a (schema, table, column) to its "ref_table.ref_column" arrow
// targets, used to annotate the notes cell in references/tables.md.
func (m *Model) fkNotes() map[string][]string {
	out := map[string][]string{}
	for _, fk := range m.ForeignKeys {
		k := fk.Schema + "\x00" + fk.Table + "\x00" + fk.Column
		out[k] = append(out[k], fk.RefTable+"."+fk.RefColumn)
	}
	return out
}

// tableComment returns the table-level COMMENT for (schema, table), if any.
func (m *Model) tableComment(schema, table string) string {
	for _, c := range m.Comments {
		if c.Schema == schema && c.Relation == table && c.Column == "" {
			return c.Text
		}
	}
	return ""
}

// columnComment returns the column-level COMMENT for (schema, table, column).
func (m *Model) columnComment(schema, table, column string) string {
	for _, c := range m.Comments {
		if c.Schema == schema && c.Relation == table && c.Column == column {
			return c.Text
		}
	}
	return ""
}
