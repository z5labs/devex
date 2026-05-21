// Tests for the dgraph daggerverse module. Each test is exposed as a
// standalone dagger function so it can be invoked individually during
// TDD; All wires them up for parallel execution under
// `dagger call all`.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

type Tests struct{}

// All runs every dgraph test. Default parallelism is 1 (serial)
// because the +cache="session" generator on Dgraph.Cluster collapses
// every same-shape freshCluster call to a single backing service —
// running tests in parallel causes them to read the same graph state.
// Pass --parallel=N to opt back in for tests that don't share a shape
// (e.g. running just multi-alpha-* alongside validation tests).
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=1
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("defaults-produce-working-single-node-cluster", func(ctx context.Context) error {
		return t.DefaultsProduceWorkingSingleNodeCluster(ctx)
	})
	jobs = jobs.WithJob("grpc-endpoints-should-not-be-cached", func(ctx context.Context) error {
		return t.GrpcEndpointsShouldNotBeCached(ctx)
	})
	jobs = jobs.WithJob("http-endpoints-should-not-be-cached", func(ctx context.Context) error {
		return t.HttpEndpointsShouldNotBeCached(ctx)
	})
	jobs = jobs.WithJob("mutate-should-not-be-cached", func(ctx context.Context) error {
		return t.MutateShouldNotBeCached(ctx)
	})
	jobs = jobs.WithJob("cluster-rejects-multiple-zeros", func(ctx context.Context) error {
		return t.ClusterRejectsMultipleZeros(ctx)
	})
	jobs = jobs.WithJob("cluster-rejects-invalid-alphas-replicas-ratio", func(ctx context.Context) error {
		return t.ClusterRejectsInvalidAlphasReplicasRatio(ctx)
	})
	jobs = jobs.WithJob("cluster-rejects-even-replicas", func(ctx context.Context) error {
		return t.ClusterRejectsEvenReplicas(ctx)
	})
	jobs = jobs.WithJob("cluster-rejects-nil-security", func(ctx context.Context) error {
		return t.ClusterRejectsNilSecurity(ctx)
	})
	jobs = jobs.WithJob("multi-alpha-single-group-all-reachable", func(ctx context.Context) error {
		return t.MultiAlphaSingleGroupAllReachable(ctx)
	})
	jobs = jobs.WithJob("multi-alpha-sharded-topology", func(ctx context.Context) error {
		return t.MultiAlphaShardedTopology(ctx)
	})
	jobs = jobs.WithJob("client-alter-schema-round-trip", func(ctx context.Context) error {
		return t.ClientAlterSchemaRoundTrip(ctx)
	})
	jobs = jobs.WithJob("client-mutate-then-query-round-trip", func(ctx context.Context) error {
		return t.ClientMutateThenQueryRoundTrip(ctx)
	})
	jobs = jobs.WithJob("client-mutate-without-commit-does-not-persist", func(ctx context.Context) error {
		return t.ClientMutateWithoutCommitDoesNotPersist(ctx)
	})
	jobs = jobs.WithJob("client-query-with-vars-round-trip", func(ctx context.Context) error {
		return t.ClientQueryWithVarsRoundTrip(ctx)
	})
	jobs = jobs.WithJob("remote-client-can-target-existing-cluster", func(ctx context.Context) error {
		return t.RemoteClientCanTargetExistingCluster(ctx)
	})

	return jobs.Run(ctx)
}

// freshCluster mints a Dgraph cluster sized as requested with a plaintext
// listener. We deliberately do NOT defer Stop: Dgraph.Cluster is
// +cache="session", so every freshCluster(a, r) call with the same shape
// returns the SAME backing cluster, and the engine tears it down when
// the session ends. Stopping mid-test would kill that shared cluster
// for any peer test still running against it. The cache-directive tests
// below intentionally violate this and Stop explicitly — they accept
// the resulting re-bootstrap cost in exchange for proving start() ran.
func freshCluster(_ context.Context, alphas, replicas int) *dagger.DgraphCluster {
	return dag.Dgraph().Cluster(
		dag.Dgraph().PlaintextServerSecurity(),
		dagger.DgraphClusterOpts{
			Alphas:   alphas,
			Replicas: replicas,
		},
	)
}

// randName returns a short hex-suffixed identifier suitable for use as
// a DQL predicate name, schema-indexed value, blank-node name, or query
// fixture. Callers can pass any prefix (including ones with `_`); the
// result is not constrained to DNS-safe characters.
func randName(ctx context.Context, prefix string) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return prefix + h[:12], nil
}

// -----------------------------------------------------------------------------
// Validation tests — no service plumbing needed.
// -----------------------------------------------------------------------------

// ClusterRejectsMultipleZeros verifies zeros != 1 surfaces a descriptive error.
//
// +cache="never"
func (t *Tests) ClusterRejectsMultipleZeros(ctx context.Context) error {
	cluster := dag.Dgraph().Cluster(
		dag.Dgraph().PlaintextServerSecurity(),
		dagger.DgraphClusterOpts{Zeros: 3},
	)
	_, err := cluster.GrpcEndpoints(ctx)
	if err == nil {
		return fmt.Errorf("expected Cluster(zeros=3) to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "single-Zero") && !strings.Contains(err.Error(), "zeros=") {
		return fmt.Errorf("expected error to mention single-Zero/zeros, got: %v", err)
	}
	return nil
}

// ClusterRejectsInvalidAlphasReplicasRatio verifies alphas % replicas != 0 is rejected.
//
// +cache="never"
func (t *Tests) ClusterRejectsInvalidAlphasReplicasRatio(ctx context.Context) error {
	cluster := dag.Dgraph().Cluster(
		dag.Dgraph().PlaintextServerSecurity(),
		dagger.DgraphClusterOpts{Alphas: 4, Replicas: 3},
	)
	_, err := cluster.GrpcEndpoints(ctx)
	if err == nil {
		return fmt.Errorf("expected Cluster(alphas=4, replicas=3) to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "multiple of replicas") {
		return fmt.Errorf("expected error to mention 'multiple of replicas', got: %v", err)
	}
	return nil
}

// ClusterRejectsEvenReplicas verifies that an even replicas value > 1
// is rejected — Dgraph's Raft consensus needs an odd replica count per
// group (or replicas=1 for no replication). Alphas=2 keeps
// alphas%replicas==0 so only the odd-replicas rule can trip.
//
// +cache="never"
func (t *Tests) ClusterRejectsEvenReplicas(ctx context.Context) error {
	cluster := dag.Dgraph().Cluster(
		dag.Dgraph().PlaintextServerSecurity(),
		dagger.DgraphClusterOpts{Alphas: 2, Replicas: 2},
	)
	_, err := cluster.GrpcEndpoints(ctx)
	if err == nil {
		return fmt.Errorf("expected Cluster(replicas=2) to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "must be odd") {
		return fmt.Errorf("expected error to mention 'must be odd', got: %v", err)
	}
	return nil
}

// ClusterRejectsNilSecurity verifies that a nil clientListenerSecurity is
// rejected. The Dagger SDK's binding panics via assertNotNil before the
// call leaves the test module; recover and assert the panic mentions
// the rejected argument.
//
// +cache="never"
func (t *Tests) ClusterRejectsNilSecurity(ctx context.Context) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			returnErr = fmt.Errorf("expected Cluster(nil) to panic via assertNotNil, but it did not")
			return
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "clientListenerSecurity") && !strings.Contains(msg, "security") {
			returnErr = fmt.Errorf("expected panic to mention clientListenerSecurity, got: %v", r)
		}
	}()
	cluster := dag.Dgraph().Cluster(nil)
	_, _ = cluster.GrpcEndpoints(ctx)
	return nil
}

// -----------------------------------------------------------------------------
// Cache-directive tests — verify +cache="never" propagation.
// -----------------------------------------------------------------------------

// GrpcEndpointsShouldNotBeCached verifies that GrpcEndpoints re-executes
// on every call rather than returning a cached snapshot. We boot the
// cluster, fetch endpoints (starts services), Stop the cluster (kills
// services), and call GrpcEndpoints again — the second call must
// re-start the services. A bare length check can't distinguish that
// from cached strings, so we follow the second call with a real
// AlterSchema against the cluster: if start() didn't run because
// GrpcEndpoints returned a cached result, the alphas remain dead and
// the alter dials a hung port.
//
// +cache="never"
func (t *Tests) GrpcEndpointsShouldNotBeCached(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	eps1, err := cluster.GrpcEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("endpoints 1: %w", err)
	}
	if len(eps1) == 0 {
		return fmt.Errorf("empty endpoint list from first call")
	}
	if err := cluster.Stop(ctx); err != nil {
		return fmt.Errorf("stop cluster: %w", err)
	}
	eps2, err := cluster.GrpcEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("endpoints 2 after restart: %w", err)
	}
	if len(eps2) != len(eps1) {
		return fmt.Errorf("expected same endpoint count after restart (1=%v, 2=%v)", eps1, eps2)
	}
	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	if err := client.AlterSchema(ctx, "name: string ."); err != nil {
		return fmt.Errorf("alter after restart (GrpcEndpoints likely cached, services never re-started): %w", err)
	}
	return nil
}

// HttpEndpointsShouldNotBeCached: same restart-after-stop check as Grpc
// but for the HTTP listener. The liveness probe is the same gRPC-based
// AlterSchema — both endpoint methods share start(), so any restart
// proves either +cache="never" directive fired.
//
// +cache="never"
func (t *Tests) HttpEndpointsShouldNotBeCached(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	eps1, err := cluster.HTTPEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("endpoints 1: %w", err)
	}
	if len(eps1) == 0 {
		return fmt.Errorf("empty endpoint list from first call")
	}
	if err := cluster.Stop(ctx); err != nil {
		return fmt.Errorf("stop cluster: %w", err)
	}
	eps2, err := cluster.HTTPEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("endpoints 2 after restart: %w", err)
	}
	if len(eps2) != len(eps1) {
		return fmt.Errorf("expected same endpoint count after restart (1=%v, 2=%v)", eps1, eps2)
	}
	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	if err := client.AlterSchema(ctx, "name: string ."); err != nil {
		return fmt.Errorf("alter after restart (HTTPEndpoints likely cached, services never re-started): %w", err)
	}
	return nil
}

// MutateShouldNotBeCached calls Mutate twice with the same payload on
// the same cluster and verifies each call assigns a fresh UID. If the
// engine cached the call, both would return identical UID JSON. The
// payload value is randomised per-run so re-running the suite never
// reuses a probe name across engine sessions.
//
// +cache="never"
func (t *Tests) MutateShouldNotBeCached(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	if err := client.AlterSchema(ctx, "name: string @index(exact) ."); err != nil {
		return fmt.Errorf("alter schema: %w", err)
	}

	probe, err := randName(ctx, "probe_")
	if err != nil {
		return err
	}
	payload := fmt.Sprintf(`{"name":%q}`, probe)
	uids1, err := client.Mutate(ctx, payload, true)
	if err != nil {
		return fmt.Errorf("mutate 1: %w", err)
	}
	uids2, err := client.Mutate(ctx, payload, true)
	if err != nil {
		return fmt.Errorf("mutate 2: %w", err)
	}
	if uids1 == uids2 {
		return fmt.Errorf("expected distinct UIDs across Mutate calls, both returned %q", uids1)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Cluster topology / binding tests
// -----------------------------------------------------------------------------

// DefaultsProduceWorkingSingleNodeCluster boots a 1-Zero, 1-Alpha,
// replicas=1 cluster (the constructor defaults) and runs a schema
// alteration against it to prove it's serving requests.
//
// +cache="never"
func (t *Tests) DefaultsProduceWorkingSingleNodeCluster(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	if err := client.AlterSchema(ctx, "name: string ."); err != nil {
		return fmt.Errorf("alter schema on defaults cluster: %w", err)
	}
	return nil
}

// MultiAlphaSingleGroupAllReachable boots a 3-Alpha cluster at
// replicas=3 (single group of three Alphas, fully replicated) and
// verifies every endpoint serves queries.
//
// +cache="never"
func (t *Tests) MultiAlphaSingleGroupAllReachable(ctx context.Context) error {
	cluster := freshCluster(ctx, 3, 3)
	eps, err := cluster.GrpcEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("get endpoints: %w", err)
	}
	if len(eps) != 3 {
		return fmt.Errorf("expected 3 grpc endpoints, got %d (%v)", len(eps), eps)
	}

	security := dag.Dgraph().PlaintextClientSecurity()
	for i, ep := range eps {
		c := dag.Dgraph().Client([]string{ep}, security)
		if err := c.AlterSchema(ctx, "name: string ."); err != nil {
			return fmt.Errorf("alter via endpoint %d (%s): %w", i, ep, err)
		}
	}
	return nil
}

// MultiAlphaShardedTopology boots a 2-Alpha cluster at replicas=1 (two
// groups, one Alpha each — sharded, no replication) and verifies the
// cluster serves a trivial schema alteration. Dgraph's Raft consensus
// requires replicas to be odd, so the smallest valid sharded topology
// is 2 Alphas at replicas=1.
//
// +cache="never"
func (t *Tests) MultiAlphaShardedTopology(ctx context.Context) error {
	cluster := freshCluster(ctx, 2, 1)
	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	if err := client.AlterSchema(ctx, "name: string ."); err != nil {
		return fmt.Errorf("alter schema on sharded cluster: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Client round-trip tests
// -----------------------------------------------------------------------------

// ClientAlterSchemaRoundTrip verifies AlterSchema accepts a non-trivial
// DQL schema and the cluster reports it on subsequent schema queries.
//
// +cache="never"
func (t *Tests) ClientAlterSchemaRoundTrip(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	pred, err := randName(ctx, "pred_")
	if err != nil {
		return err
	}

	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	schema := fmt.Sprintf("%s: string @index(exact) .", pred)
	if err := client.AlterSchema(ctx, schema); err != nil {
		return fmt.Errorf("alter: %w", err)
	}

	resp, err := client.RunQuery(ctx, "schema {}")
	if err != nil {
		return fmt.Errorf("query schema: %w", err)
	}
	if !strings.Contains(resp, pred) {
		return fmt.Errorf("expected schema response to mention predicate %q, got: %s", pred, resp)
	}
	return nil
}

// ClientMutateThenQueryRoundTrip applies a schema, sets a triple with
// a random value, and verifies the value reads back via Query.
//
// +cache="never"
func (t *Tests) ClientMutateThenQueryRoundTrip(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	val, err := randName(ctx, "v")
	if err != nil {
		return err
	}

	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	if err := client.AlterSchema(ctx, "name: string @index(exact) ."); err != nil {
		return fmt.Errorf("alter: %w", err)
	}

	payload := fmt.Sprintf(`{"name":%q}`, val)
	uidsJson, err := client.Mutate(ctx, payload, true)
	if err != nil {
		return fmt.Errorf("mutate: %w", err)
	}
	uids := map[string]string{}
	if err := json.Unmarshal([]byte(uidsJson), &uids); err != nil {
		return fmt.Errorf("parse uids %q: %w", uidsJson, err)
	}
	if len(uids) == 0 {
		return fmt.Errorf("expected at least one assigned UID, got %q", uidsJson)
	}

	dql := fmt.Sprintf(`{ q(func: eq(name, %q)) { uid name } }`, val)
	resp, err := client.RunQuery(ctx, dql)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	if !strings.Contains(resp, val) {
		return fmt.Errorf("expected query response to contain %q, got: %s", val, resp)
	}
	return nil
}

// ClientMutateWithoutCommitDoesNotPersist mutates with commit=false
// and verifies a subsequent Query does NOT see the value.
//
// +cache="never"
func (t *Tests) ClientMutateWithoutCommitDoesNotPersist(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	val, err := randName(ctx, "dry")
	if err != nil {
		return err
	}

	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	if err := client.AlterSchema(ctx, "name: string @index(exact) ."); err != nil {
		return fmt.Errorf("alter: %w", err)
	}

	payload := fmt.Sprintf(`{"name":%q}`, val)
	if _, err := client.Mutate(ctx, payload, false); err != nil {
		return fmt.Errorf("dry-run mutate: %w", err)
	}

	dql := fmt.Sprintf(`{ q(func: eq(name, %q)) { uid name } }`, val)
	resp, err := client.RunQuery(ctx, dql)
	if err != nil {
		return fmt.Errorf("query after dry-run: %w", err)
	}
	if strings.Contains(resp, val) {
		return fmt.Errorf("expected query to NOT see dry-run value %q, got: %s", val, resp)
	}
	return nil
}

// ClientQueryWithVarsRoundTrip exercises the variable-substitution path
// of QueryWithVars, which crosses the Dagger boundary as a JSON-encoded
// map.
//
// +cache="never"
func (t *Tests) ClientQueryWithVarsRoundTrip(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	val, err := randName(ctx, "v")
	if err != nil {
		return err
	}

	client := cluster.Client(dag.Dgraph().PlaintextClientSecurity())
	if err := client.AlterSchema(ctx, "name: string @index(exact) ."); err != nil {
		return fmt.Errorf("alter: %w", err)
	}
	if _, err := client.Mutate(ctx, fmt.Sprintf(`{"name":%q}`, val), true); err != nil {
		return fmt.Errorf("mutate: %w", err)
	}

	dql := `query Q($v: string) { q(func: eq(name, $v)) { uid name } }`
	varsJson, err := json.Marshal(map[string]string{"$v": val})
	if err != nil {
		return err
	}
	resp, err := client.QueryWithVars(ctx, dql, string(varsJson))
	if err != nil {
		return fmt.Errorf("query with vars: %w", err)
	}
	if !strings.Contains(resp, val) {
		return fmt.Errorf("expected vars query response to contain %q, got: %s", val, resp)
	}
	return nil
}

// RemoteClientCanTargetExistingCluster builds a cluster locally, then
// constructs a top-level Dgraph.Client (not Cluster.Client) against
// the cluster's endpoints — proving the constructor works against any
// reachable address list.
//
// +cache="never"
func (t *Tests) RemoteClientCanTargetExistingCluster(ctx context.Context) error {
	cluster := freshCluster(ctx, 1, 1)
	eps, err := cluster.GrpcEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("get endpoints: %w", err)
	}

	remote := dag.Dgraph().Client(eps, dag.Dgraph().PlaintextClientSecurity())
	if err := remote.AlterSchema(ctx, "name: string ."); err != nil {
		return fmt.Errorf("remote alter: %w", err)
	}
	return nil
}
