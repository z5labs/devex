# postgres

Daggerverse module that spins up a single-node PostgreSQL 17 primary
(from the upstream `postgres` image) and exposes a pure-Go client (built
on [`github.com/jackc/pgx/v5`](https://github.com/jackc/pgx)) that can
target either the local cluster or a remote PostgreSQL (e.g. AWS RDS,
Cloud SQL).

The client listener supports three security modes: plaintext
(scram-sha-256 password auth over an unencrypted TCP listener), one-way
TLS, and mutual TLS. Primary/replica streaming replication lands in a
follow-up.

## Security profiles

A `*ServerSecurity` configures the primary's `:5432` listener; a matching
`*ClientSecurity` configures how a client connects. Cert material is
caller-supplied PEM — PostgreSQL reads it natively (`ssl_cert_file` /
`ssl_key_file` / `ssl_ca_file`) and pgx verifies against PEM roots.

```go
// Plaintext — scram-sha-256 over an unencrypted TCP listener.
Postgres.PlaintextServerSecurity() *ServerSecurity
Postgres.PlaintextClientSecurity() *ClientSecurity

// One-way TLS — the primary presents serverCert; clients still
// authenticate with the password. Plaintext TCP is refused.
Postgres.TlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret) *ServerSecurity
Postgres.TlsClientSecurity(serverCa *dagger.File) *ClientSecurity

// Mutual TLS — clients must additionally present a cert signed by
// clientCa (clientcert=verify-full) on top of the password.
Postgres.MtlsServerSecurity(serverCert *dagger.File, serverKey *dagger.Secret, clientCa *dagger.File) *ServerSecurity
Postgres.MtlsClientSecurity(serverCa *dagger.File, clientCert *dagger.File, clientKey *dagger.Secret) *ClientSecurity
```

For TLS / mTLS, `serverCert`'s SAN must cover the hostname the client
dials — `sslmode=verify-full` checks the SAN against that host. The
cluster hostname is `postgres-<sha12(name)>`, derived from `name` alone
so a caller can predict it when minting the certificate. With
`clientcert=verify-full`, the mTLS client certificate's Common Name must
also equal the connecting role.

**Mode coupling.** `Cluster.Client(security)` validates that the client's
mode exactly matches the cluster's listener mode and otherwise returns an
error naming both modes (e.g. *"client uses plaintext but cluster
listener is mTLS"*). The standalone `Postgres.Client(...)` has no cluster
reference, so it cannot cross-validate — a mismatched standalone client
fails at the wire instead.

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
boot): `password == nil`, `clientListenerSecurity == nil`, an incomplete
TLS / mTLS profile (missing cert, key, or client CA), `user == ""`,
`db == ""`.

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
Client.Exec(ctx, sql string) (int, error)          // affected-row count (surfaced as GraphQL Int)
Client.Scalar(ctx, sql string) (string, error)     // first column of first row; errors on zero rows
Client.ApplyFile(ctx, file *dagger.File) error     // runs a .sql file's statements on one connection
Client.QueryJSON(ctx, sql string) (*dagger.File, error) // JSON array of column-keyed row objects
```

`ApplyFile` splits statements on `;` outside single/double-quoted
strings, `--` line comments, `/* */` (nesting) block comments, and
`$$` / `$tag$` dollar-quoted strings.

## Follow-ups

Primary/replica streaming replication; connection pooling.
