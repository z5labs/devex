package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"dagger/valkey/internal/dagger"
)

// clusterBusPort is the port cluster members gossip over. Valkey derives
// it from the client port by adding 10000 and there is no reason to move
// it in a throwaway topology, so — like valkeyPort — it is fixed rather
// than caller-overridable.
const clusterBusPort = valkeyPort + 10000

// minClusterShards is the smallest shard count Valkey Cluster can run
// with. Slot ownership changes are agreed by a majority vote of the
// primaries, so two primaries can never form a majority once one of them
// is unreachable, and a single primary is a standalone node wearing a
// cluster hat.
const minClusterShards = 3

// clusterBootstrapAttempts is how many one-second attempts the bootstrap
// script gives each wait. Node startup and slot assignment are both
// near-instant; the headroom is for image pulls and for gossip to
// converge on a contended engine.
const clusterBootstrapAttempts = 180

// clusterBootstrapMarker is the file the bootstrap script writes once
// every node agrees the cluster is up. BindNodes grafts it into the
// consumer container purely so the consumer's evaluation cannot start
// before the bootstrap has finished.
const clusterBootstrapMarker = "/bootstrap.done"

// clusterBoundMarker is where BindNodes lands that marker in the consumer
// container. Its contents are uninteresting; its presence is the receipt
// that the cluster was formed before the consumer's first command ran.
const clusterBoundMarker = "/etc/valkey-cluster/bootstrap.done"

// Cluster is a slot-sharded Valkey Cluster: `shards` primaries splitting
// the 16384-slot keyspace between them, each with `replicasPerShard`
// replicas. Nodes holds every member — the first `shards` entries are the
// primaries, the remainder their replicas — in the order valkey-cli is
// handed them at bootstrap.
//
// Unlike Replication, the members are symmetric peers: each one gossips
// with every other over the cluster bus, and none of them is "already up"
// when its neighbours boot. That is what rules out node-to-node service
// bindings here — see startAll.
type Cluster struct {
	// +private
	Nodes []*Server
	// +private
	Bootstrap *dagger.Container // The exec that assigns the slots; see clusterBootstrapScript.
}

// Cluster describes a Valkey Cluster: `shards` primaries sharing the
// 16384 hash slots, each with `replicasPerShard` replicas.
//
// Image: `<registry>/valkey/valkey:<tag>` for every node — the topology
// is deliberately homogeneous.
//
// Rejected inputs (each a descriptive error rather than a half-formed
// cluster):
//
//   - `password == nil` — the same secret is every node's `requirepass`
//     and every replica's `masterauth`, so it is mandatory.
//   - `clientListenerSecurity == nil` — plaintext must be a deliberate
//     caller choice, exactly as for a single Server.
//   - a TLS or MTLS profile — see the note below.
//   - `shards < 3` — see minClusterShards.
//   - `replicasPerShard < 0` — nonsense rather than a topology.
//
// TLS/mTLS is not supported for this topology yet, for the reason
// Replication does not support it and one more besides: a TLS node runs
// with `--port 0`, so the cluster bus would have to run over TLS too
// (`--tls-cluster yes`), and each peer would need trust material a
// client-facing `*ServerSecurity` profile does not carry. On top of that
// the bootstrap runs through `valkey-cli --cluster create`, which would
// need its own `--tls` / `--cacert` material to reach the nodes at all. A
// TLS/mTLS profile is therefore rejected here rather than booting a
// cluster whose members spin on a failed handshake.
//
// Like Valkey.Server, this constructor starts nothing: it validates,
// builds every node, and composes the bootstrap exec, all lazily. Both
// entry points into a running cluster — Cluster.Client and
// Cluster.BindNodes — drive that bootstrap themselves, because a Dagger
// service is only reachable from whichever client started it. Starting
// the nodes here (from the valkey module's runtime) would register them
// in the valkey module's DNS domain, and a consumer container binding
// them from ANOTHER module then cannot resolve them at all:
//
//	lookup valkey-<host> for hosts file: ... no such host
//
// — the same trap Server.Endpoint documents, and a known Dagger
// limitation reported upstream. Until it is addressed, BindNodes has to
// be able to bring the cluster up itself.
//
// Session-cached for the same reason Valkey.Server and
// Valkey.Replication are: repeated chained calls on the returned cluster
// within one test must observe the SAME backing services — and the same
// bootstrap exec — and therefore the same keyspace. `name` folds into
// that cache key and into every node's hostname, so parallel test suites
// should pass a unique value per test.
//
// +cache="session"
func (v *Valkey) Cluster(
	ctx context.Context,
	// +default=""
	name string,
	// +default="docker.io"
	registry string,
	// +default="9.1"
	tag string,
	// +default=3
	shards int,
	// +default=0
	replicasPerShard int,
	password *dagger.Secret,
	clientListenerSecurity *ServerSecurity,
) (*Cluster, error) {
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
			"cluster does not support %s listeners yet: the cluster bus between peers would also have to run over TLS (--tls-cluster yes), and both the peers and the valkey-cli that bootstraps them need trust material that a client-listener ServerSecurity profile does not carry; pass PlaintextServerSecurity()",
			securityModeLabel(clientListenerSecurity.Mode),
		)
	}
	if shards < minClusterShards {
		return nil, fmt.Errorf(
			"shards must be at least %d, got %d: Valkey Cluster agrees slot ownership by a majority vote of the primaries, so fewer than %d can never form a quorum; use Valkey.Replication or Valkey.Server for a smaller topology",
			minClusterShards, shards, minClusterShards,
		)
	}
	if replicasPerShard < 0 {
		return nil, fmt.Errorf("replicasPerShard must not be negative, got %d", replicasPerShard)
	}

	image := valkeyImage(registry, tag)

	// Primaries first, then replicas: `valkey-cli --cluster create` takes
	// the first len(nodes)/(replicasPerShard+1) addresses as primaries and
	// distributes the remainder as their replicas, so this ordering is
	// what makes Nodes[:shards] the primaries.
	total := shards * (1 + replicasPerShard)
	nodes := make([]*Server, 0, total)
	for i := 0; i < total; i++ {
		nodeName := clusterNodeName(name, i)
		nodes = append(nodes, buildServer(
			nodeName,
			image,
			password,
			clientListenerSecurity,
			clusterArgs(serverHostname(nodeName)),
			// Deliberately no bindings between peers — see startAll.
			nil,
			[]int{clusterBusPort},
		))
	}

	cluster := &Cluster{Nodes: nodes}
	bootstrap, err := cluster.bootstrapContainer(image, password, replicasPerShard)
	if err != nil {
		return nil, err
	}
	cluster.Bootstrap = bootstrap
	return cluster, nil
}

// clusterNodeName derives the per-node `name` each node's hostname is
// hashed from. Folding the topology role and index into the name is what
// gives every member its own pinned hostname, keeps two Cluster calls
// with different `name`s from colliding, and keeps a member from
// colliding with a Valkey.Server or Valkey.Replication node booted under
// the same `name`.
func clusterNodeName(name string, index int) string {
	return name + "/cluster/node-" + strconv.Itoa(index)
}

// clusterArgs renders the `valkey-server` flags that turn a node into a
// cluster member advertising itself at host.
//
// Every node must advertise a routable identity of its own. Left to
// itself a node announces the address it believes it has, and inside a
// Dagger service that is not an address its peers (or a client following
// a MOVED redirect) can dial — so the three `--cluster-announce-*` flags
// pin it to the node's own WithHostname alias, its client port, and the
// bus port. A node that cannot self-identify ends up advertising
// localhost, at which point every peer dials itself and the cluster never
// forms.
//
// `--cluster-announce-ip` takes the hostname rather than an IP on
// purpose: the container IP is not known until the service starts (and
// changes between sessions), whereas the WithHostname alias is stable and
// resolvable from every peer, from the module runtime, and — via
// BindNodes — from a consumer container.
//
// `--masterauth` is what lets a replica authenticate to the primary it is
// assigned at bootstrap. It is the same secret as the node's own
// `requirepass`, expanded by the shell wrapper buildServer wraps these
// args in, so it never enters the node's argv in the Dagger graph. See
// replicaArgs for why `masterauth` and not `primaryauth`.
func clusterArgs(host string) []string {
	return []string{
		"--cluster-enabled", "yes",
		"--cluster-config-file", "nodes.conf",
		"--cluster-node-timeout", "5000",
		"--cluster-announce-ip", host,
		"--cluster-announce-port", strconv.Itoa(valkeyPort),
		"--cluster-announce-bus-port", strconv.Itoa(clusterBusPort),
		"--masterauth", `"$VALKEY_PASSWORD"`,
	}
}

// startAll boots every node service concurrently, so the module runtime
// can resolve them by hostname.
//
// Concurrency is not an optimisation here, it is a correctness
// requirement. Cluster members are symmetric peers: they gossip over the
// bus port and none of them is "already up" when its neighbours boot, so
// there is deliberately NO node-to-node WithServiceBinding. A binding
// would force Dagger to fully ready node B before it even boots node A,
// while B's own readiness depends on the cluster it needs A to help form
// — the same readiness deadlock the Redpanda multi-broker topology hit.
// Starting the nodes together instead lets them discover each other by
// hostname over session-wide DNS.
func startAll(ctx context.Context, nodes []*Server) error {
	var wg sync.WaitGroup
	errs := make([]error, len(nodes))
	for i, node := range nodes {
		if node == nil || node.Svc == nil {
			continue
		}
		wg.Add(1)
		go func(i int, node *Server) {
			defer wg.Done()
			if _, err := node.Svc.Start(ctx); err != nil {
				errs[i] = fmt.Errorf("start cluster node %d (%s): %w", i, node.Host, err)
			}
		}(i, node)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// bootstrapContainer composes (but does not run) the exec that assigns
// the 16384 hash slots across the primaries, wires each replica to one of
// them, and waits for every node to agree the cluster is up.
//
// `valkey-cli --cluster create` does the assignment rather than a Go
// reimplementation of CLUSTER MEET + ADDSLOTS + REPLICATE, because the
// slot split and the replica placement are exactly what it exists to get
// right. Its container binds every node — a fan-out from one consumer to
// N services, not the peer-to-peer cycle the nodes themselves must avoid.
//
// It is left lazy on purpose. Whoever forces it — Cluster.Client from the
// module runtime, or a consumer container's exec through BindNodes —
// drives the node services up as dependencies of their own request, in
// their own DNS domain. Bootstrapping eagerly in the constructor would
// pin the services to the valkey module's domain and make BindNodes
// unusable from any other module.
//
// The nonce env var is a cache buster. Nothing else in this exec's inputs
// changes between engine sessions (the image, the args, and the node
// hostnames are all derived from `name`), so a second session with the
// same `name` would replay the first session's cached exec and hand back
// a cluster that was never actually bootstrapped. Cluster is
// session-cached, so the nonce is minted once per session — the exec
// re-runs per session, not per call.
func (c *Cluster) bootstrapContainer(image string, password *dagger.Secret, replicasPerShard int) (*dagger.Container, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate bootstrap nonce: %w", err)
	}

	ctr := dag.Container().
		From(image).
		WithSecretVariable("VALKEY_PASSWORD", password).
		WithEnvVariable("VALKEY_CLUSTER_BOOTSTRAP_NONCE", hex.EncodeToString(nonce))
	for _, node := range c.Nodes {
		ctr = ctr.WithServiceBinding(node.Host, node.Svc)
	}
	script := clusterBootstrapScript(c.Endpoints(), len(c.Nodes), replicasPerShard)
	return ctr.WithExec([]string{"sh", "-c", script}), nil
}

// clusterBootstrapScript renders the shell program that brings a cluster
// up: wait for every node to answer, create the cluster, then wait for
// every node to agree it is formed.
//
// It waits in the container rather than in Go so that BindNodes — which
// returns a *dagger.Container and so has neither a context nor an error
// to report through — gets the same guarantee Cluster.Client does.
//
// Both waits matter. `valkey-cli --cluster create` aborts on the first
// node it cannot reach, and valkey-server opens its client port only once
// it has finished loading, so the pre-create wait is what stops the
// bootstrap racing a node that is still starting. The post-create wait
// covers the other end: slot ownership reaches the rest of the cluster by
// gossip, and a client seeded with a node that has not caught up yet gets
// a stale slot map and MOVED-loops. Every node is polled, not just the
// one valkey-cli was pointed at, and `cluster_known_nodes` is checked
// alongside `cluster_state:ok` — a node whose gossip never reached the
// others still reports state:ok for the slots it can see, and only the
// node count gives it away.
func clusterBootstrapScript(endpoints []string, nodeCount, replicasPerShard int) string {
	return fmt.Sprintf(`set -eu

NODES=%q
ATTEMPTS=%d

# probe HOST PORT COMMAND... — one command against one specific node.
probe() {
  h=$1; p=$2; shift 2
  valkey-cli --no-auth-warning -h "$h" -p "$p" -a "$VALKEY_PASSWORD" "$@" 2>/dev/null
}

# await NEEDLE WHAT ENDPOINT COMMAND... — poll one node until its reply
# contains NEEDLE, then fail loudly (with the last reply) if it never does.
await() {
  needle=$1; what=$2; ep=$3; shift 3
  h=${ep%%%%:*}; p=${ep##*:}
  i=0
  while [ "$i" -lt "$ATTEMPTS" ]; do
    if probe "$h" "$p" "$@" | grep -q "$needle"; then
      return 0
    fi
    i=$((i + 1))
    sleep 1
  done
  echo "timed out after ${ATTEMPTS}s waiting for $what on $ep; last reply:" >&2
  probe "$h" "$p" "$@" >&2 || true
  exit 1
}

for ep in $NODES; do
  await PONG "the node to accept authenticated commands" "$ep" PING
done

valkey-cli --no-auth-warning -a "$VALKEY_PASSWORD" \
  --cluster create $NODES \
  --cluster-replicas %d \
  --cluster-yes

for ep in $NODES; do
  await cluster_state:ok "every hash slot to be owned" "$ep" CLUSTER INFO
  await cluster_known_nodes:%d "every node to be known to this one" "$ep" CLUSTER INFO
done

echo ok > %s
`,
		strings.Join(endpoints, " "),
		clusterBootstrapAttempts,
		replicasPerShard,
		nodeCount,
		clusterBootstrapMarker,
	)
}

// Endpoints returns every member's `host:6379` address, primaries first.
// These are the addresses a cluster-aware client seeds from, and the
// hostnames BindNodes makes reachable. Like Server.Endpoint it is a pure
// accessor and starts nothing.
//
// Session-cached rather than never-cached: Dagger v0.21 detaches module
// objects returned from a `+cache="never"` function when a consumer
// module reads their fields lazily, and `tests/` is such a consumer.
//
// +cache="session"
func (c *Cluster) Endpoints() []string {
	out := make([]string, 0, len(c.Nodes))
	for _, node := range c.Nodes {
		out = append(out, node.Endpoint())
	}
	return out
}

// BindNodes attaches every member service to the given container under
// the hostname it advertises, so the container can dial any of them at
// the addresses Endpoints reports — and, just as importantly, can follow
// a MOVED redirect to any other member, since a cluster client is told to
// go to a node's *advertised* hostname rather than the one it dialed.
// Binding only the seed node would leave every redirect unresolvable.
//
// The returned container also carries the bootstrap script's completion
// marker. That file is never read; grafting it is what makes the
// bootstrap a build-time dependency of whatever the consumer runs next,
// so the container's first command meets a cluster whose slots are
// already assigned rather than one answering CLUSTERDOWN.
//
// +cache="never"
func (c *Cluster) BindNodes(ctr *dagger.Container) *dagger.Container {
	ctr = ctr.WithFile(clusterBoundMarker, c.Bootstrap.File(clusterBootstrapMarker))
	for _, node := range c.Nodes {
		ctr = ctr.WithServiceBinding(node.Host, node.Svc)
	}
	return ctr
}

// Client brings the cluster up and returns a cluster-aware valkey-go
// Client seeded with every member's endpoint. valkey-go detects cluster
// mode from the seed addresses (CLUSTER SLOTS), keeps its own slot map,
// and follows MOVED/ASK redirects itself, so callers address the cluster
// as one keyspace.
//
// The supplied ClientSecurity mode must match the cluster's listener
// mode, which is PLAINTEXT for now; a mismatch returns an error naming
// both modes rather than failing opaquely at the wire.
//
// The nodes are started explicitly (and concurrently — see startAll)
// rather than left to the bootstrap exec's service bindings, because a
// binding only wires the service into that one container's hosts file:
// the returned valkey-go client dials from the module runtime, which
// needs the hostnames in session DNS. Starting them here is also what
// makes BindNodes unusable on the same cluster afterwards — see
// Valkey.Cluster — so a consumer container should bind a cluster this
// module has not already dialled.
//
// +cache="never"
func (c *Cluster) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	if len(c.Nodes) == 0 {
		return nil, fmt.Errorf("cluster has no nodes")
	}
	if err := c.Nodes[0].requireMode(security); err != nil {
		return nil, err
	}
	if err := startAll(ctx, c.Nodes); err != nil {
		return nil, err
	}
	if _, err := c.Bootstrap.Sync(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap valkey cluster: %w", err)
	}

	seed := c.Nodes[0]
	client := clientFrom(seed.Host, valkeyPort, seed.UserName, seed.Pass, 0, security)
	client.Addrs = c.Endpoints()
	client.ClusterMode = true
	return client, nil
}

// Stop tears down every member. Every node is attempted even if an
// earlier one fails, and the failures are joined — a partial teardown
// that reported only the first error would leave services running with
// nothing naming them.
//
// +cache="never"
func (c *Cluster) Stop(ctx context.Context) error {
	var errs []error
	for i := len(c.Nodes) - 1; i >= 0; i-- {
		if err := c.Nodes[i].Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("node %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}
