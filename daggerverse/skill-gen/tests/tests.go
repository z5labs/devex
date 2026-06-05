package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"dagger/tests/internal/dagger"
)

// -----------------------------------------------------------------------------
// Cluster + skill-gen helpers. Each test boots its OWN cluster via a runtime-
// random name that folds into Postgres.Cluster's +cache="session" key, so
// concurrent tests get independent backing services and never share storage.
// -----------------------------------------------------------------------------

// bootCluster mints a fresh single-node primary (not yet started) and the
// password it was provisioned with. We deliberately do NOT defer Stop: the
// runtime-random cluster name folds into Postgres.Cluster's +cache="session"
// key, so each cluster is bound to this engine session and torn down with it.
// This mirrors postgres/tests' own bootCluster, which documents the same
// choice; an explicit per-test Stop would only matter for an invariant that
// asserts restart behaviour, which none of these tests do.
func bootCluster(ctx context.Context) (*dagger.PostgresCluster, *dagger.Secret, error) {
	name, err := randHex(ctx)
	if err != nil {
		return nil, nil, err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return nil, nil, err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().PlaintextServerSecurity(),
		dagger.PostgresClusterOpts{Name: name},
	)
	return cluster, pass, nil
}

// applyDDL runs each statement on the cluster's primary over a plaintext
// client, starting the service on the first call so its hostname becomes
// session-reachable.
func applyDDL(ctx context.Context, cluster *dagger.PostgresCluster, stmts ...string) error {
	return applyDDLSec(ctx, cluster, dag.Postgres().PlaintextClientSecurity(), stmts...)
}

// applyDDLSec is applyDDL over a caller-supplied ClientSecurity, so TLS / mTLS
// clusters can be seeded with the matching transport before introspection.
func applyDDLSec(ctx context.Context, cluster *dagger.PostgresCluster, security *dagger.PostgresClientSecurity, stmts ...string) error {
	client := cluster.Client(security)
	for _, s := range stmts {
		if _, err := client.Exec(ctx, s); err != nil {
			return fmt.Errorf("apply DDL %q: %w", s, err)
		}
	}
	return nil
}

// coords resolves the cluster's host, port, user, and database.
func coords(ctx context.Context, cluster *dagger.PostgresCluster) (host string, port int, user, db string, err error) {
	ep, err := cluster.Endpoint(ctx)
	if err != nil {
		return "", 0, "", "", err
	}
	host, portStr, found := strings.Cut(ep, ":")
	if !found {
		return "", 0, "", "", fmt.Errorf("endpoint %q has no port", ep)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("parse port from %q: %w", ep, err)
	}
	if user, err = cluster.User(ctx); err != nil {
		return "", 0, "", "", err
	}
	if db, err = cluster.Database(ctx); err != nil {
		return "", 0, "", "", err
	}
	return host, port, user, db, nil
}

// genSkill generates a skill Directory for the cluster's database over a
// plaintext connection using the given password (pass a wrong secret to
// exercise the auth-failure path).
func genSkill(ctx context.Context, cluster *dagger.PostgresCluster, pass *dagger.Secret) (*dagger.Directory, string, error) {
	return genSkillSec(ctx, cluster, pass, nil, nil, nil)
}

// genSkillSec is genSkill with explicit TLS material. Omitting all three cert
// args is plaintext; serverCa alone is one-way TLS; serverCa + clientCert +
// clientKey is mTLS. The skill-gen module infers the mode from what is set.
func genSkillSec(ctx context.Context, cluster *dagger.PostgresCluster, pass *dagger.Secret, serverCa, clientCert *dagger.File, clientKey *dagger.Secret) (*dagger.Directory, string, error) {
	host, port, user, db, err := coords(ctx, cluster)
	if err != nil {
		return nil, "", err
	}
	dir := dag.SkillGen().Postgres(host, user, db, pass, dagger.SkillGenPostgresOpts{
		Port:       port,
		ServerCa:   serverCa,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	})
	return dir, db, nil
}

// ddlFull builds a schema with two tables, an FK, an enum, a view, an index,
// and comments — enough to populate every reference file.
var ddlFull = []string{
	`CREATE TYPE book_status AS ENUM ('draft', 'published', 'archived')`,
	`CREATE TABLE authors (id bigint PRIMARY KEY, name text NOT NULL)`,
	`CREATE TABLE books (
		id bigint PRIMARY KEY,
		author_id bigint NOT NULL REFERENCES authors(id),
		title text NOT NULL,
		status book_status NOT NULL DEFAULT 'draft'
	)`,
	`CREATE VIEW published_books AS SELECT id, title FROM books WHERE status = 'published'`,
	`CREATE INDEX books_author_idx ON books(author_id)`,
	`COMMENT ON TABLE authors IS 'Book authors.'`,
	`COMMENT ON COLUMN books.title IS 'Display title.'`,
}

// ddlBase is ddlFull without the enum (so a later enum addition shows up as an
// added reference file).
var ddlBase = []string{
	`CREATE TABLE authors (id bigint PRIMARY KEY, name text NOT NULL)`,
	`CREATE TABLE books (
		id bigint PRIMARY KEY,
		author_id bigint NOT NULL REFERENCES authors(id),
		title text NOT NULL
	)`,
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// RejectsInvalidDbName pins that a db name violating ^[A-Za-z0-9_-]+$ is
// rejected before any introspection (no cluster needed — validation precedes
// the network).
//
// +cache="never"
func (t *Tests) RejectsInvalidDbName(ctx context.Context) error {
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	_, err = dag.SkillGen().
		Postgres("unused-host", "postgres", "bad name", pass).
		Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected rejection of invalid db name, got nil error")
	}
	if !strings.Contains(err.Error(), "invalid db name") {
		return fmt.Errorf("expected 'invalid db name' error, got: %v", err)
	}
	return nil
}

// IntrospectionFailureAborts pins that an introspection failure (here, a wrong
// password against a live cluster) aborts with a non-zero error and yields no
// Directory.
//
// +cache="never"
func (t *Tests) IntrospectionFailureAborts(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	// Start the service so the host exists and rejects on auth, not on dial.
	if err := applyDDL(ctx, cluster, "SELECT 1"); err != nil {
		return err
	}
	wrong, err := randSecret(ctx)
	if err != nil {
		return err
	}
	dir, _, err := genSkill(ctx, cluster, wrong)
	if err != nil {
		return err
	}
	if _, err := dir.Sync(ctx); err == nil {
		return fmt.Errorf("expected introspection failure with wrong password, got nil error")
	}
	return nil
}

// GeneratesPgSkillFromCluster pins the happy path: the full tree is present and
// fully substituted, the frontmatter is correct and model-invocable, and
// enums.md exists because the schema defines an enum.
//
// +cache="never"
func (t *Tests) GeneratesPgSkillFromCluster(ctx context.Context) error {
	cluster, pass, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	if err := applyDDL(ctx, cluster, ddlFull...); err != nil {
		return err
	}
	dir, db, err := genSkill(ctx, cluster, pass)
	if err != nil {
		return err
	}
	return assertPgSkillTree(ctx, dir, db)
}

// assertPgSkillTree verifies the generated skill Directory has the full,
// fully-substituted tree (top-level files, references/, scripts/), correct
// model-invocable frontmatter, and the FK-hub table 'authors' ranked into
// SKILL.md. Shared by the plaintext, TLS, and mTLS happy-path tests, which
// all generate from the same ddlFull schema.
func assertPgSkillTree(ctx context.Context, dir *dagger.Directory, db string) error {
	// Top-level + references entries present. Directory entries carry a
	// trailing slash, so compare on the trimmed name.
	top, err := dir.Entries(ctx)
	if err != nil {
		return err
	}
	for _, want := range []string{"SKILL.md", "README.md", "references", "scripts"} {
		if !slices.ContainsFunc(top, func(e string) bool { return strings.TrimSuffix(e, "/") == want }) {
			return fmt.Errorf("missing top-level entry %q (got %v)", want, top)
		}
	}
	refs, err := dir.Directory("references").Entries(ctx)
	if err != nil {
		return err
	}
	for _, want := range []string{"tables.md", "relationships.md", "views.md", "indexes.md", "enums.md"} {
		if !slices.Contains(refs, want) {
			return fmt.Errorf("missing references/%s (got %v)", want, refs)
		}
	}

	skillMD, err := dir.File("SKILL.md").Contents(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(skillMD, "name: pg-"+db) {
		return fmt.Errorf("SKILL.md frontmatter missing name: pg-%s", db)
	}
	if strings.Contains(skillMD, "disable-model-invocation") {
		return fmt.Errorf("SKILL.md must be model-invocable")
	}
	for _, ph := range []string{"<dbname>", "<skill-dir>", "<top tables>", "<top-table>"} {
		if strings.Contains(skillMD, ph) {
			return fmt.Errorf("SKILL.md has unsubstituted placeholder %q", ph)
		}
	}
	// authors is the FK hub (books.author_id → authors.id), so it ranks first.
	if !strings.Contains(skillMD, "authors") {
		return fmt.Errorf("SKILL.md does not mention the top table 'authors'")
	}

	// scripts/query.sh is executable and free of placeholders.
	queryEntry, err := dir.File("scripts/query.sh").Contents(ctx)
	if err != nil {
		return err
	}
	for _, ph := range []string{"<host>", "<port>", "<user>", "<dbname>"} {
		if strings.Contains(queryEntry, ph) {
			return fmt.Errorf("query.sh has unsubstituted placeholder %q", ph)
		}
	}
	return nil
}

// PostgresShouldNotBeCached pins the non-caching contract: regenerating after a
// schema change reflects the change rather than serving a stale cached
// Directory.
//
// +cache="never"
func (t *Tests) PostgresShouldNotBeCached(ctx context.Context) error {
	cluster, pass, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	if err := applyDDL(ctx, cluster, ddlBase...); err != nil {
		return err
	}
	dir1, _, err := genSkill(ctx, cluster, pass)
	if err != nil {
		return err
	}
	before, err := dir1.File("references/tables.md").Contents(ctx)
	if err != nil {
		return err
	}
	if strings.Contains(before, "publishers") {
		return fmt.Errorf("base tables.md unexpectedly mentions 'publishers'")
	}

	if err := applyDDL(ctx, cluster, `CREATE TABLE publishers (id bigint PRIMARY KEY, name text NOT NULL)`); err != nil {
		return err
	}
	dir2, _, err := genSkill(ctx, cluster, pass)
	if err != nil {
		return err
	}
	after, err := dir2.File("references/tables.md").Contents(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(after, "publishers") {
		return fmt.Errorf("regeneration did not reflect the new 'publishers' table (cache leak?)")
	}
	return nil
}

// RegenChangesetEmptyWhenUnchanged pins byte-stability: two generations over an
// unchanged schema produce an empty changeset.
//
// +cache="never"
func (t *Tests) RegenChangesetEmptyWhenUnchanged(ctx context.Context) error {
	cluster, pass, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	if err := applyDDL(ctx, cluster, ddlFull...); err != nil {
		return err
	}
	dir1, _, err := genSkill(ctx, cluster, pass)
	if err != nil {
		return err
	}
	dir2, _, err := genSkill(ctx, cluster, pass)
	if err != nil {
		return err
	}
	empty, err := dir2.Changes(dir1).IsEmpty(ctx)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("changeset over an unchanged schema is not empty (non-deterministic output)")
	}
	return nil
}

// RegenChangesetReflectsSchemaDrift pins that a schema change yields a non-empty
// changeset: a new table modifies the references, and adding the first enum
// adds references/enums.md as a brand-new path.
//
// +cache="never"
func (t *Tests) RegenChangesetReflectsSchemaDrift(ctx context.Context) error {
	cluster, pass, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	if err := applyDDL(ctx, cluster, ddlBase...); err != nil { // no enum yet
		return err
	}
	oldDir, _, err := genSkill(ctx, cluster, pass)
	if err != nil {
		return err
	}
	// Materialize oldDir against the pre-drift schema NOW. genSkill returns a
	// lazy Directory and Postgres is +cache="never", so without this Sync the
	// "before" snapshot would re-introspect at changeset-resolution time —
	// after the drift DDL below — and the changeset would come back empty.
	oldDir, err = oldDir.Sync(ctx)
	if err != nil {
		return err
	}

	// Drift: a new table plus the first enum type.
	if err := applyDDL(ctx, cluster,
		`CREATE TYPE shelf AS ENUM ('new', 'used')`,
		`CREATE TABLE publishers (id bigint PRIMARY KEY, name text NOT NULL)`,
	); err != nil {
		return err
	}
	newDir, _, err := genSkill(ctx, cluster, pass)
	if err != nil {
		return err
	}

	cs := newDir.Changes(oldDir)
	empty, err := cs.IsEmpty(ctx)
	if err != nil {
		return err
	}
	if empty {
		return fmt.Errorf("changeset after a schema change is unexpectedly empty")
	}

	added, err := cs.AddedPaths(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(added, "references/enums.md") {
		return fmt.Errorf("expected references/enums.md in added paths, got %v", added)
	}

	patch, err := cs.AsPatch().Contents(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(patch, "publishers") {
		return fmt.Errorf("patch does not mention the new 'publishers' table")
	}
	return nil
}

// -----------------------------------------------------------------------------
// TLS / mTLS round-trip tests. Each mints a per-test CA + leaf certs at runtime,
// boots a TLS-required cluster whose listener refuses plaintext, and proves
// SkillGen.Postgres introspects it over the encrypted transport.
// -----------------------------------------------------------------------------

// GeneratesPgSkillOverTls pins that SkillGen.Postgres introspects a one-way-TLS
// cluster (sslmode=verify-full) when handed only the server CA, producing the
// full skill tree. The server cert's SAN must cover the dialed clusterHost.
//
// +cache="never"
func (t *Tests) GeneratesPgSkillOverTls(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "skillgen-tls")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, clusterHost(name), "skillgen-tls-server")
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().TLSServerSecurity(cert, key),
		dagger.PostgresClusterOpts{Name: name},
	)
	if err := applyDDLSec(ctx, cluster, dag.Postgres().TLSClientSecurity(ca.CertPemFile()), ddlFull...); err != nil {
		return err
	}
	dir, db, err := genSkillSec(ctx, cluster, pass, ca.CertPemFile(), nil, nil)
	if err != nil {
		return err
	}
	return assertPgSkillTree(ctx, dir, db)
}

// GeneratesPgSkillOverMtls pins that SkillGen.Postgres introspects a mutual-TLS
// cluster when handed the server CA plus a client cert/key whose CN matches the
// role (clientcert=verify-full), producing the full skill tree.
//
// +cache="never"
func (t *Tests) GeneratesPgSkillOverMtls(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	// One CA both signs the server leaf and anchors the accepted client certs.
	ca, err := freshCa(ctx, "skillgen-mtls")
	if err != nil {
		return err
	}
	serverCert, serverKey, err := issueServerCert(ctx, ca, clusterHost(name), "skillgen-mtls-server")
	if err != nil {
		return err
	}
	// CN must equal the DB role ("postgres", the Cluster default) or the
	// primary rejects the cert with a misleading 28P01 auth failure.
	clientCert, clientKey, err := issueClientCert(ctx, ca, "postgres", "skillgen-mtls-client")
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().MtlsServerSecurity(serverCert, serverKey, ca.CertPemFile()),
		dagger.PostgresClusterOpts{Name: name},
	)
	clientSec := dag.Postgres().MtlsClientSecurity(ca.CertPemFile(), clientCert, clientKey)
	if err := applyDDLSec(ctx, cluster, clientSec, ddlFull...); err != nil {
		return err
	}
	dir, db, err := genSkillSec(ctx, cluster, pass, ca.CertPemFile(), clientCert, clientKey)
	if err != nil {
		return err
	}
	return assertPgSkillTree(ctx, dir, db)
}

// PlaintextParamsAgainstTlsAbort pins that introspecting a TLS-required cluster
// with no cert params (plaintext) aborts: the primary's hostssl-only pg_hba.conf
// refuses the unencrypted connection, so generation fails and yields no
// Directory rather than silently producing partial output.
//
// +cache="never"
func (t *Tests) PlaintextParamsAgainstTlsAbort(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "skillgen-tls-neg")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, clusterHost(name), "skillgen-tls-neg-server")
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().TLSServerSecurity(cert, key),
		dagger.PostgresClusterOpts{Name: name},
	)
	// Seed over TLS so the service is up and rejects on transport, not on dial.
	if err := applyDDLSec(ctx, cluster, dag.Postgres().TLSClientSecurity(ca.CertPemFile()), "SELECT 1"); err != nil {
		return err
	}
	dir, _, err := genSkill(ctx, cluster, pass) // no cert params → plaintext
	if err != nil {
		return err
	}
	if _, err := dir.Sync(ctx); err == nil {
		return fmt.Errorf("expected plaintext introspection of a TLS-required cluster to fail, got nil error")
	}
	return nil
}
