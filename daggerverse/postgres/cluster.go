package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"dagger/postgres/internal/dagger"
)

// Cluster represents a running single-node PostgreSQL primary plus the
// connection metadata callers need to reach it. Holds a reference to
// the backing service so callers can bind it into their own containers
// or open a pgx Client against it.
type Cluster struct {
	// +private
	Svc *dagger.Service
	// +private
	Host string
	// +private
	UserName string
	// +private
	DbName string
	// +private
	Pass *dagger.Secret
	// +private
	ClientListenerMode string // PLAINTEXT | TLS | MTLS — drives Client coupling validation.
}

// Cluster spins up a single-node PostgreSQL primary listening on 5432
// with scram-sha-256 password auth over a plaintext TCP listener (the
// only security mode in this story).
//
// Image: `<registry>/library/postgres:<tag>` — the `library/postgres`
// portion is fixed; only `registry` and `tag` are caller-overridable.
// The default tag `"17"` pins this story to PostgreSQL 17.
//
// Rejected inputs (each surfaces a descriptive error rather than
// booting a half-broken cluster):
//
//   - `password == nil` — the primary refuses to start without a
//     superuser password and a plaintext-password cluster needs one.
//   - `clientListenerSecurity == nil` or a non-PLAINTEXT mode —
//     plaintext must be a deliberate caller choice so a future TLS
//     upgrade stays explicit.
//   - `user == ""` / `db == ""` — the postgres image needs both to
//     provision the superuser role and the default database.
//
// Session-cached so that repeated chained method calls on the returned
// cluster (e.g. Client.Exec → Client.Scalar across two Cluster.Client()
// calls in `exec-scalar-round-trip`) observe the SAME underlying
// service — and therefore the same on-disk state. Every method on
// *Cluster and *Client is independently marked never-cache, so any
// data-returning call re-executes per invocation.
//
// `name` is a caller-supplied discriminator that folds into the session
// cache key. Parallel test suites should pass a unique value per test
// so each test gets its own backing service — without it, every
// same-shape call collapses to one cached cluster and concurrent tests
// race on shared tables and storage. Same name + same shape still
// cache-hits, which is what a single test's chained Client.Exec →
// Client.Scalar sequence needs. Leaving the default empty is fine for
// ad-hoc `dagger call` use where only one cluster is in play.
//
// +cache="session"
func (p *Postgres) Cluster(
	ctx context.Context,
	// +default=""
	name string,
	// +default="docker.io"
	registry string,
	// +default="17"
	tag string,
	// +default="postgres"
	user string,
	// +default="postgres"
	db string,
	password *dagger.Secret,
	clientListenerSecurity *ServerSecurity,
) (*Cluster, error) {
	if password == nil {
		return nil, fmt.Errorf("password must not be nil; pass a *dagger.Secret with the superuser password")
	}
	if clientListenerSecurity == nil {
		return nil, fmt.Errorf("clientListenerSecurity must not be nil; pass PlaintextServerSecurity() explicitly")
	}
	if err := validateServerSecurity(clientListenerSecurity); err != nil {
		return nil, err
	}
	if user == "" {
		return nil, fmt.Errorf("user must not be empty")
	}
	if db == "" {
		return nil, fmt.Errorf("db must not be empty")
	}
	// For TLS / mTLS the hostname is derived from `name` alone (so the
	// caller can mint a server cert whose SAN matches the dialed host).
	// An empty `name` collapses every such cluster onto the same
	// sha256("") hostname, colliding within one engine session and
	// inviting the wrong cert/SAN to be reused — so require a discriminator.
	if name == "" && clientListenerSecurity.Mode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"name must not be empty for %s clusters: the hostname derives from name and the server certificate's SAN must match it, so each TLS/mTLS cluster needs a unique name",
			securityModeLabel(clientListenerSecurity.Mode),
		)
	}

	image := fmt.Sprintf("%s/library/postgres:%s", registry, tag)

	// Stable hostname is scoped per-cluster so parallel invocations don't
	// collide on a single `postgres` alias. It is derived from `name`
	// alone so a caller minting a TLS server certificate can predict the
	// hostname and embed it as the cert's SAN (sslmode=verify-full checks
	// the SAN against the dialed host). This mirrors the kafka module,
	// whose broker hostnames derive purely from the caller-supplied
	// clusterId. `name` does not need to feed the +cache="session" key —
	// Dagger derives that from the full argument set independently — so
	// dropping registry/tag/user/db/mode/passwordID from the hostname
	// hash is safe as long as callers pass a unique `name` per cluster
	// (already the documented expectation, and what every test does).
	keyBytes := sha256.Sum256([]byte(name))
	host := "postgres-" + hex.EncodeToString(keyBytes[:6]) // 12 hex chars = 48 bits

	ctr := dag.Container().
		From(image).
		WithSecretVariable("POSTGRES_PASSWORD", password).
		WithEnvVariable("POSTGRES_USER", user).
		WithEnvVariable("POSTGRES_DB", db).
		WithExposedPort(5432)

	// Render the TLS cert mounts and the postgres startup args matching
	// the listener mode. PLAINTEXT leaves the image defaults untouched
	// (scram-sha-256 over a plaintext TCP listener); TLS / MTLS mount the
	// caller-supplied PEM material and a custom pg_hba.conf.
	ctr, args := applyServerSecurity(ctr, clientListenerSecurity)

	// The postgres image bootstraps the data directory, superuser, and
	// default database in its docker-entrypoint.sh, then execs the
	// `postgres` server. UseEntrypoint runs that script (without it Dagger
	// would launch `postgres` raw, skipping initdb). When POSTGRES_PASSWORD
	// is set the image defaults host auth to scram-sha-256.
	svc := ctr.
		AsService(dagger.ContainerAsServiceOpts{Args: args, UseEntrypoint: true}).
		WithHostname(host)

	return &Cluster{
		Svc:                svc,
		Host:               host,
		UserName:           user,
		DbName:             db,
		Pass:               password,
		ClientListenerMode: clientListenerSecurity.Mode,
	}, nil
}

// Endpoint returns the primary's `host:5432` address. It does NOT start
// the service: it is a pure accessor, mirroring kafka's BootstrapServers.
// BindPrimary is what makes that address reachable from a consumer
// container (WithServiceBinding starts the service as the consumer's
// dependency and wires its IP into /etc/hosts). For module-runtime
// access use Cluster.Client, which starts the service itself.
//
// Pre-starting the service from this module before a consumer binds it
// would register the service in the module's DNS domain, which the
// binding's host-file lookup can't resolve from a session-domain
// consumer — so the start must be driven by the binding, not here.
//
// +cache="never"
func (c *Cluster) Endpoint() string {
	return c.Host + ":5432"
}

// User returns the superuser role name the cluster was provisioned
// with.
//
// +cache="never"
func (c *Cluster) User() string {
	return c.UserName
}

// Database returns the default database name the cluster was
// provisioned with.
//
// +cache="never"
func (c *Cluster) Database() string {
	return c.DbName
}

// Password returns the superuser password secret the cluster was
// provisioned with, so callers can re-use it via Postgres.Client
// against the same endpoint.
//
// +cache="never"
func (c *Cluster) Password() *dagger.Secret {
	return c.Pass
}

// BindPrimary attaches the primary service to the given container under
// the same hostname Endpoint reports, so the container can dial the
// primary at `Endpoint()` (e.g. `pg_isready -h <host>`).
//
// +cache="never"
func (c *Cluster) BindPrimary(ctr *dagger.Container) *dagger.Container {
	return ctr.WithServiceBinding(c.Host, c.Svc)
}

// Client starts the primary and returns a pgx Client wired with its
// endpoint, superuser role, default database, and password.
//
// The supplied ClientSecurity mode must match the cluster's listener
// mode (PLAINTEXT/TLS/MTLS); a mismatch returns an error naming both
// modes rather than failing opaquely at the wire. Readiness is then
// probed with the client itself, so a TLS / mTLS listener is polled over
// TLS using the caller's own cert material — the only way to
// authenticate the probe against an mTLS listener.
//
// +cache="never"
func (c *Cluster) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	if err := c.requireMode(security); err != nil {
		return nil, err
	}
	client := clientFrom(c.Host, 5432, c.UserName, c.DbName, c.Pass, security)
	if err := c.start(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// requireMode validates that the client's security mode exactly matches
// the cluster's client-facing listener mode. Postgres.Client (the
// standalone constructor) has no cluster reference and therefore cannot
// perform this check — callers reaching a listener via a mismatched
// standalone client fail at the wire instead.
func (c *Cluster) requireMode(security *ClientSecurity) error {
	clientMode := "PLAINTEXT"
	if security != nil {
		clientMode = security.Mode
	}
	if clientMode != c.ClientListenerMode {
		return fmt.Errorf(
			"client uses %s but cluster listener is %s",
			securityModeLabel(clientMode), securityModeLabel(c.ClientListenerMode),
		)
	}
	return nil
}

// securityModeLabel renders a mode constant as the spelling used in
// user-facing error messages.
func securityModeLabel(mode string) string {
	switch mode {
	case "PLAINTEXT":
		return "plaintext"
	case "TLS":
		return "TLS"
	case "MTLS":
		return "mTLS"
	default:
		return mode
	}
}

// Stop tears down the service container backing this cluster. Tests
// should call this in a defer so the service span closes when the test
// returns. SIGKILL skips graceful shutdown — Postgres' checkpoint-on-
// shutdown path is wasted work for a torn-down test cluster.
//
// +cache="never"
func (c *Cluster) Stop(ctx context.Context) error {
	if c.Svc == nil {
		return nil
	}
	if _, err := c.Svc.Stop(ctx, dagger.ServiceStopOpts{Kill: true}); err != nil {
		return fmt.Errorf("stop postgres: %w", err)
	}
	return nil
}

// start explicitly Starts the primary service so its WithHostname alias
// becomes session-reachable from the postgres module runtime, then polls
// the supplied probe Client until the server accepts authenticated
// queries. Probing through the Client means the dial honours the
// listener's security mode (plaintext / TLS / mTLS) using the caller's
// own cert material.
//
// The postgres entrypoint listens on 5432 only after initdb completes
// and it has restarted into normal operation, so an early dial returns
// "connection refused" or "the database system is starting up" (and, on
// a TLS listener, a transient handshake error); the retry loop absorbs
// all of them. This is the pure-Go analogue of dgraph's HTTP /health
// poll — no helper container in the module runtime.
func (c *Cluster) start(ctx context.Context, probe *Client) error {
	if c.Svc == nil {
		return fmt.Errorf("cluster has no backing service")
	}
	if _, err := c.Svc.Start(ctx); err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}

	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := probe.Ping(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return fmt.Errorf("postgres %s not ready: %w", c.Host, lastErr)
}
