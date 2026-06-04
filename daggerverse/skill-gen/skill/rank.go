package skill

import "sort"

// TableRef is a schema-qualified table identity from the top-table ranking.
type TableRef struct {
	Schema string
	Table  string
}

// TopTables computes the deterministic top-table ranking used in both the
// SKILL.md frontmatter description and the README. The rule (reproduced from
// the original postgres-skill-creator) sorts all base tables by:
//
//  1. FK-referenced-row count DESC — how many foreign_keys rows point at this
//     table (composite FKs contribute multiple rows; that's intentional —
//     a composite-FK target is a higher-traffic join target).
//  2. column count DESC.
//  3. (schema, table) ASC — final lexicographic tie-break.
//
// It returns at most n entries (fewer if the schema has fewer tables).
func (m *Model) TopTables(n int) []TableRef {
	tables := m.Tables()

	refCount := map[string]int{}
	for _, fk := range m.ForeignKeys {
		refCount[fk.RefSchema+"\x00"+fk.RefTable]++
	}

	ranked := make([]Table, len(tables))
	copy(ranked, tables)
	sort.SliceStable(ranked, func(i, j int) bool {
		ri := refCount[ranked[i].Schema+"\x00"+ranked[i].Name]
		rj := refCount[ranked[j].Schema+"\x00"+ranked[j].Name]
		if ri != rj {
			return ri > rj
		}
		if len(ranked[i].Columns) != len(ranked[j].Columns) {
			return len(ranked[i].Columns) > len(ranked[j].Columns)
		}
		return ranked[i].key() < ranked[j].key()
	})

	if n > len(ranked) {
		n = len(ranked)
	}
	out := make([]TableRef, 0, n)
	for _, t := range ranked[:n] {
		out = append(out, TableRef{Schema: t.Schema, Table: t.Name})
	}
	return out
}
