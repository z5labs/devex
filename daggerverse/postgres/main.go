// Postgres provides Dagger functions for spinning up a single-node
// PostgreSQL 17 primary (from the upstream `postgres` image) and a
// pure-Go pgx-based client that can target either the local cluster or
// any reachable remote PostgreSQL (e.g. AWS RDS, Cloud SQL).
//
// This module is plaintext-only: scram-sha-256 password auth over an
// unencrypted TCP listener. TLS / mTLS and primary/replica streaming
// replication land in follow-ups; the empty-but-distinct security
// types are kept so future constructors slot in without changing the
// Cluster / Client signatures.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - security.go  — *ServerSecurity / *ClientSecurity + the two
//                    Plaintext constructors.
//   - cluster.go   — *Cluster + Postgres.Cluster, input validation, the
//                    single-node topology builder, and the Endpoint /
//                    User / Database / Password / BindPrimary / Client /
//                    Stop methods.
//   - client.go    — *Client + Postgres.Client, pgx wiring, and the
//                    Ping / Exec / Scalar / ApplyFile / QueryJSON method
//                    set.
package main

// Postgres is the root namespace for every exported function in this
// module. All cluster constructors, security helpers, and the
// remote-client factory hang off *Postgres so the generated Dagger SDK
// surfaces them under `dag.Postgres().<Func>(...)`.
type Postgres struct{}
