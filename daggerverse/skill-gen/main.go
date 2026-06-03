// Package main implements the skill-gen daggerverse module: deterministic,
// gen-AI-free generation of project-level Claude Code skills by introspecting
// a source system. The first (and only, in this story) source is Postgres.
//
// All determinism-critical logic — the introspection model, top-table
// ranking, markdown rendering, and verification — lives in the pure-Go
// ./skill subpackage (zero dagger import, go test -race-able with no engine).
// This file owns only the Dagger I/O: delegating introspection to the
// postgres module's Client.QueryJSON and assembling the result Directory.
package main

import (
	"context"
	"fmt"
	"sort"

	"dagger/skill-gen/internal/dagger"
	"dagger/skill-gen/skill"
)

// SkillGen is the module's root object.
type SkillGen struct{}

// Postgres introspects the PostgreSQL database at host:port and returns a
// generated `pg-<db>` Claude Code skill as a *dagger.Directory. The function
// never touches the host filesystem — the caller exports the returned tree
// wherever they want (e.g. `export --path .claude/skills/pg-<db>`).
//
// Introspection is delegated to the postgres module's pgx-backed
// Client.QueryJSON; only core types cross this module's boundary
// (*dagger.Secret in, *dagger.Directory out). `db` is validated against
// ^[A-Za-z0-9_-]+$ before any network I/O, because it flows into the skill's
// `name: pg-<db>` frontmatter and into filenames. Any introspection failure
// aborts with a non-zero error and no partial output. Plaintext only (mirrors
// the postgres module's v1); TLS lands in a follow-up.
//
// +cache="never"
func (s *SkillGen) Postgres(
	ctx context.Context,
	host string,
	// +default=5432
	port int,
	user string,
	db string,
	password *dagger.Secret,
) (*dagger.Directory, error) {
	if err := skill.ValidateDBName(db); err != nil {
		return nil, err
	}

	client := dag.Postgres().Client(
		host, user, db, password,
		dag.Postgres().PlaintextClientSecurity(),
		dagger.PostgresClientOpts{Port: port},
	)

	model := &skill.Model{DBName: db, Host: host, Port: port, User: user}
	queries := []struct {
		name  string
		sql   string
		parse func([]byte) error
	}{
		{"tables", tablesSQL, model.ParseColumns},
		{"primary_keys", primaryKeysSQL, model.ParsePrimaryKeys},
		{"foreign_keys", foreignKeysSQL, model.ParseForeignKeys},
		{"indexes", indexesSQL, model.ParseIndexes},
		{"enums", enumsSQL, model.ParseEnums},
		{"views", viewsSQL, model.ParseViews},
		{"comments", commentsSQL, model.ParseComments},
	}
	for _, q := range queries {
		contents, err := client.QueryJSON(q.sql).Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("introspect %s: %w", q.name, err)
		}
		if err := q.parse([]byte(contents)); err != nil {
			return nil, fmt.Errorf("introspect %s: %w", q.name, err)
		}
	}

	files, err := skill.Render(model)
	if err != nil {
		return nil, err
	}
	if err := skill.Verify(files, db); err != nil {
		return nil, err
	}

	// Assemble the result in a deterministic path order so the Directory's
	// content is byte-stable across regenerations of an unchanged schema.
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	dir := dag.Directory()
	for _, p := range paths {
		opts := dagger.DirectoryWithNewFileOpts{}
		if p == skill.PathQuery {
			opts.Permissions = 0o755 // query.sh must be executable
		}
		dir = dir.WithNewFile(p, files[p], opts)
	}
	return dir, nil
}
