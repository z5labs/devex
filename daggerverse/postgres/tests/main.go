// Tests for the postgres daggerverse module. Each test is exposed as a
// standalone dagger function so it can be invoked individually during
// TDD; All wires them up for parallel execution under
// `dagger call all`.
//
// Every password, cluster name, and table name is minted at runtime via
// dag.Random().Sha256. Role and database deliberately use the postgres
// module's defaults ("postgres"), which a few tests assert against.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

type Tests struct{}

// All runs every postgres test as a convenience for local `dagger call
// all` invocations. CI does NOT call All: each of the two
// sub-aggregators below (Validation, Cluster) is registered as its own
// check, so GH Actions schedules each onto its own runner in parallel —
// running All on top would double-bill the same work.
//
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("Validation", func(ctx context.Context) error {
		return t.Validation(ctx, parallel)
	})
	jobs = jobs.WithJob("Cluster", func(ctx context.Context) error {
		return t.Cluster(ctx, parallel)
	})
	jobs = jobs.WithJob("Security", func(ctx context.Context) error {
		return t.Security(ctx, parallel)
	})
	return jobs.Run(ctx)
}

// Validation runs the input-rejection tests plus the cache-directive
// tests (*ShouldNotBeCached). These don't share session-cached cluster
// state with one another, so they're safe to fan out unbounded.
//
// +check
// +cache="session"
func (t *Tests) Validation(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("cluster-rejects-nil-password", t.ClusterRejectsNilPassword)
	jobs = jobs.WithJob("cluster-rejects-nil-security", t.ClusterRejectsNilSecurity)
	jobs = jobs.WithJob("endpoint-should-not-be-cached", t.EndpointShouldNotBeCached)
	jobs = jobs.WithJob("scalar-should-not-be-cached", t.ScalarShouldNotBeCached)
	return jobs.Run(ctx)
}

// Cluster runs the topology and client round-trip tests. Each test
// boots its own cluster via bootCluster, whose runtime-random name folds
// into Postgres.Cluster's session-cache key so concurrent tests boot
// independent backing services and never share storage.
//
// +check
// +cache="session"
func (t *Tests) Cluster(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("defaults-produce-healthy-primary", t.DefaultsProduceHealthyPrimary)
	jobs = jobs.WithJob("user-database-round-trip", t.UserDatabaseRoundTrip)
	jobs = jobs.WithJob("password-reusable-via-client", t.PasswordReusableViaClient)
	jobs = jobs.WithJob("bind-primary-reachable-from-alpine", t.BindPrimaryReachableFromAlpine)
	jobs = jobs.WithJob("client-ping-wrong-password-fails", t.ClientPingWrongPasswordFails)
	jobs = jobs.WithJob("exec-scalar-round-trip", t.ExecScalarRoundTrip)
	jobs = jobs.WithJob("apply-file-round-trip", t.ApplyFileRoundTrip)
	jobs = jobs.WithJob("query-json-returns-row-objects", t.QueryJSONReturnsRowObjects)
	return jobs.Run(ctx)
}

// Security runs the TLS / mTLS listener + client tests. Each test mints
// its own CA, leaf certs, password, and cluster name at runtime (no
// literal credentials or PEM blobs), and folds a unique name into the
// cluster's session-cache key, so the tests fan out without sharing
// state.
//
// +check
// +cache="session"
func (t *Tests) Security(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("cluster-tls-round-trip-from-client", t.ClusterTlsRoundTripFromClient)
	jobs = jobs.WithJob("cluster-mtls-round-trip-from-client", t.ClusterMtlsRoundTripFromClient)
	jobs = jobs.WithJob("tls-cluster-rejects-plaintext-client", t.TlsClusterRejectsPlaintextClient)
	jobs = jobs.WithJob("mtls-cluster-rejects-tls-only-client", t.MtlsClusterRejectsTlsOnlyClient)
	jobs = jobs.WithJob("tls-cluster-rejects-empty-name", t.TlsClusterRejectsEmptyName)
	jobs = jobs.WithJob("bind-primary-resolves-from-user-container-tls", t.BindPrimaryResolvesFromUserContainerTls)
	return jobs.Run(ctx)
}

// -----------------------------------------------------------------------------
// Helpers — all identifiers minted at runtime, no literals.
// -----------------------------------------------------------------------------

// randHex returns a fresh 12-hex-char value via the random module.
func randHex(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return h[:12], nil
}

// randIdent returns a SQL-safe identifier (letter-led) with a random
// suffix, e.g. a table or column name.
func randIdent(ctx context.Context, prefix string) (string, error) {
	h, err := randHex(ctx)
	if err != nil {
		return "", err
	}
	return prefix + h, nil
}

// randSecret mints a random password and wraps it in a uniquely-named
// *dagger.Secret. The plaintext is a full SHA-256 hash; the secret name
// carries a random suffix so concurrent SetSecret calls don't collide.
func randSecret(ctx context.Context) (*dagger.Secret, error) {
	full, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 32})
	if err != nil {
		return nil, err
	}
	return dag.SetSecret("pg-pw-"+full[:12], full), nil
}

// bootCluster mints a fresh single-node primary and returns it together
// with the password secret it was provisioned with. The cluster name is
// a runtime-random value (no literals) that folds into
// Postgres.Cluster's +cache="session" key, so concurrent tests boot
// independent backing services and never share storage; a single test
// mints one name and reuses the returned handle, so its chained
// Client.Exec → Client.Scalar calls stay cache-coherent. We deliberately
// do NOT defer Stop: the *ShouldNotBeCached tests Stop their own cluster
// as part of the invariant; everyone else lets the session teardown
// handle it.
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

// hostOf splits a `host:port` endpoint into its host component.
func hostOf(endpoint string) string {
	host, _, _ := strings.Cut(endpoint, ":")
	return host
}

// clusterHost reproduces Postgres.Cluster's hostname derivation
// (`postgres-` + the first 12 hex chars of sha256(name)). Tests need it
// to mint a server certificate whose SAN matches the hostname clients
// dial — sslmode=verify-full checks the SAN against the dialed host, and
// the psql test cannot override ServerName.
func clusterHost(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "postgres-" + hex.EncodeToString(sum[:6])
}

// randNamedSecret mints a uniquely-named *dagger.Secret holding fresh
// random bytes. Used for the throwaway PKCS#12 passwords the
// certificate-management leaf issuers require (we consume the PEM cert /
// key directly, never the PKCS#12 archive, so the value is irrelevant).
func randNamedSecret(ctx context.Context, label string) (*dagger.Secret, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 32})
	if err != nil {
		return nil, err
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(label+"-"+suffix, h), nil
}

// freshCa mints a fresh per-test root CA via the certificate-management
// module from a runtime-random RSA key, password, and serial.
func freshCa(ctx context.Context, label string) (*dagger.CertificateManagementCertificateAuthority, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	key := dag.SetSecret(label+"-ca-key-"+suffix, keyPem)
	pwd, err := randNamedSecret(ctx, label+"-ca-pwd")
	if err != nil {
		return nil, fmt.Errorf("generate %s ca password: %w", label, err)
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	return dag.CertificateManagement().CreateCertificateAuthority(nb, serial, pwd, key,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   "pg test ca " + label,
			ValidityDays: 30,
		}), nil
}

// leafKey mints a fresh RSA private key for a leaf certificate, wrapped
// in a uniquely-named *dagger.Secret (PEM PKCS#8, as the issuer expects).
func leafKey(ctx context.Context, label string) (*dagger.Secret, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s leaf key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(label+"-leaf-key-"+suffix, keyPem), nil
}

// issueServerCert signs a server leaf certificate carrying host (and
// localhost / 127.0.0.1) as SANs, returning the PEM cert file and PEM
// key secret to hand to TlsServerSecurity / MtlsServerSecurity.
func issueServerCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, host, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label)
	if err != nil {
		return nil, nil, err
	}
	pwd, err := randNamedSecret(ctx, label+"-leaf-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	issued := ca.IssueServerCertificate(host, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans:      []string{host, "localhost"},
			IPSans:       []string{"127.0.0.1"},
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// issueClientCert signs a client leaf certificate, returning the PEM
// cert file and PEM key secret to hand to MtlsClientSecurity. The
// certificate's Common Name is set to user because the primary's
// pg_hba.conf uses clientcert=verify-full, which additionally requires
// the client cert CN to match the PostgreSQL role being authenticated.
func issueClientCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, user, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label)
	if err != nil {
		return nil, nil, err
	}
	pwd, err := randNamedSecret(ctx, label+"-leaf-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	issued := ca.IssueClientCertificate(user, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueClientCertificateOpts{
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// -----------------------------------------------------------------------------
// Validation tests — exercise the input rejections reachable through the
// generated SDK binding. nil required args are rejected by the binding's
// assertNotNil (it panics before the call leaves the test module), so we
// recover and assert the panic names the offending argument.
// -----------------------------------------------------------------------------

// ClusterRejectsNilPassword verifies a nil password is rejected.
//
// +cache="never"
func (t *Tests) ClusterRejectsNilPassword(ctx context.Context) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			returnErr = fmt.Errorf("expected Cluster(nil password) to panic via assertNotNil, but it did not")
			return
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "password") {
			returnErr = fmt.Errorf("expected panic to mention password, got: %v", r)
		}
	}()
	cluster := dag.Postgres().Cluster(nil, dag.Postgres().PlaintextServerSecurity())
	_, _ = cluster.Endpoint(ctx)
	return nil
}

// ClusterRejectsNilSecurity verifies a nil clientListenerSecurity is rejected.
//
// +cache="never"
func (t *Tests) ClusterRejectsNilSecurity(ctx context.Context) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			returnErr = fmt.Errorf("expected Cluster(nil security) to panic via assertNotNil, but it did not")
			return
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "clientListenerSecurity") && !strings.Contains(msg, "security") {
			returnErr = fmt.Errorf("expected panic to mention clientListenerSecurity, got: %v", r)
		}
	}()
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(pass, nil)
	_, _ = cluster.Endpoint(ctx)
	return nil
}

// -----------------------------------------------------------------------------
// Cache-directive tests — verify +cache="never" propagation off chained
// methods.
// -----------------------------------------------------------------------------

// EndpointShouldNotBeCached verifies the chained cluster methods
// re-execute on every call rather than freezing on a cached result.
// Endpoint is a pure address getter, so we exercise the
// re-execution that matters: Ping (which starts the service), Stop
// (which kills it), then Ping again — the second Ping must re-start the
// killed service. If Client/start were cached, the service would stay
// dead and the second Ping would dial a hung port. We also assert the
// Endpoint address is stable across the cycle.
//
// +cache="never"
func (t *Tests) EndpointShouldNotBeCached(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	ep1, err := cluster.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint 1: %w", err)
	}
	if err := cluster.Client(dag.Postgres().PlaintextClientSecurity()).Ping(ctx); err != nil {
		return fmt.Errorf("initial ping: %w", err)
	}
	if err := cluster.Stop(ctx); err != nil {
		return fmt.Errorf("stop cluster: %w", err)
	}
	ep2, err := cluster.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint 2: %w", err)
	}
	if ep1 != ep2 {
		return fmt.Errorf("expected stable endpoint across restart (1=%q, 2=%q)", ep1, ep2)
	}
	if err := cluster.Client(dag.Postgres().PlaintextClientSecurity()).Ping(ctx); err != nil {
		return fmt.Errorf("ping after restart (Client/start likely cached, service never re-started): %w", err)
	}
	return nil
}

// ScalarShouldNotBeCached verifies Scalar re-executes on every call. We
// insert one row, read count(*) == "1", insert a second row, then read
// count(*) again: a cached Scalar would still report "1" instead of "2".
//
// +cache="never"
func (t *Tests) ScalarShouldNotBeCached(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	table, err := randIdent(ctx, "t_")
	if err != nil {
		return err
	}
	client := cluster.Client(dag.Postgres().PlaintextClientSecurity())
	if _, err := client.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id int)", table)); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := client.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES (1)", table)); err != nil {
		return fmt.Errorf("insert 1: %w", err)
	}
	got1, err := client.Scalar(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table))
	if err != nil {
		return fmt.Errorf("scalar 1: %w", err)
	}
	if got1 != "1" {
		return fmt.Errorf("expected count 1 after first insert, got %q", got1)
	}
	if _, err := client.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES (2)", table)); err != nil {
		return fmt.Errorf("insert 2: %w", err)
	}
	got2, err := client.Scalar(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table))
	if err != nil {
		return fmt.Errorf("scalar 2: %w", err)
	}
	if got2 != "2" {
		return fmt.Errorf("expected count 2 after second insert (Scalar likely cached), got %q", got2)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Cluster + Client round-trip tests.
// -----------------------------------------------------------------------------

// DefaultsProduceHealthyPrimary boots a default cluster and proves it is
// a healthy primary by running `pg_isready` against it from a container
// running the postgres image (which ships pg_isready), bound via
// BindPrimary.
//
// +cache="never"
func (t *Tests) DefaultsProduceHealthyPrimary(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	ep, err := cluster.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	host := hostOf(ep)
	// BindPrimary starts the service as this container's dependency and
	// wires its IP into /etc/hosts. pg_isready is retried briefly: the
	// postgres entrypoint flaps the listener once during initdb before
	// settling on the external port.
	probe := fmt.Sprintf(
		"for i in $(seq 1 30); do pg_isready -h %s -p 5432 && exit 0; sleep 1; done; pg_isready -h %s -p 5432",
		host, host,
	)
	out, err := cluster.BindPrimary(dag.Container().From("postgres:17")).
		WithExec([]string{"sh", "-c", probe}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("pg_isready: %w", err)
	}
	if !strings.Contains(out, "accepting connections") {
		return fmt.Errorf("expected pg_isready to report accepting connections, got: %s", out)
	}
	return nil
}

// UserDatabaseRoundTrip verifies Cluster.User()/Database() echo the
// inputs and a pgx round-trip confirms current_user / current_database
// match.
//
// +cache="never"
func (t *Tests) UserDatabaseRoundTrip(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	user, err := cluster.User(ctx)
	if err != nil {
		return fmt.Errorf("user: %w", err)
	}
	db, err := cluster.Database(ctx)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if user != "postgres" || db != "postgres" {
		return fmt.Errorf("expected default user/db postgres, got user=%q db=%q", user, db)
	}

	client := cluster.Client(dag.Postgres().PlaintextClientSecurity())
	gotUser, err := client.Scalar(ctx, "SELECT current_user")
	if err != nil {
		return fmt.Errorf("select current_user: %w", err)
	}
	if gotUser != user {
		return fmt.Errorf("current_user %q != Cluster.User() %q", gotUser, user)
	}
	gotDB, err := client.Scalar(ctx, "SELECT current_database()")
	if err != nil {
		return fmt.Errorf("select current_database: %w", err)
	}
	if gotDB != db {
		return fmt.Errorf("current_database %q != Cluster.Database() %q", gotDB, db)
	}
	return nil
}

// PasswordReusableViaClient verifies Cluster.Password() returns a secret
// whose plaintext equals the provisioning password: re-using it via
// Postgres.Client against the same endpoint authenticates successfully.
//
// +cache="never"
func (t *Tests) PasswordReusableViaClient(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	// Start the primary in this module's DNS domain so the module-runtime
	// standalone client below can resolve its hostname.
	if err := cluster.Client(dag.Postgres().PlaintextClientSecurity()).Ping(ctx); err != nil {
		return fmt.Errorf("start cluster via client ping: %w", err)
	}
	ep, err := cluster.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	user, err := cluster.User(ctx)
	if err != nil {
		return err
	}
	db, err := cluster.Database(ctx)
	if err != nil {
		return err
	}
	// Re-use the password the cluster reports, via the standalone client
	// factory, against the same endpoint.
	client := dag.Postgres().Client(
		hostOf(ep),
		user,
		db,
		cluster.Password(),
		dag.Postgres().PlaintextClientSecurity(),
	)
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("ping with Cluster.Password(): %w", err)
	}
	return nil
}

// BindPrimaryReachableFromAlpine verifies BindPrimary makes the primary
// reachable at Cluster.Endpoint() from a fresh alpine container. Alpine
// lacks pg_isready, so we prove TCP reachability with busybox nc.
//
// +cache="never"
func (t *Tests) BindPrimaryReachableFromAlpine(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	ep, err := cluster.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	host := hostOf(ep)
	_, err = cluster.BindPrimary(dag.Container().From("alpine:3")).
		WithExec([]string{"nc", "-z", "-w", "10", host, "5432"}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("nc reachability probe to %s:5432 failed: %w", host, err)
	}
	return nil
}

// ClientPingWrongPasswordFails verifies a correct-password Ping succeeds
// and a wrong-password Ping fails with an auth error.
//
// +cache="never"
func (t *Tests) ClientPingWrongPasswordFails(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	if err := cluster.Client(dag.Postgres().PlaintextClientSecurity()).Ping(ctx); err != nil {
		return fmt.Errorf("expected correct-password ping to succeed: %w", err)
	}

	ep, err := cluster.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	user, err := cluster.User(ctx)
	if err != nil {
		return err
	}
	db, err := cluster.Database(ctx)
	if err != nil {
		return err
	}
	wrong, err := randSecret(ctx)
	if err != nil {
		return err
	}
	err = dag.Postgres().Client(
		hostOf(ep),
		user,
		db,
		wrong,
		dag.Postgres().PlaintextClientSecurity(),
	).Ping(ctx)
	if err == nil {
		return fmt.Errorf("expected wrong-password ping to fail, got nil error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "password") &&
		!strings.Contains(strings.ToLower(err.Error()), "authentication") {
		return fmt.Errorf("expected an auth error, got: %v", err)
	}
	return nil
}

// ExecScalarRoundTrip runs a CREATE TABLE + INSERT + SELECT count(*)
// sequence across chained Cluster.Client() calls, proving the
// session-cached cluster preserves on-disk state between separate
// Client handles.
//
// +cache="never"
func (t *Tests) ExecScalarRoundTrip(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	table, err := randIdent(ctx, "t_")
	if err != nil {
		return err
	}

	writer := cluster.Client(dag.Postgres().PlaintextClientSecurity())
	if _, err := writer.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id int, label text)", table)); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	affected, err := writer.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, label) VALUES (1, 'a'), (2, 'b')", table))
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	if affected != 2 {
		return fmt.Errorf("expected 2 rows affected by insert, got %d", affected)
	}

	// Fresh Client handle off the same session-cached cluster — must see
	// the rows the prior handle wrote.
	reader := cluster.Client(dag.Postgres().PlaintextClientSecurity())
	count, err := reader.Scalar(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table))
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}
	if count != "2" {
		return fmt.Errorf("expected count 2 across chained Client() calls, got %q", count)
	}
	return nil
}

// ApplyFileRoundTrip runs a multi-statement *dagger.File and confirms
// the resulting rows are readable via Scalar.
//
// +cache="never"
func (t *Tests) ApplyFileRoundTrip(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	table, err := randIdent(ctx, "t_")
	if err != nil {
		return err
	}

	script := fmt.Sprintf(`-- seed script for %[1]s
CREATE TABLE %[1]s (id int, note text);
INSERT INTO %[1]s (id, note) VALUES (1, 'one'); /* first */
INSERT INTO %[1]s (id, note) VALUES (2, 'two; not a delimiter');
`, table)
	file := dag.Directory().WithNewFile("seed.sql", script).File("seed.sql")

	client := cluster.Client(dag.Postgres().PlaintextClientSecurity())
	if err := client.ApplyFile(ctx, file); err != nil {
		return fmt.Errorf("apply file: %w", err)
	}
	count, err := client.Scalar(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table))
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}
	if count != "2" {
		return fmt.Errorf("expected 2 rows after ApplyFile, got %q", count)
	}
	return nil
}

// QueryJSONReturnsRowObjects verifies QueryJSON returns a *dagger.File
// whose contents parse as a JSON array of row objects keyed by column
// name.
//
// +cache="never"
func (t *Tests) QueryJSONReturnsRowObjects(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx)
	if err != nil {
		return err
	}
	table, err := randIdent(ctx, "t_")
	if err != nil {
		return err
	}
	client := cluster.Client(dag.Postgres().PlaintextClientSecurity())
	if _, err := client.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id int, label text)", table)); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := client.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, label) VALUES (7, 'lucky')", table)); err != nil {
		return fmt.Errorf("insert: %w", err)
	}

	contents, err := client.
		QueryJSON(fmt.Sprintf("SELECT id, label FROM %s ORDER BY id", table)).
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("query json contents: %w", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(contents), &rows); err != nil {
		return fmt.Errorf("parse json %q: %w", contents, err)
	}
	if len(rows) != 1 {
		return fmt.Errorf("expected 1 row object, got %d (%q)", len(rows), contents)
	}
	if _, ok := rows[0]["id"]; !ok {
		return fmt.Errorf("expected row object keyed by column name 'id', got %q", contents)
	}
	if label, _ := rows[0]["label"].(string); label != "lucky" {
		return fmt.Errorf("expected label 'lucky', got %q (%q)", label, contents)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Security tests — TLS / mTLS listeners and clients. CA + leaf certs,
// passwords, and cluster names are all minted at runtime.
// -----------------------------------------------------------------------------

// ClusterTlsRoundTripFromClient boots a one-way-TLS primary and proves a
// matching TLS client can Exec + Scalar against it over the encrypted
// listener.
//
// +cache="never"
func (t *Tests) ClusterTlsRoundTripFromClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "pg-tls")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, clusterHost(name), "pg-tls-server")
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().TLSServerSecurity(cert, key),
		dagger.PostgresClusterOpts{Name: name},
	)
	clientSec := dag.Postgres().TLSClientSecurity(ca.CertPemFile())

	table, err := randIdent(ctx, "t_")
	if err != nil {
		return err
	}
	writer := cluster.Client(clientSec)
	if _, err := writer.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id int)", table)); err != nil {
		return fmt.Errorf("create table over TLS: %w", err)
	}
	if _, err := writer.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES (1), (2), (3)", table)); err != nil {
		return fmt.Errorf("insert over TLS: %w", err)
	}
	count, err := cluster.Client(clientSec).Scalar(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table))
	if err != nil {
		return fmt.Errorf("scalar over TLS: %w", err)
	}
	if count != "3" {
		return fmt.Errorf("expected count 3 over TLS round trip, got %q", count)
	}
	return nil
}

// ClusterMtlsRoundTripFromClient boots a mutual-TLS primary and proves a
// matching mTLS client (presenting a client cert signed by the trusted
// CA) can round-trip Exec + Scalar.
//
// +cache="never"
func (t *Tests) ClusterMtlsRoundTripFromClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	// One CA both signs the server leaf and anchors the accepted client
	// certs — the simplest symmetric mTLS trust setup.
	ca, err := freshCa(ctx, "pg-mtls")
	if err != nil {
		return err
	}
	serverCert, serverKey, err := issueServerCert(ctx, ca, clusterHost(name), "pg-mtls-server")
	if err != nil {
		return err
	}
	clientCert, clientKey, err := issueClientCert(ctx, ca, "postgres", "pg-mtls-client")
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().MtlsServerSecurity(serverCert, serverKey, ca.CertPemFile()),
		dagger.PostgresClusterOpts{Name: name},
	)
	clientSec := dag.Postgres().MtlsClientSecurity(ca.CertPemFile(), clientCert, clientKey)

	table, err := randIdent(ctx, "t_")
	if err != nil {
		return err
	}
	writer := cluster.Client(clientSec)
	if _, err := writer.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id int)", table)); err != nil {
		return fmt.Errorf("create table over mTLS: %w", err)
	}
	if _, err := writer.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES (1), (2)", table)); err != nil {
		return fmt.Errorf("insert over mTLS: %w", err)
	}
	count, err := cluster.Client(clientSec).Scalar(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table))
	if err != nil {
		return fmt.Errorf("scalar over mTLS: %w", err)
	}
	if count != "2" {
		return fmt.Errorf("expected count 2 over mTLS round trip, got %q", count)
	}
	return nil
}

// TlsClusterRejectsPlaintextClient verifies the mode-coupling check:
// asking a TLS cluster for a plaintext client returns an error naming
// both modes, before any wire activity.
//
// +cache="never"
func (t *Tests) TlsClusterRejectsPlaintextClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "pg-tls-reject")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, clusterHost(name), "pg-tls-reject-server")
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().TLSServerSecurity(cert, key),
		dagger.PostgresClusterOpts{Name: name},
	)
	err = cluster.Client(dag.Postgres().PlaintextClientSecurity()).Ping(ctx)
	if err == nil {
		return fmt.Errorf("expected plaintext client against TLS cluster to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "plaintext") || !strings.Contains(msg, "TLS") {
		return fmt.Errorf("expected mode-mismatch error naming both modes, got: %v", err)
	}
	return nil
}

// TlsClusterRejectsEmptyName verifies a TLS cluster rejects an empty
// `name`. The cluster hostname — and therefore the SAN the server cert
// must carry — derives from `name` alone, so an empty name would
// collapse every TLS/mTLS cluster onto the same sha256("") host and
// invite cert/SAN reuse. The guard fires in the constructor, before any
// service starts, so a placeholder SAN on the cert is fine here.
//
// +cache="never"
func (t *Tests) TlsClusterRejectsEmptyName(ctx context.Context) error {
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "pg-tls-emptyname")
	if err != nil {
		return err
	}
	// SAN value is irrelevant: the empty-name guard rejects before any
	// dial or TLS handshake.
	cert, key, err := issueServerCert(ctx, ca, "postgres-placeholder", "pg-tls-emptyname-server")
	if err != nil {
		return err
	}
	// No Name opt → defaults to "".
	cluster := dag.Postgres().Cluster(pass, dag.Postgres().TLSServerSecurity(cert, key))
	_, err = cluster.Endpoint(ctx)
	if err == nil {
		return fmt.Errorf("expected TLS cluster with empty name to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "name") || !strings.Contains(msg, "TLS") {
		return fmt.Errorf("expected empty-name rejection naming TLS, got: %v", err)
	}
	return nil
}

// MtlsClusterRejectsTlsOnlyClient boots an mTLS primary (started via a
// valid mTLS client so the service is ready), then dials it with a
// TLS-only standalone client that presents no client certificate. The
// standalone Postgres.Client has no cluster reference, so it bypasses the
// coupling check and reaches the wire, where the listener's
// clientcert=verify-full rejects it with a TLS/cert error.
//
// +cache="never"
func (t *Tests) MtlsClusterRejectsTlsOnlyClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "pg-mtls-reject")
	if err != nil {
		return err
	}
	serverCert, serverKey, err := issueServerCert(ctx, ca, clusterHost(name), "pg-mtls-reject-server")
	if err != nil {
		return err
	}
	clientCert, clientKey, err := issueClientCert(ctx, ca, "postgres", "pg-mtls-reject-client")
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().MtlsServerSecurity(serverCert, serverKey, ca.CertPemFile()),
		dagger.PostgresClusterOpts{Name: name},
	)
	// Start + ready the primary using a valid mTLS client.
	mtlsSec := dag.Postgres().MtlsClientSecurity(ca.CertPemFile(), clientCert, clientKey)
	if err := cluster.Client(mtlsSec).Ping(ctx); err != nil {
		return fmt.Errorf("expected valid mTLS client to connect: %w", err)
	}

	ep, err := cluster.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	user, err := cluster.User(ctx)
	if err != nil {
		return err
	}
	db, err := cluster.Database(ctx)
	if err != nil {
		return err
	}
	// TLS-only: trusts the server CA but presents no client cert.
	tlsOnly := dag.Postgres().TLSClientSecurity(ca.CertPemFile())
	err = dag.Postgres().Client(hostOf(ep), user, db, pass, tlsOnly).Ping(ctx)
	if err == nil {
		return fmt.Errorf("expected TLS-only client to be rejected by the mTLS listener")
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "certificate") &&
		!strings.Contains(low, "tls") &&
		!strings.Contains(low, "ssl") {
		return fmt.Errorf("expected a TLS/certificate error, got: %v", err)
	}
	return nil
}

// BindPrimaryResolvesFromUserContainerTls binds a TLS primary into an
// alpine container running psql: a verify-full connection with the right
// CA succeeds, and the same connection without sslrootcert fails (it
// cannot verify the server).
//
// +cache="never"
func (t *Tests) BindPrimaryResolvesFromUserContainerTls(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "pg-tls-bind")
	if err != nil {
		return err
	}
	host := clusterHost(name)
	cert, key, err := issueServerCert(ctx, ca, host, "pg-tls-bind-server")
	if err != nil {
		return err
	}
	cluster := dag.Postgres().Cluster(
		pass,
		dag.Postgres().TLSServerSecurity(cert, key),
		dagger.PostgresClusterOpts{Name: name},
	)

	base := cluster.BindPrimary(dag.Container().From("alpine:3")).
		WithExec([]string{"apk", "add", "--no-cache", "postgresql-client"}).
		WithFile("/tmp/ca.crt", ca.CertPemFile()).
		WithSecretVariable("PGPASSWORD", pass)

	// verify-full WITH the CA succeeds (retried: the primary flaps its
	// listener once during initdb before settling).
	okCmd := fmt.Sprintf(
		"for i in $(seq 1 30); do "+
			"psql 'host=%s port=5432 user=postgres dbname=postgres sslmode=verify-full sslrootcert=/tmp/ca.crt' -tAc 'SELECT 1' && exit 0; "+
			"sleep 1; done; echo TIMEOUT; exit 1",
		host,
	)
	out, err := base.WithExec([]string{"sh", "-c", okCmd}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("verify-full psql with sslrootcert should succeed: %w", err)
	}
	if !strings.Contains(out, "1") {
		return fmt.Errorf("expected psql to return 1, got %q", out)
	}

	// verify-full WITHOUT the CA fails: it cannot verify the server cert.
	failCmd := fmt.Sprintf(
		"psql 'host=%s port=5432 user=postgres dbname=postgres sslmode=verify-full' -tAc 'SELECT 1'",
		host,
	)
	if _, err := base.WithExec([]string{"sh", "-c", failCmd}).Sync(ctx); err == nil {
		return fmt.Errorf("expected verify-full without sslrootcert to fail, but it succeeded")
	}
	return nil
}
