package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"dagger/valkey/internal/dagger"
)

// Replication is a primary/replica Valkey topology: one primary plus N
// asynchronous read replicas. Nodes[0] is always the primary and the
// remainder are its replicas, in the order they were created.
//
// A replica is asymmetric — it dials a primary that is already up — so
// the whole topology is expressible with an ordinary service binding and
// none of the symmetric-peer startup problems of Valkey Cluster apply.
type Replication struct {
	// +private
	Nodes []*Server
}

// Replication boots a primary/replica topology: one primary node plus
// `replicas` read replicas, each booted with `--replicaof <primary-host>
// 6379`, the primary's password, and `--replica-read-only yes`.
//
// Image: `<registry>/valkey/valkey:<tag>` for every node — the topology
// is deliberately homogeneous.
//
// Rejected inputs (each a descriptive error rather than a half-broken
// topology):
//
//   - `password == nil` — the same secret is both the nodes'
//     `requirepass` and the replicas' `masterauth`, so it is mandatory.
//   - `clientListenerSecurity == nil` — plaintext must be a deliberate
//     caller choice, exactly as for a single Server.
//   - a TLS or MTLS profile — see the note below.
//   - `replicas < 1` — a zero-replica "replication" topology is a
//     single node with extra steps, and Valkey.Server already builds
//     that.
//
// TLS/mTLS is not supported for this topology yet. A TLS node runs with
// `--port 0`, so the replication link would have to run over TLS too
// (`--tls-replication yes`), and that link needs trust material this
// profile does not carry: the replica must verify the primary against a
// CA, and under mTLS it must additionally present a client certificate
// the primary's CA accepts. `*ServerSecurity` describes the
// *client-facing* listener only, so a TLS/mTLS profile is rejected here
// rather than silently booting replicas that spin on a failed handshake.
//
// Session-cached for the same reason Valkey.Server is: repeated chained
// calls on the returned topology within one test must observe the SAME
// backing services, and therefore the same keyspace. `name` folds into
// that cache key and into every node's hostname, so parallel test suites
// should pass a unique value per test.
//
// +cache="session"
func (v *Valkey) Replication(
	ctx context.Context,
	// +default=""
	name string,
	// +default="docker.io"
	registry string,
	// +default="9.1"
	tag string,
	// +default=1
	replicas int,
	password *dagger.Secret,
	clientListenerSecurity *ServerSecurity,
) (*Replication, error) {
	if password == nil {
		return nil, fmt.Errorf("password must not be nil; pass a *dagger.Secret with the requirepass value")
	}
	if clientListenerSecurity == nil {
		return nil, fmt.Errorf("clientListenerSecurity must not be nil; pass PlaintextServerSecurity() explicitly")
	}
	if err := validateServerSecurity(clientListenerSecurity); err != nil {
		return nil, err
	}
	if clientListenerSecurity.Mode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"replication does not support %s listeners yet: the replication link would also have to run over TLS, and the replica needs trust material (a CA to verify the primary, plus a client certificate under mTLS) that a client-listener ServerSecurity profile does not carry; pass PlaintextServerSecurity()",
			securityModeLabel(clientListenerSecurity.Mode),
		)
	}
	if replicas < 1 {
		return nil, fmt.Errorf("replicas must be at least 1, got %d; use Valkey.Server for a single node", replicas)
	}

	image := valkeyImage(registry, tag)

	primary := buildServer(replicationNodeName(name, 0), image, password, clientListenerSecurity, nil, nil, nil)

	nodes := make([]*Server, 0, replicas+1)
	nodes = append(nodes, primary)
	for i := 1; i <= replicas; i++ {
		nodes = append(nodes, buildServer(
			replicationNodeName(name, i),
			image,
			password,
			clientListenerSecurity,
			replicaArgs(primary.Host),
			[]serviceBinding{{host: primary.Host, svc: primary.Svc}},
			nil,
		))
	}

	return &Replication{Nodes: nodes}, nil
}

// replicationNodeName derives the per-node `name` each node's hostname is
// hashed from: index 0 is the primary, the rest are replicas. Folding the
// role and index into the name is what gives every node in a topology a
// distinct pinned hostname, keeps two Replication calls with different
// `name`s from colliding, and keeps a node from colliding with a
// Valkey.Server booted under the same `name`.
func replicationNodeName(name string, index int) string {
	if index == 0 {
		return name + "/replication/primary"
	}
	return name + "/replication/replica-" + strconv.Itoa(index)
}

// replicaArgs renders the `valkey-server` flags that turn a node into a
// read replica of primaryHost.
//
// `masterauth` rather than `primaryauth`: Valkey 9.1 accepts both
// spellings (verified against the pinned image), but `primaryauth` only
// exists from Valkey 8.0 onwards, and `tag` is caller-overridable — so
// the older spelling is the one that works across every tag a caller
// could plausibly pass.
//
// `"$VALKEY_PASSWORD"` is expanded by the shell wrapper buildServer wraps
// these args in, from the secret environment variable, so the primary's
// password never enters the replica's argv in the Dagger graph. It is the
// same secret as the node's own `requirepass`: every node in the topology
// shares one password.
//
// `--replica-read-only yes` is Valkey's default, but it is passed
// explicitly so a write against a replica failing with READONLY is a
// property of this topology rather than of an upstream default that could
// move.
func replicaArgs(primaryHost string) []string {
	return []string{
		"--replicaof", primaryHost, strconv.Itoa(valkeyPort),
		"--masterauth", `"$VALKEY_PASSWORD"`,
		"--replica-read-only", "yes",
	}
}

// Primary returns the topology's primary node — the only node that
// accepts writes.
//
// Session-cached rather than never-cached: Dagger v0.21 detaches module
// objects returned from a `+cache="never"` function when a consumer
// module reads their fields lazily, and `tests/` is such a consumer. The
// methods on the returned *Server are individually never-cached, so no
// data-returning call is served stale.
//
// +cache="session"
func (rep *Replication) Primary() *Server {
	if len(rep.Nodes) == 0 {
		return nil
	}
	return rep.Nodes[0]
}

// Replicas returns the topology's read replicas, in creation order.
// Session-cached for the same reason Primary is.
//
// +cache="session"
func (rep *Replication) Replicas() []*Server {
	if len(rep.Nodes) == 0 {
		return nil
	}
	return rep.Nodes[1:]
}

// Stop tears down every node in the topology, replicas first so the
// primary does not spend its last moments logging dropped links. Every
// node is attempted even if an earlier one fails, and the failures are
// joined — a partial teardown that reported only the first error would
// leave services running with nothing naming them.
//
// +cache="never"
func (rep *Replication) Stop(ctx context.Context) error {
	var errs []error
	for i := len(rep.Nodes) - 1; i >= 0; i-- {
		if err := rep.Nodes[i].Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("node %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}
