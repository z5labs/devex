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
// generated `pg-<db>` Claude Code skill as a *dagger.Directory. The returned
// tree is the skill directory itself (SKILL.md at its root) with no enclosing
// `.claude/skills/` wrapper, so it can be dropped straight into Claude Code,
// Copilot, or any other gen-AI environment. The function never touches the
// host filesystem — the caller exports the tree wherever they want (e.g.
// `export --path pg-<db>` for Copilot, or `export --path .claude/skills/pg-<db>`
// for Claude Code).
//
// Introspection is delegated to the postgres module's pgx-backed
// Client.QueryJSON; only core types cross this module's boundary
// (*dagger.Secret/*dagger.File in, *dagger.Directory out). `db` is validated
// against ^[A-Za-z0-9_-]+$ before any network I/O, because it flows into the
// skill's `name: pg-<db>` frontmatter and into filenames. Any introspection
// failure aborts with a non-zero error and no partial output.
//
// The transport security mode is inferred from the supplied cert params, so it
// can never disagree with the material actually provided:
//
//   - none → plaintext (scram-sha-256 over an unencrypted TCP connection).
//   - serverCa only → one-way TLS (sslmode=verify-full against serverCa).
//   - serverCa + clientCert + clientKey → mTLS; the client presents its leaf
//     to satisfy the server's clientcert=verify-full. The client cert's CN
//     must equal `user`, or the server rejects it with a misleading 28P01.
//
// serverCa and clientCert are public PEM certs (*dagger.File); clientKey is the
// PEM PKCS#8 private key kept as a *dagger.Secret end-to-end.
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
	// serverCa pins the server's CA (sslmode=verify-full). Required for TLS/mTLS; omit for plaintext.
	// +optional
	serverCa *dagger.File,
	// clientCert is the PEM client leaf for mTLS; its CN must equal `user`. Provide with clientKey.
	// +optional
	clientCert *dagger.File,
	// clientKey is the PEM PKCS#8 client private key for mTLS. Provide with clientCert.
	// +optional
	clientKey *dagger.Secret,
) (*dagger.Directory, error) {
	if err := skill.ValidateDBName(db); err != nil {
		return nil, err
	}

	security, err := postgresClientSecurity(serverCa, clientCert, clientKey)
	if err != nil {
		return nil, err
	}
	client := dag.Postgres().Client(
		host, user, db, password,
		security,
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

// postgresClientSecurity selects the postgres ClientSecurity profile from the
// supplied cert material. The mode is inferred rather than passed explicitly so
// it can never disagree with the certs actually provided: no serverCa is
// plaintext, serverCa alone is one-way TLS, and serverCa plus a client
// cert/key pair is mTLS. A client cert without its key (or vice versa) and an
// mTLS attempt without a serverCa are rejected up front with a clear error.
func postgresClientSecurity(serverCa *dagger.File, clientCert *dagger.File, clientKey *dagger.Secret) (*dagger.PostgresClientSecurity, error) {
	hasCert, hasKey := clientCert != nil, clientKey != nil
	switch {
	case serverCa == nil && !hasCert && !hasKey:
		return dag.Postgres().PlaintextClientSecurity(), nil
	case hasCert != hasKey:
		return nil, fmt.Errorf("mTLS requires both clientCert and clientKey (clientCert=%t, clientKey=%t)", hasCert, hasKey)
	case hasCert && hasKey:
		if serverCa == nil {
			return nil, fmt.Errorf("mTLS requires serverCa to verify the server")
		}
		return dag.Postgres().MtlsClientSecurity(serverCa, clientCert, clientKey), nil
	default: // serverCa != nil, no client cert/key
		return dag.Postgres().TLSClientSecurity(serverCa), nil
	}
}
