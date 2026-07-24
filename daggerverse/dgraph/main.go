// Dgraph provides Dagger functions for spinning up Dgraph graph-database
// clusters (one Zero coordinator + N Alpha data nodes grouped at a
// configurable replication factor) and a pure-Go dgo-based client that
// can target either the local cluster or any reachable remote cluster
// (e.g. Dgraph Cloud, an existing self-hosted cluster).
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - security.go       — *ServerSecurity / *ClientSecurity + the
//                          Plaintext / TLS / mTLS constructors and the
//                          server-profile validation.
//   - serversecurity.go — mounts the caller-supplied TLS material onto
//                          each Alpha / Zero container and renders the
//                          `--tls` superflag args.
//   - cluster.go        — *Cluster + Dgraph.Cluster, input validation,
//                          the topology builder (one Zero + N Alphas),
//                          and the Stop / GrpcEndpoints / HttpEndpoints /
//                          BindAlphas / Client methods (with mode
//                          coupling).
//   - client.go         — *Client + Dgraph.Client, dgo wiring including
//                          the client-side *tls.Config, and the DropAll /
//                          AlterSchema / Mutate / RunQuery / QueryWithVars
//                          method set.
package main

// Dgraph is the root namespace for every exported function in this
// module. All cluster constructors, security helpers, and the
// remote-client factory hang off *Dgraph so the generated Dagger SDK
// surfaces them under `dag.Dgraph().<Func>(...)`.
type Dgraph struct{}
