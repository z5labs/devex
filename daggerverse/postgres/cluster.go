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
	if clientListenerSecurity.Mode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"only PLAINTEXT clientListenerSecurity is supported in this story, got %q",
			clientListenerSecurity.Mode,
		)
	}
	if user == "" {
		return nil, fmt.Errorf("user must not be empty")
	}
	if db == "" {
		return nil, fmt.Errorf("db must not be empty")
	}

	image := fmt.Sprintf("%s/library/postgres:%s", registry, tag)

	// Stable hostname is scoped per-cluster so parallel invocations don't
	// collide on a single `postgres` alias. The suffix is a deterministic
	// hash over every argument that distinguishes one session-cache entry
	// from another — including the password secret's ID and the listener
	// security mode, not just name/registry/tag/user/db — so two distinct
	// cache entries can never share a hostname within one engine session,
	// while identical-arg calls reuse the cached entry (and this value)
	// without re-running this path.
	passID, err := password.ID(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve password secret id: %w", err)
	}
	keyBytes := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s|%s|%s|%s|%s",
		name, registry, tag, user, db, clientListenerSecurity.Mode, passID,
	))
	host := "postgres-" + hex.EncodeToString(keyBytes[:6]) // 12 hex chars = 48 bits

	svc := dag.Container().
		From(image).
		WithSecretVariable("POSTGRES_PASSWORD", password).
		WithEnvVariable("POSTGRES_USER", user).
		WithEnvVariable("POSTGRES_DB", db).
		WithExposedPort(5432).
		// The postgres image bootstraps the data directory, superuser,
		// and default database in its docker-entrypoint.sh, then execs
		// the `postgres` server. UseEntrypoint runs that script (without
		// it Dagger would launch `postgres` raw, skipping initdb). When
		// POSTGRES_PASSWORD is set the image defaults host auth to
		// scram-sha-256, which is exactly this story's security mode.
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true}).
		WithHostname(host)

	return &Cluster{
		Svc:      svc,
		Host:     host,
		UserName: user,
		DbName:   db,
		Pass:     password,
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
// +cache="never"
func (c *Cluster) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	return clientFrom(c.Host, 5432, c.UserName, c.DbName, c.Pass, security), nil
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
// becomes session-reachable from the postgres module runtime, then
// polls a real pgx connection until the server accepts queries.
//
// The postgres entrypoint listens on 5432 only after initdb completes
// and it has restarted into normal operation, so an early dial returns
// "connection refused" or "the database system is starting up"; the
// retry loop absorbs both. This is the pure-Go analogue of dgraph's
// HTTP /health poll — no helper container in the module runtime.
func (c *Cluster) start(ctx context.Context) error {
	if c.Svc == nil {
		return fmt.Errorf("cluster has no backing service")
	}
	if _, err := c.Svc.Start(ctx); err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}
	password, err := c.Pass.Plaintext(ctx)
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}

	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := pgConnect(attemptCtx, c.Host, 5432, c.UserName, c.DbName, password)
		if err == nil {
			pingErr := conn.Ping(attemptCtx)
			_ = conn.Close(attemptCtx)
			cancel()
			if pingErr == nil {
				return nil
			}
			lastErr = pingErr
		} else {
			cancel()
			lastErr = err
		}
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
