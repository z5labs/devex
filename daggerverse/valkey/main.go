// Valkey provides Dagger functions for spinning up a single-node Valkey
// server (from the upstream `valkey/valkey` image) and a pure-Go
// valkey-go based client that can target either the local node or any
// reachable remote Valkey (e.g. ElastiCache Serverless, MemoryDB, a
// self-hosted node).
//
// This module is plaintext-only: `requirepass` auth over an unencrypted
// TCP listener. TLS / mTLS, primary/replica replication, and Valkey
// Cluster land in follow-ups; the empty-but-distinct security types are
// kept so future constructors slot in without changing the Server /
// Client signatures.
//
// The single-node type is `Server`, not `Cluster`: in Valkey "cluster"
// means slot-sharded Valkey Cluster, so that name is reserved for the
// follow-up that adds it.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - security.go — *ServerSecurity / *ClientSecurity + the two
//     Plaintext constructors.
//   - server.go   — *Server + Valkey.Server, input validation, the
//     single-node topology builder, and the Endpoint / User / Password /
//     BindServer / Client / Stop methods.
//   - client.go   — *Client + Valkey.Client, valkey-go wiring, and the
//     Ping / Do / Get / Set / Del / Keys / ApplyFile / Info / DbSize /
//     FlushAll method set.
package main

// Valkey is the root namespace for every exported function in this
// module. The server constructor, security helpers, and the
// remote-client factory all hang off *Valkey so the generated Dagger SDK
// surfaces them under `dag.Valkey().<Func>(...)`.
type Valkey struct{}
