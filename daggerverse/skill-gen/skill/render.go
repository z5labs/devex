package skill

import (
	"embed"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

//go:embed templates/SKILL.md.tmpl templates/README.md.tmpl templates/query.sh templates/env.example
var templatesFS embed.FS

// Generated-file relative paths. The result map Render returns is keyed by
// these; references/enums.md is present only when the schema defines enums.
const (
	PathSKILL         = "SKILL.md"
	PathREADME        = "README.md"
	PathQuery         = "scripts/query.sh"
	PathEnvExample    = "scripts/.env.example"
	PathTables        = "references/tables.md"
	PathRelationships = "references/relationships.md"
	PathViews         = "references/views.md"
	PathIndexes       = "references/indexes.md"
	PathEnums         = "references/enums.md"
)

// topTableCount is how many tables feed the SKILL.md/README "top tables" list.
const topTableCount = 5

// tmplData is the substitution context for the SKILL.md and README templates.
type tmplData struct {
	DB             string
	SkillDir       string
	TopTablesProse string
	TopTable       string
	TableCount     int
	ViewCount      int
	EnumCount      int
	TablesPhrase   string // e.g. "4 tables" / "1 table"
	ViewsPhrase    string // e.g. "1 view"
	EnumsPhrase    string // e.g. "2 enums"
	HasEnums       bool
	SchemaOverview string
	TableBullets   string
	RegenCommand   string
	RegenChanges   string
}

// Render turns the introspection Model into the generated skill's file tree,
// keyed by relative path. Output is a pure function of the Model — no
// timestamps or other run-varying data — so an unchanged schema renders
// byte-identical bytes and regeneration diffs cleanly.
func Render(m *Model) (map[string]string, error) {
	tables := m.Tables()
	enumTypes := m.enumTypes()
	skillDir := "pg-" + m.DBName

	data := tmplData{
		DB:             m.DBName,
		SkillDir:       skillDir,
		TopTablesProse: m.topTablesProse(),
		TopTable:       m.topTableSQL(),
		TableCount:     len(tables),
		ViewCount:      len(m.Views),
		EnumCount:      len(enumTypes),
		TablesPhrase:   countPhrase(len(tables), "table"),
		ViewsPhrase:    countPhrase(len(m.Views), "view"),
		EnumsPhrase:    countPhrase(len(enumTypes), "enum"),
		HasEnums:       len(enumTypes) > 0,
		SchemaOverview: m.schemaOverview(tables, enumTypes),
		TableBullets:   m.tableBullets(tables),
		RegenCommand:   m.regenCommand(skillDir, "export --path "+skillDir),
		RegenChanges:   m.regenCommand(skillDir, "changes --from "+skillDir+" is-empty"),
	}

	skillMD, err := renderTemplate("templates/SKILL.md.tmpl", data)
	if err != nil {
		return nil, err
	}
	readmeMD, err := renderTemplate("templates/README.md.tmpl", data)
	if err != nil {
		return nil, err
	}

	queryRaw, err := templatesFS.ReadFile("templates/query.sh")
	if err != nil {
		return nil, err
	}
	envRaw, err := templatesFS.ReadFile("templates/env.example")
	if err != nil {
		return nil, err
	}
	subst := strings.NewReplacer(
		"<host>", m.Host,
		"<port>", fmt.Sprintf("%d", m.Port),
		"<user>", m.User,
		"<dbname>", m.DBName,
	)

	files := map[string]string{
		PathSKILL:         skillMD,
		PathREADME:        readmeMD,
		PathQuery:         subst.Replace(string(queryRaw)),
		PathEnvExample:    subst.Replace(string(envRaw)),
		PathTables:        m.tablesRef(tables),
		PathRelationships: m.relationshipsRef(),
		PathViews:         m.viewsRef(),
		PathIndexes:       m.indexesRef(),
	}
	if len(enumTypes) > 0 {
		files[PathEnums] = m.enumsRef(enumTypes)
	}
	return files, nil
}

func renderTemplate(name string, data tmplData) (string, error) {
	t, err := template.New(name).ParseFS(templatesFS, name)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var sb strings.Builder
	if err := t.ExecuteTemplate(&sb, baseName(name), data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	return sb.String(), nil
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// regenCommand builds a deterministic `dagger call` regeneration command with
// the given trailing subcommand. The password is referenced as
// env:PGPASSWORD — never the secret value — so the command is safe to commit.
func (m *Model) regenCommand(_ string, trailer string) string {
	return fmt.Sprintf(
		"dagger call skill-gen postgres --host %s --port %d --user %s --db %s --password env:PGPASSWORD %s",
		m.Host, m.Port, m.User, m.DBName, trailer,
	)
}

// enumTypes returns the distinct (schema, type) enum identities, sorted.
func (m *Model) enumTypes() []TableRef {
	seen := map[string]bool{}
	var out []TableRef
	for _, e := range m.Enums {
		k := e.Schema + "\x00" + e.Type
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, TableRef{Schema: e.Schema, Table: e.Type})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Table < out[j].Table
	})
	return out
}

func (m *Model) topTablesProse() string {
	top := m.TopTables(topTableCount)
	names := make([]string, len(top))
	for i, t := range top {
		names[i] = t.Table
	}
	return strings.Join(names, ", ")
}

// topTableSQL formats the single highest-ranked table as a schema-qualified,
// double-quoted SQL identifier so the README's row-count sample is runnable
// for any introspected name. Falls back to a clearly-degenerate (but non-
// placeholder) value when the schema has no tables.
func (m *Model) topTableSQL() string {
	top := m.TopTables(1)
	if len(top) == 0 {
		return `"public"."(no tables found)"`
	}
	return fmt.Sprintf("%q.%q", top[0].Schema, top[0].Table)
}

func (m *Model) schemaOverview(tables []Table, enumTypes []TableRef) string {
	schemas := distinctSchemas(tables)
	var sb strings.Builder
	fmt.Fprintf(&sb, "The `%s` Postgres database has %d %s", m.DBName, len(tables), plural(len(tables), "table", "tables"))
	switch len(schemas) {
	case 0:
	case 1:
		fmt.Fprintf(&sb, " in the `%s` schema", schemas[0])
	default:
		fmt.Fprintf(&sb, " across %d schemas (%s)", len(schemas), strings.Join(schemas, ", "))
	}
	fmt.Fprintf(&sb, ", %d %s", len(m.Views), plural(len(m.Views), "view", "views"))
	if len(enumTypes) > 0 {
		fmt.Fprintf(&sb, ", and %d enum %s", len(enumTypes), plural(len(enumTypes), "type", "types"))
	}
	sb.WriteString(".")
	if prose := m.topTablesProse(); prose != "" {
		fmt.Fprintf(&sb, " Most-referenced tables: %s.", prose)
	}
	return sb.String()
}

// tableBullets renders the compact, schema-grouped table list for SKILL.md.
func (m *Model) tableBullets(tables []Table) string {
	var sb strings.Builder
	var curSchema string
	first := true
	for _, t := range tables {
		if first || t.Schema != curSchema {
			if !first {
				sb.WriteString("\n")
			}
			fmt.Fprintf(&sb, "#### %s\n\n", t.Schema)
			curSchema = t.Schema
			first = false
		}
		pk := "none"
		if len(t.PK) > 0 {
			pk = strings.Join(t.PK, ", ")
		}
		fmt.Fprintf(&sb, "- %s (%d %s, PK: %s)\n", t.Name, len(t.Columns), plural(len(t.Columns), "col", "cols"), pk)
	}
	if sb.Len() == 0 {
		return "_No tables._\n"
	}
	return sb.String()
}

func (m *Model) tablesRef(tables []Table) string {
	if len(tables) == 0 {
		return "_No tables._\n"
	}
	fkNotes := m.fkNotes()
	var sb strings.Builder
	for i, t := range tables {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "## %s.%s\n\n", t.Schema, t.Name)
		if c := m.tableComment(t.Schema, t.Name); c != "" {
			fmt.Fprintf(&sb, "%s\n\n", mdText(c))
		}
		pkSet := map[string]bool{}
		for _, c := range t.PK {
			pkSet[c] = true
		}
		sb.WriteString("| column | type | null | default | notes |\n")
		sb.WriteString("|---|---|---|---|---|\n")
		for _, col := range t.Columns {
			var notes []string
			if pkSet[col.Name] {
				notes = append(notes, "PK")
			}
			for _, ref := range fkNotes[col.Schema+"\x00"+col.Table+"\x00"+col.Name] {
				notes = append(notes, "FK → "+ref)
			}
			if cc := m.columnComment(t.Schema, t.Name, col.Name); cc != "" {
				notes = append(notes, mdCell(cc))
			}
			fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s |\n",
				mdCell(col.Name), mdCell(col.Type), mdCell(col.Nullable),
				mdCell(col.Default), strings.Join(notes, "; "))
		}
	}
	return sb.String()
}

func (m *Model) relationshipsRef() string {
	if len(m.ForeignKeys) == 0 {
		return "_No foreign keys._\n"
	}
	var sb strings.Builder
	for _, fk := range m.ForeignKeys {
		fmt.Fprintf(&sb, "%s.%s → %s.%s\n", fk.Table, fk.Column, fk.RefTable, fk.RefColumn)
	}
	return sb.String()
}

func (m *Model) viewsRef() string {
	if len(m.Views) == 0 {
		return "_No views._\n"
	}
	views := make([]View, len(m.Views))
	copy(views, m.Views)
	sort.Slice(views, func(i, j int) bool {
		if views[i].Schema != views[j].Schema {
			return views[i].Schema < views[j].Schema
		}
		return views[i].Name < views[j].Name
	})
	var sb strings.Builder
	for i, v := range views {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "## %s.%s\n\n", v.Schema, v.Name)
		if c := m.viewComment(v.Schema, v.Name); c != "" {
			fmt.Fprintf(&sb, "%s\n\n", mdText(c))
		}
		fmt.Fprintf(&sb, "```sql\n%s\n```\n", strings.TrimRight(v.Definition, "\n"))
	}
	return sb.String()
}

func (m *Model) viewComment(schema, view string) string {
	for _, c := range m.Comments {
		if c.Schema == schema && c.Relation == view && c.Column == "" {
			return c.Text
		}
	}
	return ""
}

func (m *Model) indexesRef() string {
	if len(m.Indexes) == 0 {
		return "_No indexes._\n"
	}
	var sb strings.Builder
	for _, ix := range m.Indexes {
		fmt.Fprintf(&sb, "%s.%s: %s\n", ix.Schema, ix.Table, mdText(ix.Definition))
	}
	return sb.String()
}

func (m *Model) enumsRef(enumTypes []TableRef) string {
	values := map[string][]string{}
	for _, e := range m.Enums {
		k := e.Schema + "\x00" + e.Type
		values[k] = append(values[k], e.Label)
	}
	var sb strings.Builder
	for i, et := range enumTypes {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "## %s.%s\n", et.Schema, et.Table)
		for _, v := range values[et.Schema+"\x00"+et.Table] {
			fmt.Fprintf(&sb, "- %s\n", v)
		}
	}
	return sb.String()
}

func distinctSchemas(tables []Table) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tables {
		if !seen[t.Schema] {
			seen[t.Schema] = true
			out = append(out, t.Schema)
		}
	}
	sort.Strings(out)
	return out
}

// countPhrase renders a count with its noun, pluralized with a trailing "s".
func countPhrase(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
