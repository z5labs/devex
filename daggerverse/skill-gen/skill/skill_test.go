package skill

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files under testdata/golden")

// fixtureModel is a deterministic multi-schema snapshot that exercises every
// rendering path: two schemas, FK hubs (for ranking), a composite FK, an enum,
// a view, indexes, and table/column comments.
func fixtureModel() *Model {
	m := &Model{DBName: "shop", Host: "db.internal", Port: 5432, User: "analyst"}

	m.Columns = []Column{
		// public.users (4 cols) — referenced by orders.user_id and analytics.sessions.user_id
		{"public", "users", "id", "bigint", "NO", "nextval('users_id_seq'::regclass)"},
		{"public", "users", "email", "text", "NO", ""},
		{"public", "users", "name", "text", "YES", ""},
		{"public", "users", "created_at", "timestamptz", "NO", "now()"},
		// public.orders (5 cols) — referenced by order_items.order_id
		{"public", "orders", "id", "bigint", "NO", "nextval('orders_id_seq'::regclass)"},
		{"public", "orders", "user_id", "bigint", "NO", ""},
		{"public", "orders", "status", "order_status", "NO", "'pending'::order_status"},
		{"public", "orders", "total", "numeric", "NO", "0"},
		{"public", "orders", "placed_at", "timestamptz", "NO", "now()"},
		// public.order_items (3 cols) — composite PK + composite-ish FKs
		{"public", "order_items", "order_id", "bigint", "NO", ""},
		{"public", "order_items", "line_no", "integer", "NO", ""},
		{"public", "order_items", "sku", "text", "NO", ""},
		// analytics.sessions (3 cols)
		{"analytics", "sessions", "id", "bigint", "NO", ""},
		{"analytics", "sessions", "user_id", "bigint", "NO", ""},
		{"analytics", "sessions", "started_at", "timestamptz", "NO", "now()"},
	}
	m.PrimaryKeys = []ColumnRef{
		{"public", "users", "id"},
		{"public", "orders", "id"},
		{"public", "order_items", "order_id"},
		{"public", "order_items", "line_no"},
		{"analytics", "sessions", "id"},
	}
	m.ForeignKeys = []ForeignKey{
		{"public", "orders", "user_id", "public", "users", "id"},
		{"public", "order_items", "order_id", "public", "orders", "id"},
		{"analytics", "sessions", "user_id", "public", "users", "id"},
	}
	m.Indexes = []Index{
		{"public", "orders", "orders_pkey", "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)"},
		{"public", "users", "users_email_key", "CREATE UNIQUE INDEX users_email_key ON public.users USING btree (email)"},
		{"public", "users", "users_pkey", "CREATE UNIQUE INDEX users_pkey ON public.users USING btree (id)"},
	}
	m.Enums = []EnumValue{
		{"public", "order_status", "pending"},
		{"public", "order_status", "shipped"},
		{"public", "order_status", "delivered"},
	}
	m.Views = []View{
		{"public", "active_users", "SELECT u.id,\n    u.email\n   FROM users u\n  WHERE (u.created_at > (now() - '30 days'::interval))"},
	}
	m.Comments = []Comment{
		{"public", "users", "", "Customer accounts."},
		{"public", "orders", "total", "Order total in USD."},
		{"public", "active_users", "", "Users active in the last 30 days."},
	}
	return m
}

func TestTopTablesRanking(t *testing.T) {
	m := fixtureModel()
	got := m.TopTables(5)
	want := []TableRef{
		{"public", "users"},       // 2 FK refs
		{"public", "orders"},      // 1 FK ref
		{"analytics", "sessions"}, // 0 refs, 3 cols, (analytics < public)
		{"public", "order_items"}, // 0 refs, 3 cols
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tables, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rank[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTopTableSQL(t *testing.T) {
	if got := fixtureModel().topTableSQL(); got != `"public"."users"` {
		t.Errorf("topTableSQL = %s, want \"public\".\"users\"", got)
	}
}

func TestValidateDBName(t *testing.T) {
	ok := []string{"shop", "my_db", "db-1", "ABC123", "x"}
	bad := []string{"", "my db", "db;drop", "../etc", "pg.main", "naïve", "a/b"}
	for _, s := range ok {
		if err := ValidateDBName(s); err != nil {
			t.Errorf("ValidateDBName(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidateDBName(s); err == nil {
			t.Errorf("ValidateDBName(%q) = nil, want error", s)
		}
	}
}

func TestMarkdownEscaping(t *testing.T) {
	cases := map[string]string{
		"a|b":          `a\|b`,
		"line1\nline2": "line1 line2",
		"  pad\t end ": "pad end",
		"x | y | z":    `x \| y \| z`,
	}
	for in, want := range cases {
		if got := mdCell(in); got != want {
			t.Errorf("mdCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellSingleQuote(t *testing.T) {
	cases := map[string]string{
		`plain`:       `'plain'`,
		`a b`:         `'a b'`,
		`$(whoami)`:   `'$(whoami)'`,
		"`id`":        "'`id`'",
		`it's`:        `'it'\''s'`,
		`a";rm -rf /`: `'a";rm -rf /'`,
	}
	for in, want := range cases {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderEscapesShellMetacharacters pins that host/user values carrying shell
// metacharacters are emitted as inert single-quoted literals — never in a
// ${:=} default word, where the shell would expand them — so the generated
// query.sh and README regen command stay safe to run/paste.
func TestRenderEscapesShellMetacharacters(t *testing.T) {
	m := fixtureModel()
	m.Host = "h$(touch /tmp/pwn)"
	m.User = "u`id`"
	files, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	q := files[PathQuery]
	if !strings.Contains(q, `PGHOST='h$(touch /tmp/pwn)'`) {
		t.Errorf("query.sh did not single-quote host:\n%s", q)
	}
	if !strings.Contains(q, "PGUSER='u`id`'") {
		t.Errorf("query.sh did not single-quote user:\n%s", q)
	}
	// The unsafe context this fix removed: a raw value inside a ${:=} default
	// word undergoes command substitution even within double quotes.
	if strings.Contains(q, "${PGHOST:=") || strings.Contains(q, "${PGUSER:=") {
		t.Errorf("query.sh still assigns host/user via a ${:=} default word:\n%s", q)
	}
	if !strings.Contains(files[PathREADME], `--host 'h$(touch /tmp/pwn)'`) {
		t.Errorf("README regen command did not single-quote host:\n%s", files[PathREADME])
	}
}

// TestRowCountExampleIsShellSafe pins that the README row-count sample wraps the
// SQL in single quotes so the SQL-double-quoted identifier survives, rather than
// nesting double quotes (which collapses "public"."users" and breaks on a table
// name with a space).
func TestRowCountExampleIsShellSafe(t *testing.T) {
	readme := mustRender(t, fixtureModel())[PathREADME]
	want := `bash pg-shop/scripts/query.sh 'SELECT count(*) FROM "public"."users"'`
	if !strings.Contains(readme, want) {
		t.Errorf("README missing single-quoted row-count example %q", want)
	}
	// No double-quoted SELECT example should remain (the broken nested form).
	if strings.Contains(readme, `query.sh "SELECT count(*)`) {
		t.Errorf("README still uses a double-quoted SELECT example:\n%s", readme)
	}
}

// TestRenderCustomPsqlImage pins that a caller-supplied image replaces the
// baked default in both generated script files, and that the README's regen
// command carries the flag forward — without it, the drift check the README
// documents would regenerate with the default image and report false drift.
func TestRenderCustomPsqlImage(t *testing.T) {
	m := fixtureModel()
	m.PsqlImage = "registry.internal:5000/team/psql:16.4"
	files := mustRender(t, m)

	want := `PSQL_IMAGE="${PSQL_IMAGE:-registry.internal:5000/team/psql:16.4}"`
	if !strings.Contains(files[PathQuery], want) {
		t.Errorf("query.sh missing custom psql image line %q:\n%s", want, files[PathQuery])
	}
	if strings.Contains(files[PathQuery], DefaultPsqlImage) {
		t.Errorf("query.sh still carries the default psql image:\n%s", files[PathQuery])
	}
	if !strings.Contains(files[PathEnvExample], "# PSQL_IMAGE=registry.internal:5000/team/psql:16.4") {
		t.Errorf(".env.example missing custom psql image key:\n%s", files[PathEnvExample])
	}
	if !strings.Contains(files[PathREADME], "--psql-image 'registry.internal:5000/team/psql:16.4'") {
		t.Errorf("README regen command missing --psql-image:\n%s", files[PathREADME])
	}
	if err := Verify(files, m.DBName); err != nil {
		t.Errorf("Verify on custom-image tree: %v", err)
	}
}

// TestRenderDefaultPsqlImage pins that an unset PsqlImage renders exactly what
// an explicit default does, so omitting the module param reproduces v1 output.
func TestRenderDefaultPsqlImage(t *testing.T) {
	implicit := mustRender(t, fixtureModel())

	m := fixtureModel()
	m.PsqlImage = DefaultPsqlImage
	explicit := mustRender(t, m)

	for _, p := range []string{PathQuery, PathEnvExample, PathREADME} {
		if implicit[p] != explicit[p] {
			t.Errorf("%s differs between unset and explicitly-default PsqlImage", p)
		}
	}
	want := `PSQL_IMAGE="${PSQL_IMAGE:-` + DefaultPsqlImage + `}"`
	if !strings.Contains(implicit[PathQuery], want) {
		t.Errorf("query.sh missing default psql image line %q:\n%s", want, implicit[PathQuery])
	}
	// The default must not bloat the regen command documented in the README.
	if strings.Contains(implicit[PathREADME], "--psql-image") {
		t.Errorf("README regen command names --psql-image for the default image:\n%s", implicit[PathREADME])
	}
}

// TestValidatePsqlImage pins the charset that keeps a raw-substituted image
// inert inside query.sh's "${PSQL_IMAGE:-…}" default word.
func TestValidatePsqlImage(t *testing.T) {
	valid := []string{
		DefaultPsqlImage,
		"psql",
		"registry.internal:5000/team/psql:16.4",
		"docker.io/alpine/psql@sha256:" + strings.Repeat("a", 64),
	}
	for _, img := range valid {
		if err := ValidatePsqlImage(img); err != nil {
			t.Errorf("ValidatePsqlImage(%q) = %v, want nil", img, err)
		}
	}
	invalid := []string{
		"",
		"psql:17.7 --privileged",       // word splitting
		"$(touch /tmp/pwn)",            // command substitution
		"psql:`id`",                    // backtick substitution
		"psql:${HOME}",                 // parameter expansion
		"psql:17.7}\"; touch /tmp/pwn", // closes the expansion and the string
		`psql:17.7\n`,                  // backslash escape
		"-psql:17.7",                   // leading dash reads as a flag
	}
	for _, img := range invalid {
		if err := ValidatePsqlImage(img); err == nil {
			t.Errorf("ValidatePsqlImage(%q) = nil, want error", img)
		}
	}
}

func mustRender(t *testing.T, m *Model) map[string]string {
	t.Helper()
	files, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return files
}

func TestVerifyPositive(t *testing.T) {
	files, err := Render(fixtureModel())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := Verify(files, "shop"); err != nil {
		t.Fatalf("Verify on good tree: %v", err)
	}
}

func TestVerifyCatchesPlaceholder(t *testing.T) {
	files, _ := Render(fixtureModel())
	files[PathSKILL] += "\nleftover <dbname> here\n"
	if err := Verify(files, "shop"); err == nil {
		t.Error("Verify accepted a leftover <dbname> placeholder")
	}
}

func TestVerifyCatchesDisableModelInvocation(t *testing.T) {
	files, _ := Render(fixtureModel())
	files[PathSKILL] = "---\nname: pg-shop\ndisable-model-invocation: true\n---\nbody\n"
	if err := Verify(files, "shop"); err == nil {
		t.Error("Verify accepted disable-model-invocation in frontmatter")
	}
}

func TestVerifyCatchesWrongName(t *testing.T) {
	files, _ := Render(fixtureModel())
	if err := Verify(files, "other"); err == nil {
		t.Error("Verify accepted SKILL.md whose name doesn't match db")
	}
}

func TestVerifyCatchesMissingFile(t *testing.T) {
	files, _ := Render(fixtureModel())
	delete(files, PathRelationships)
	if err := Verify(files, "shop"); err == nil {
		t.Error("Verify accepted a tree missing references/relationships.md")
	}
}

func TestEnumsConditional(t *testing.T) {
	// With enums: enums.md present and referenced.
	files, _ := Render(fixtureModel())
	if _, ok := files[PathEnums]; !ok {
		t.Error("expected references/enums.md when enums exist")
	}
	// Without enums: enums.md absent and not advertised anywhere.
	m := fixtureModel()
	m.Enums = nil
	files, _ = Render(m)
	if _, ok := files[PathEnums]; ok {
		t.Error("enums.md should be absent when no enums")
	}
	for _, p := range []string{PathSKILL, PathREADME} {
		for _, sub := range []string{"enums.md", "enum types", "and enums"} {
			if strings.Contains(files[p], sub) {
				t.Errorf("%s still advertises enums (%q) when none exist", p, sub)
			}
		}
	}
}

func TestGoldenRender(t *testing.T) {
	files, err := Render(fixtureModel())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	dir := filepath.Join("testdata", "golden")

	if *update {
		// Wipe and rewrite the golden tree so removed files don't linger.
		_ = os.RemoveAll(dir)
		for p, content := range files {
			full := filepath.Join(dir, p)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Log("golden files updated")
		return
	}

	// Every rendered file matches its golden, and the golden set matches
	// exactly (no extra, no missing).
	var gotPaths, wantPaths []string
	for p := range files {
		gotPaths = append(gotPaths, p)
	}
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		wantPaths = append(wantPaths, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(gotPaths)
	sort.Strings(wantPaths)
	if len(gotPaths) == 0 {
		t.Fatal("no golden files found; run `go test ./skill -update`")
	}
	if !equalStrings(gotPaths, wantPaths) {
		t.Fatalf("file set mismatch:\n got: %v\nwant: %v", gotPaths, wantPaths)
	}
	for p, content := range files {
		want, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("read golden %s: %v", p, err)
			continue
		}
		if string(want) != content {
			t.Errorf("%s differs from golden (run -update to regenerate)", p)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
