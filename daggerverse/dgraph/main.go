// Dgraph provides Dagger functions for spinning up Dgraph graph-database
// clusters (one Zero coordinator + N Alpha data nodes grouped at a
// configurable replication factor) and a pure-Go dgo-based client that
// can target either the local cluster or any reachable remote cluster
// (e.g. Dgraph Cloud, an existing self-hosted cluster).
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - security.go  — *ServerSecurity / *ClientSecurity + the two
//                    Plaintext constructors. TLS / mTLS variants land
//                    in a follow-up; the empty-struct types are kept
//                    distinct so future constructors slot in without
//                    changing Cluster / Client signatures.
//   - cluster.go   — *Cluster + Dgraph.Cluster, input validation, the
//                    topology builder (one Zero + N Alphas), and the
//                    Stop / GrpcEndpoints / HttpEndpoints / BindAlphas /
//                    BindZeros / Client methods.
//   - client.go    — *Client + Dgraph.Client, dgo wiring, and the
//                    DropAll / AlterSchema / Mutate / Query /
//                    QueryWithVars method set.
package main

// Dgraph is the root namespace for every exported function in this
// module. All cluster constructors, security helpers, and the
// remote-client factory hang off *Dgraph so the generated Dagger SDK
// surfaces them under `dag.Dgraph().<Func>(...)`.
type Dgraph struct{}
