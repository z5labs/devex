# postgres

Daggerverse module that spins up a single-node PostgreSQL 17 primary
(from the upstream `postgres` image) and exposes a pure-Go client (built
on [`github.com/jackc/pgx/v5`](https://github.com/jackc/pgx)) that can
target either the local cluster or a remote PostgreSQL (e.g. AWS RDS,
Cloud SQL).

This story is **plaintext-only**: scram-sha-256 password auth over an
unencrypted TCP listener. TLS / mTLS and primary/replica streaming
replication land in follow-ups; the empty-struct security profile types
are kept distinct so future constructors slot in without changing the
`Cluster` / `Client` signatures.

## Security profiles

```go
Postgres.PlaintextServerSecurity() *ServerSecurity
Postgres.PlaintextClientSecurity() *ClientSecurity
```

## Cluster

Single-node primary listening on 5432, scram-sha-256 password auth over
plaintext TCP. Built from a single `<registry>/library/postgres:<tag>`
image; the `library/postgres` portion is fixed and only `registry` and
`tag` are caller-overridable. Default tag is `"17"`.

```go
Postgres.Cluster(
    ctx,
    name="",
    registry="docker.io", tag="17",
    user="postgres", db="postgres",
    password *dagger.Secret,
    clientListenerSecurity *ServerSecurity,
) (*Cluster, error)

Cluster.Endpoint() string              // host:5432 (pure accessor; BindPrimary makes it reachable)
Cluster.User() string
Cluster.Database() string
Cluster.Password() *dagger.Secret
Cluster.BindPrimary(*dagger.Container) *dagger.Container
Cluster.Client(ctx, security *ClientSecurity) (*Client, error)
Cluster.Stop(ctx) error
```

Rejected inputs (each a descriptive error rather than a half-broken
boot): `password == nil`, `clientListenerSecurity == nil` or a
non-`PLAINTEXT` mode, `user == ""`, `db == ""`.

`Cluster()` is `+cache="session"` so a single test's chained
`Client.Exec` → `Client.Scalar` calls observe the same backing service
and its state. The `name` argument folds into that session-cache key:
pass a unique value per parallel test to get isolated services. Every
method on `*Cluster` / `*Client` is `+cache="never"` so any
data-returning call re-executes per invocation.

`Endpoint()` is a pure accessor and does **not** start the service.
Reachability from a consumer container comes from `BindPrimary`, which
lets `WithServiceBinding` start the service as the consumer's dependency
and wire its IP into `/etc/hosts`. (Pre-starting from this module would
register the service in the module's DNS domain, which a session-domain
consumer's host-file lookup can't resolve.) For module-runtime access,
use `Cluster.Client`, which starts the service itself.

## Client

Pure-Go pgx-based client. No container image. Works against the local
cluster or any reachable remote PostgreSQL.

```go
Postgres.Client(
    host string, port=5432,
    user, db string,
    password *dagger.Secret,
    security *ClientSecurity,
) *Client

Client.Ping(ctx) error
Client.Exec(ctx, sql string) (int64, error)        // affected-row count
Client.Scalar(ctx, sql string) (string, error)     // first column of first row; errors on zero rows
Client.ApplyFile(ctx, file *dagger.File) error     // runs a .sql file's statements on one connection
Client.QueryJSON(ctx, sql string) (*dagger.File, error) // JSON array of column-keyed row objects
```

`ApplyFile` splits statements on `;` outside single/double-quoted
strings, `--` line comments, `/* */` (nesting) block comments, and
`$$` / `$tag$` dollar-quoted strings.

## Follow-ups

TLS / mTLS listeners and client connections; primary/replica streaming
replication; connection pooling.
