// Valkey provides Dagger functions for spinning up Valkey topologies
// (from the upstream `valkey/valkey` image) — a single node, a primary
// with N read replicas, or a slot-sharded Valkey Cluster — plus a pure-Go
// valkey-go based client that can target either a local topology or any
// reachable remote Valkey (e.g. ElastiCache Serverless, MemoryDB, a
// self-hosted node).
//
// Three client-facing listener modes are supported: PLAINTEXT
// (`requirepass` auth over an unencrypted TCP listener), TLS (one-way:
// the node presents a server certificate on a `--tls-port` listener and
// clients still authenticate with the password), and MTLS (mutual: the
// client must additionally present a certificate signed by the node's
// trusted CA). Replication and Cluster are plaintext-only for now — see
// Valkey.Replication and Valkey.Cluster.
//
// The single-node type is `Server`, not `Cluster`: in Valkey "cluster"
// means slot-sharded Valkey Cluster, and that is what `Cluster` is.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - security.go    — *ServerSecurity / *ClientSecurity, the Plaintext /
//     Tls / Mtls constructors, and the listener-mode rendering
//     (validateServerSecurity / applyServerSecurity).
//   - server.go      — *Server + Valkey.Server, input validation, the
//     shared node builder (buildServer), and the Endpoint / User /
//     Password / BindServer / Client / Stop methods.
//   - replication.go — *Replication + Valkey.Replication, the
//     primary/replica topology builder, and the Primary / Replicas /
//     Stop methods.
//   - cluster.go     — *Cluster + Valkey.Cluster, the symmetric-peer node
//     builder, the concurrent start (startAll) and the containerised
//     slot-assignment bootstrap, and the Endpoints / BindNodes / Client /
//     Stop methods.
//   - client.go      — *Client + Valkey.Client, valkey-go wiring, and the
//     Ping / Do / Get / Set / Del / Keys / ApplyFile / Info / DbSize /
//     FlushAll method set — the last of which are cluster-aware.
package main

// Valkey is the root namespace for every exported function in this
// module. The server constructor, security helpers, and the
// remote-client factory all hang off *Valkey so the generated Dagger SDK
// surfaces them under `dag.Valkey().<Func>(...)`.
type Valkey struct{}
