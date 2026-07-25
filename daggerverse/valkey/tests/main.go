// Tests for the valkey daggerverse module. Each test is exposed as a
// standalone dagger function so it can be invoked individually during
// TDD; All wires them up for parallel execution under
// `dagger call all`.
//
// Every password, server name, and key prefix is minted at runtime via
// dag.Random().Sha256. The ACL user deliberately uses the valkey
// module's default ("default"), which a few tests assert against.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

type Tests struct{}

// All runs every valkey test as a convenience for local `dagger call
// all` invocations. CI does NOT call All: each of the sub-aggregators
// below is registered as its own check, so GH Actions schedules each
// onto its own runner in parallel — running All on top would
// double-bill the same work.
//
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("Validation", func(ctx context.Context) error {
		return t.Validation(ctx, parallel)
	})
	jobs = jobs.WithJob("Server", func(ctx context.Context) error {
		return t.Server(ctx, parallel)
	})
	jobs = jobs.WithJob("Client", func(ctx context.Context) error {
		return t.Client(ctx, parallel)
	})
	jobs = jobs.WithJob("Security", func(ctx context.Context) error {
		return t.Security(ctx, parallel)
	})
	jobs = jobs.WithJob("Replication", func(ctx context.Context) error {
		return t.Replication(ctx, parallel)
	})
	jobs = jobs.WithJob("Cluster", func(ctx context.Context) error {
		return t.Cluster(ctx, parallel)
	})
	return jobs.Run(ctx)
}

// Validation runs the input-rejection tests. These boot no service, so
// they're safe to fan out unbounded.
//
// +check
// +cache="session"
func (t *Tests) Validation(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("server-rejects-nil-password", t.ServerRejectsNilPassword)
	jobs = jobs.WithJob("server-rejects-nil-security", t.ServerRejectsNilSecurity)
	jobs = jobs.WithJob("replication-rejects-too-few-replicas", t.ReplicationRejectsTooFewReplicas)
	jobs = jobs.WithJob("replication-rejects-tls-security", t.ReplicationRejectsTlsSecurity)
	jobs = jobs.WithJob("cluster-rejects-too-few-shards", t.ClusterRejectsTooFewShards)
	jobs = jobs.WithJob("cluster-rejects-tls-security", t.ClusterRejectsTlsSecurity)
	return jobs.Run(ctx)
}

// Replication runs the primary/replica topology tests. Each test boots
// its own topology via bootReplication, whose runtime-random name folds
// into Valkey.Replication's session-cache key and into every node's
// hostname, so concurrent tests get independent topologies.
//
// +check
// +cache="session"
func (t *Tests) Replication(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("replication-propagates-writes-to-replica", t.ReplicationPropagatesWritesToReplica)
	jobs = jobs.WithJob("replication-reports-roles", t.ReplicationReportsRoles)
	jobs = jobs.WithJob("replication-replica-rejects-writes", t.ReplicationReplicaRejectsWrites)
	jobs = jobs.WithJob("replication-link-authenticates", t.ReplicationLinkAuthenticates)
	jobs = jobs.WithJob("replication-nodes-have-distinct-hostnames", t.ReplicationNodesHaveDistinctHostnames)
	jobs = jobs.WithJob("replication-stop-terminates-every-node", t.ReplicationStopTerminatesEveryNode)
	return jobs.Run(ctx)
}

// Cluster runs the slot-sharded Valkey Cluster tests. Each test boots its
// own cluster via bootCluster, whose runtime-random name folds into
// Valkey.Cluster's session-cache key and into every node's hostname, so
// concurrent tests get independent clusters.
//
// These are the most expensive tests in the suite — the smallest legal
// cluster is three nodes, and every one of them has to boot before the
// bootstrap can even start — so they get their own aggregator rather than
// riding along with the single-node groups.
//
// +check
// +cache="session"
func (t *Tests) Cluster(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("cluster-reports-formed-state", t.ClusterReportsFormedState)
	jobs = jobs.WithJob("cluster-advertises-pinned-hostnames", t.ClusterAdvertisesPinnedHostnames)
	jobs = jobs.WithJob("cluster-round-trips-keys-across-slots", t.ClusterRoundTripsKeysAcrossSlots)
	jobs = jobs.WithJob("cluster-keys-scans-every-shard", t.ClusterKeysScansEveryShard)
	jobs = jobs.WithJob("cluster-del-spans-multiple-slots", t.ClusterDelSpansMultipleSlots)
	jobs = jobs.WithJob("cluster-bind-nodes-reachable-from-consumer", t.ClusterBindNodesReachableFromConsumer)
	jobs = jobs.WithJob("cluster-stop-terminates-every-node", t.ClusterStopTerminatesEveryNode)
	return jobs.Run(ctx)
}

// Server runs the topology, default, and caching tests. Each test boots
// its own node via bootServer, whose runtime-random name folds into
// Valkey.Server's session-cache key so concurrent tests boot independent
// backing services and never share a keyspace.
//
// +check
// +cache="session"
func (t *Tests) Server(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("defaults-produce-healthy-server", t.DefaultsProduceHealthyServer)
	jobs = jobs.WithJob("endpoint-should-not-be-cached", t.EndpointShouldNotBeCached)
	jobs = jobs.WithJob("same-name-server-shares-state", t.SameNameServerSharesState)
	jobs = jobs.WithJob("bind-server-reachable-from-alpine", t.BindServerReachableFromAlpine)
	jobs = jobs.WithJob("password-reusable-via-client", t.PasswordReusableViaClient)
	jobs = jobs.WithJob("stop-terminates-server", t.StopTerminatesServer)
	return jobs.Run(ctx)
}

// Client runs the command round-trip tests. Each test boots its own node
// via bootServer, so the keyspaces stay disjoint and the group fans out
// safely.
//
// +check
// +cache="session"
func (t *Tests) Client(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("set-get-round-trip", t.SetGetRoundTrip)
	jobs = jobs.WithJob("get-should-not-be-cached", t.GetShouldNotBeCached)
	jobs = jobs.WithJob("get-missing-key-fails", t.GetMissingKeyFails)
	jobs = jobs.WithJob("set-with-ttl-expires", t.SetWithTtlExpires)
	jobs = jobs.WithJob("del-removes-keys", t.DelRemovesKeys)
	jobs = jobs.WithJob("keys-scans-pattern", t.KeysScansPattern)
	jobs = jobs.WithJob("do-returns-json-replies", t.DoReturnsJsonReplies)
	jobs = jobs.WithJob("apply-file-seeds-data", t.ApplyFileSeedsData)
	jobs = jobs.WithJob("apply-file-reports-failing-command", t.ApplyFileReportsFailingCommand)
	jobs = jobs.WithJob("info-reports-role", t.InfoReportsRole)
	jobs = jobs.WithJob("flush-all-clears-keys", t.FlushAllClearsKeys)
	jobs = jobs.WithJob("client-ping-wrong-password-fails", t.ClientPingWrongPasswordFails)
	jobs = jobs.WithJob("db-selects-logical-database", t.DbSelectsLogicalDatabase)
	return jobs.Run(ctx)
}

// Security runs the TLS / mTLS listener + client tests. Each test mints
// its own CA, leaf certs, password, and server name at runtime (no
// literal credentials or PEM blobs), and folds a unique name into the
// node's session-cache key, so the tests fan out without sharing state.
//
// +check
// +cache="session"
func (t *Tests) Security(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("server-tls-round-trip-from-client", t.ServerTlsRoundTripFromClient)
	jobs = jobs.WithJob("server-mtls-round-trip-from-client", t.ServerMtlsRoundTripFromClient)
	jobs = jobs.WithJob("tls-server-rejects-plaintext-client", t.TlsServerRejectsPlaintextClient)
	jobs = jobs.WithJob("mtls-server-rejects-tls-only-client", t.MtlsServerRejectsTlsOnlyClient)
	jobs = jobs.WithJob("mtls-node-demands-client-cert-at-wire", t.MtlsNodeDemandsClientCertAtWire)
	jobs = jobs.WithJob("plaintext-dial-against-tls-node-fails", t.PlaintextDialAgainstTlsNodeFails)
	jobs = jobs.WithJob("tls-server-rejects-empty-name", t.TlsServerRejectsEmptyName)
	jobs = jobs.WithJob("bind-server-reachable-under-tls", t.BindServerReachableUnderTls)
	return jobs.Run(ctx)
}

// -----------------------------------------------------------------------------
// Helpers — all identifiers minted at runtime, no literals.
// -----------------------------------------------------------------------------

// randHex returns a fresh 12-hex-char value via the random module.
func randHex(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return h[:12], nil
}

// randSecret mints a random password and wraps it in a uniquely-named
// *dagger.Secret. The plaintext is a full SHA-256 hash; the secret name
// carries a random suffix so concurrent SetSecret calls don't collide.
func randSecret(ctx context.Context) (*dagger.Secret, error) {
	full, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 32})
	if err != nil {
		return nil, err
	}
	return dag.SetSecret("valkey-pw-"+full[:12], full), nil
}

// bootServer mints a fresh single-node Valkey server and returns it
// together with the password secret it was provisioned with. The server
// name is a runtime-random value (no literals) that folds into
// Valkey.Server's +cache="session" key, so concurrent tests boot
// independent backing services and never share a keyspace; a single test
// mints one name and reuses the returned handle, so its chained
// Client.Set → Client.Get calls stay cache-coherent. We deliberately do
// NOT defer Stop: the tests that care about teardown Stop their own
// node as part of the invariant; everyone else lets the session teardown
// handle it.
func bootServer(ctx context.Context) (*dagger.ValkeyServer, *dagger.Secret, error) {
	name, err := randHex(ctx)
	if err != nil {
		return nil, nil, err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return nil, nil, err
	}
	server := dag.Valkey().Server(
		pass,
		dag.Valkey().PlaintextServerSecurity(),
		dagger.ValkeyServerOpts{Name: name},
	)
	return server, pass, nil
}

// plaintextClient opens a plaintext client against a booted node.
func plaintextClient(server *dagger.ValkeyServer) *dagger.ValkeyClient {
	return server.Client(dag.Valkey().PlaintextClientSecurity())
}

// -----------------------------------------------------------------------------
// Validation tests — exercise the input rejections reachable through the
// generated SDK binding. nil required args are rejected by the binding's
// assertNotNil (it panics before the call leaves the test module), so we
// recover and assert the panic names the offending argument.
// -----------------------------------------------------------------------------

// ServerRejectsNilPassword verifies a passwordless node must not boot.
//
// +cache="never"
func (t *Tests) ServerRejectsNilPassword(ctx context.Context) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			returnErr = fmt.Errorf("expected Server(nil password) to panic via assertNotNil, but it did not")
			return
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "password") {
			returnErr = fmt.Errorf("expected panic to mention password, got: %v", r)
		}
	}()
	server := dag.Valkey().Server(nil, dag.Valkey().PlaintextServerSecurity())
	_, _ = server.Endpoint(ctx)
	return nil
}

// ServerRejectsNilSecurity verifies plaintext must be a deliberate
// choice, so the TLS follow-up stays an explicit upgrade rather than a
// silent default.
//
// +cache="never"
func (t *Tests) ServerRejectsNilSecurity(ctx context.Context) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			returnErr = fmt.Errorf("expected Server(nil security) to panic via assertNotNil, but it did not")
			return
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "clientListenerSecurity") && !strings.Contains(msg, "security") {
			returnErr = fmt.Errorf("expected panic to mention clientListenerSecurity, got: %v", r)
		}
	}()
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	server := dag.Valkey().Server(pass, nil)
	_, _ = server.Endpoint(ctx)
	return nil
}

// ReplicationRejectsTooFewReplicas verifies a replica-less "replication"
// topology is refused rather than silently degrading to a single node —
// Valkey.Server is what builds that, and a caller who asked for
// replication and got none would find out only when their read-replica
// assertions started passing against the primary.
//
// The cases are negative rather than 0: the generated Go binding drops
// any optional argument holding its zero value, so `Replicas: 0` never
// reaches the module and resolves to the `+default=1`. `--replicas=0` on
// the CLI does reach the guard, which is why the guard is `< 1` and not
// `< 0`.
//
// +cache="never"
func (t *Tests) ReplicationRejectsTooFewReplicas(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	for _, replicas := range []int{-1, -2} {
		rep := dag.Valkey().Replication(
			pass,
			dag.Valkey().PlaintextServerSecurity(),
			dagger.ValkeyReplicationOpts{Name: name, Replicas: replicas},
		)
		_, err := rep.Primary().Endpoint(ctx)
		if err == nil {
			return fmt.Errorf("expected Replication(replicas=%d) to be rejected, but it built a topology", replicas)
		}
		if !strings.Contains(err.Error(), "replicas") {
			return fmt.Errorf("expected the replicas=%d rejection to name the replicas argument, got: %v", replicas, err)
		}
	}
	return nil
}

// ReplicationRejectsTlsSecurity verifies a TLS listener profile is
// refused with an explanation rather than booting replicas that spin
// forever on a failed handshake: a TLS node runs with `--port 0`, so the
// replication link would also have to run over TLS, and a client-listener
// ServerSecurity carries none of the trust material that link needs.
//
// +cache="never"
func (t *Tests) ReplicationRejectsTlsSecurity(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-rep-tls")
	if err != nil {
		return err
	}
	// SAN value is irrelevant: the guard rejects before any node boots.
	cert, key, err := issueServerCert(ctx, ca, "valkey-placeholder", "vk-rep-tls-server")
	if err != nil {
		return err
	}
	rep := dag.Valkey().Replication(
		pass,
		dag.Valkey().TLSServerSecurity(cert, key),
		dagger.ValkeyReplicationOpts{Name: name},
	)
	_, err = rep.Primary().Endpoint(ctx)
	if err == nil {
		return fmt.Errorf("expected a TLS Replication topology to be rejected")
	}
	if !strings.Contains(err.Error(), "TLS") {
		return fmt.Errorf("expected the rejection to name TLS, got: %v", err)
	}
	return nil
}

// ClusterRejectsTooFewShards verifies a sub-quorum cluster is refused
// with an explanation rather than booting. Valkey Cluster agrees slot
// ownership by a majority vote of the primaries, so two primaries can
// never form a quorum once one is unreachable and one primary is a
// standalone node wearing a cluster hat — either would boot into a
// topology that looks healthy right up until it has to agree on
// something.
//
// The guard is checked before anything starts, so this test costs no
// containers.
//
// +cache="never"
func (t *Tests) ClusterRejectsTooFewShards(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	for _, shards := range []int{1, 2} {
		cluster := dag.Valkey().Cluster(
			pass,
			dag.Valkey().PlaintextServerSecurity(),
			dagger.ValkeyClusterOpts{Name: name, Shards: shards},
		)
		_, err := cluster.Endpoints(ctx)
		if err == nil {
			return fmt.Errorf("expected Cluster(shards=%d) to be rejected, but it built a topology", shards)
		}
		if !strings.Contains(err.Error(), "shards") {
			return fmt.Errorf("expected the shards=%d rejection to name the shards argument, got: %v", shards, err)
		}
		if !strings.Contains(err.Error(), "quorum") {
			return fmt.Errorf("expected the shards=%d rejection to explain the quorum requirement, got: %v", shards, err)
		}
	}
	return nil
}

// ClusterRejectsTlsSecurity verifies a TLS listener profile is refused
// with an explanation rather than booting peers that spin forever on a
// failed handshake: a TLS node runs with `--port 0`, so the cluster bus
// would also have to run over TLS, and neither the peers nor the
// valkey-cli that bootstraps them get the trust material they'd need from
// a client-listener ServerSecurity.
//
// +cache="never"
func (t *Tests) ClusterRejectsTlsSecurity(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-cluster-tls")
	if err != nil {
		return err
	}
	// SAN value is irrelevant: the guard rejects before any node boots.
	cert, key, err := issueServerCert(ctx, ca, "valkey-placeholder", "vk-cluster-tls-server")
	if err != nil {
		return err
	}
	cluster := dag.Valkey().Cluster(
		pass,
		dag.Valkey().TLSServerSecurity(cert, key),
		dagger.ValkeyClusterOpts{Name: name},
	)
	_, err = cluster.Endpoints(ctx)
	if err == nil {
		return fmt.Errorf("expected a TLS Cluster topology to be rejected")
	}
	if !strings.Contains(err.Error(), "TLS") {
		return fmt.Errorf("expected the rejection to name TLS, got: %v", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Server tests — topology, defaults, caching.
// -----------------------------------------------------------------------------

// DefaultsProduceHealthyServer boots a default node and proves it is a
// healthy Valkey by answering PING and self-reporting 9.1 in INFO
// server. Catches image-path drift and a silently moved default tag.
//
// +cache="never"
func (t *Tests) DefaultsProduceHealthyServer(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	info, err := client.Info(ctx, dagger.ValkeyClientInfoOpts{Section: "server"})
	if err != nil {
		return fmt.Errorf("info server: %w", err)
	}
	if !strings.Contains(info, "valkey_version:9.1") {
		return fmt.Errorf("expected INFO server to report valkey_version:9.1.x, got:\n%s", info)
	}
	return nil
}

// EndpointShouldNotBeCached verifies Endpoint re-executes against the
// receiver it was called on rather than freezing on the first result.
// Valkey.Server is +cache="session", so a method that forgot its own
// +cache="never" would answer the second server with the first server's
// address.
//
// +cache="never"
func (t *Tests) EndpointShouldNotBeCached(ctx context.Context) error {
	first, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	second, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	ep1, err := first.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint 1: %w", err)
	}
	ep2, err := second.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint 2: %w", err)
	}
	if ep1 == ep2 {
		return fmt.Errorf("expected distinct endpoints for distinct server names (Endpoint likely cached), both were %q", ep1)
	}
	return nil
}

// SameNameServerSharesState verifies two Server calls with the same name
// resolve to one backing node: a key written through the first is
// readable through the second. Catches session cache-key drift, which
// would silently split a multi-step test across two empty keyspaces.
//
// +cache="never"
func (t *Tests) SameNameServerSharesState(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	open := func() *dagger.ValkeyServer {
		return dag.Valkey().Server(pass, dag.Valkey().PlaintextServerSecurity(), dagger.ValkeyServerOpts{Name: name})
	}
	key, err := randHex(ctx)
	if err != nil {
		return err
	}
	want, err := randHex(ctx)
	if err != nil {
		return err
	}
	if err := plaintextClient(open()).Set(ctx, key, want); err != nil {
		return fmt.Errorf("set via first handle: %w", err)
	}
	got, err := plaintextClient(open()).Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get via second handle (same name likely booted a second node): %w", err)
	}
	if got != want {
		return fmt.Errorf("expected %q through the second handle, got %q", want, got)
	}
	return nil
}

// BindServerReachableFromAlpine verifies BindServer wires the node into
// a consumer container under the hostname Endpoint reports, and that the
// consumer gets a real PONG back. A port probe would be a false
// positive: it proves something is listening, not that Valkey answers.
//
// +cache="never"
func (t *Tests) BindServerReachableFromAlpine(ctx context.Context) error {
	server, pass, err := bootServer(ctx)
	if err != nil {
		return err
	}
	endpoint, err := server.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	host, port, ok := strings.Cut(endpoint, ":")
	if !ok {
		return fmt.Errorf("expected a host:port endpoint, got %q", endpoint)
	}
	// The alpine-flavoured valkey image is the smallest thing that ships
	// valkey-cli; the password rides in as a secret env var so it never
	// enters the exec's argv.
	ctr := dag.Container().
		From("docker.io/valkey/valkey:9.1-alpine").
		WithSecretVariable("VALKEY_PW", pass)
	out, err := server.BindServer(ctr).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`valkey-cli --no-auth-warning -h %s -p %s -a "$VALKEY_PW" PING`, host, port,
		)}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("valkey-cli ping from consumer container: %w", err)
	}
	if !strings.Contains(out, "PONG") {
		return fmt.Errorf("expected PONG from %s, got %q", endpoint, out)
	}
	return nil
}

// PasswordReusableViaClient verifies the credentials a Server hands back
// authenticate through the standalone constructor: Endpoint + Password
// are enough to build a working client, which is what makes the same
// Client usable against a remote Valkey.
//
// +cache="never"
func (t *Tests) PasswordReusableViaClient(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	// Server.Client is what starts the backing service; the standalone
	// constructor only dials, so the node has to be up first.
	if err := plaintextClient(server).Ping(ctx); err != nil {
		return fmt.Errorf("boot ping: %w", err)
	}
	endpoint, err := server.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	user, err := server.User(ctx)
	if err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if user != "default" {
		return fmt.Errorf("expected the built-in requirepass user %q, got %q", "default", user)
	}
	host, _, _ := strings.Cut(endpoint, ":")
	standalone := dag.Valkey().Client(
		host,
		server.Password(),
		dag.Valkey().PlaintextClientSecurity(),
		dagger.ValkeyClientOpts{User: user},
	)
	if err := standalone.Ping(ctx); err != nil {
		return fmt.Errorf("standalone client ping against %s: %w", endpoint, err)
	}
	return nil
}

// StopTerminatesServer verifies Stop actually kills the backing service.
// The post-Stop probe goes through the standalone constructor on
// purpose: Server.Client would re-Start the service it is asked to dial
// and the test would pass no matter what Stop did.
//
// +cache="never"
func (t *Tests) StopTerminatesServer(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	if err := plaintextClient(server).Ping(ctx); err != nil {
		return fmt.Errorf("ping before stop: %w", err)
	}
	endpoint, err := server.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	host, _, _ := strings.Cut(endpoint, ":")
	standalone := dag.Valkey().Client(host, server.Password(), dag.Valkey().PlaintextClientSecurity())
	if err := standalone.Ping(ctx); err != nil {
		return fmt.Errorf("standalone ping before stop: %w", err)
	}
	if err := server.Stop(ctx); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if err := standalone.Ping(ctx); err == nil {
		return fmt.Errorf("expected ping to fail after Stop, but the node still answered")
	}
	return nil
}

// -----------------------------------------------------------------------------
// Client tests — command round-trips.
// -----------------------------------------------------------------------------

// commandFile materialises a *dagger.File of Valkey commands for
// ApplyFile.
func commandFile(contents string) *dagger.File {
	return dag.Directory().WithNewFile("seed.valkey", contents).File("seed.valkey")
}

// SetGetRoundTrip is the core smoke path: a value written through Set
// comes back through Get unchanged.
//
// +cache="never"
func (t *Tests) SetGetRoundTrip(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	key, err := randHex(ctx)
	if err != nil {
		return err
	}
	want, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)
	if err := client.Set(ctx, key, want); err != nil {
		return fmt.Errorf("set: %w", err)
	}
	got, err := client.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	if got != want {
		return fmt.Errorf("expected %q, got %q", want, got)
	}
	return nil
}

// GetShouldNotBeCached verifies Get re-executes on every call: write v1,
// read it, overwrite with v2, read again. A cached Get would still
// report v1 — the canonical stale-read bug a missing +cache="never"
// produces.
//
// +cache="never"
func (t *Tests) GetShouldNotBeCached(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	key, err := randHex(ctx)
	if err != nil {
		return err
	}
	first, err := randHex(ctx)
	if err != nil {
		return err
	}
	second, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)
	if err := client.Set(ctx, key, first); err != nil {
		return fmt.Errorf("set 1: %w", err)
	}
	got1, err := client.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get 1: %w", err)
	}
	if got1 != first {
		return fmt.Errorf("expected %q on the first read, got %q", first, got1)
	}
	if err := client.Set(ctx, key, second); err != nil {
		return fmt.Errorf("set 2: %w", err)
	}
	got2, err := client.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get 2: %w", err)
	}
	if got2 != second {
		return fmt.Errorf("expected %q after the overwrite (Get likely cached), got %q", second, got2)
	}
	return nil
}

// GetMissingKeyFails verifies absence stays distinguishable from an
// empty value: an unset key errors, while a key holding "" reads back as
// "".
//
// +cache="never"
func (t *Tests) GetMissingKeyFails(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	missing, err := randHex(ctx)
	if err != nil {
		return err
	}
	empty, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)
	if _, err := client.Get(ctx, missing); err == nil {
		return fmt.Errorf("expected Get on a missing key to fail, but it returned a value")
	} else if !strings.Contains(err.Error(), missing) {
		return fmt.Errorf("expected the error to name the missing key %q, got: %v", missing, err)
	}
	if err := client.Set(ctx, empty, ""); err != nil {
		return fmt.Errorf("set empty: %w", err)
	}
	got, err := client.Get(ctx, empty)
	if err != nil {
		return fmt.Errorf("expected a stored empty string to read back cleanly: %w", err)
	}
	if got != "" {
		return fmt.Errorf("expected an empty string back, got %q", got)
	}
	return nil
}

// SetWithTtlExpires verifies ttl is honoured at millisecond precision
// and that the key actually goes away.
//
// A sub-second ttl is deliberate: an implementation that floored the
// duration to whole seconds would send `EX 0`, which Valkey rejects
// outright, and one that mistook the unit would inflate PTTL far past
// the 500ms ceiling. The assertions are all one-sided upper bounds plus
// an expiry check, because every step here is a round trip through a
// re-probed service — a lower bound on the *remaining* TTL would be a
// stopwatch race under a loaded parallel suite.
//
// +cache="never"
func (t *Tests) SetWithTtlExpires(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	key, err := randHex(ctx)
	if err != nil {
		return err
	}
	value, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)

	if err := client.Set(ctx, key, value, dagger.ValkeyClientSetOpts{TTL: "500ms"}); err != nil {
		return fmt.Errorf("set with a sub-second ttl: %w", err)
	}
	pttl, err := pttlOf(ctx, client, key)
	if err != nil {
		return err
	}
	if pttl > 500 {
		return fmt.Errorf("expected a 500ms ttl to leave at most 500ms on the clock, PTTL reports %d (ttl parsed in the wrong unit)", pttl)
	}
	// Twice the ttl, so a slow round trip can only help.
	time.Sleep(time.Second)
	if _, err := client.Get(ctx, key); err == nil {
		return fmt.Errorf("expected the key to have expired after its ttl, but Get still returned a value")
	}

	// A long ttl proves the expiry is set at all rather than the key
	// simply never having been written.
	long, err := randHex(ctx)
	if err != nil {
		return err
	}
	if err := client.Set(ctx, long, value, dagger.ValkeyClientSetOpts{TTL: "1h"}); err != nil {
		return fmt.Errorf("set with a long ttl: %w", err)
	}
	longPttl, err := pttlOf(ctx, client, long)
	if err != nil {
		return err
	}
	if longPttl <= 0 || longPttl > int(time.Hour/time.Millisecond) {
		return fmt.Errorf("expected PTTL within (0, 3600000]ms for a 1h ttl, got %d (-1 means no expiry was set, -2 means no such key)", longPttl)
	}
	return nil
}

// pttlOf reads a key's remaining TTL in milliseconds through Do. Valkey
// answers -1 for a key with no expiry and -2 for a key that does not
// exist, so the caller gets those through unchanged.
func pttlOf(ctx context.Context, client *dagger.ValkeyClient, key string) (int, error) {
	reply, err := client.Do(ctx, []string{"PTTL", key})
	if err != nil {
		return 0, fmt.Errorf("pttl %s: %w", key, err)
	}
	pttl, err := strconv.Atoi(reply)
	if err != nil {
		return 0, fmt.Errorf("expected PTTL to decode as a JSON integer, got %q: %w", reply, err)
	}
	return pttl, nil
}

// DelRemovesKeys verifies Del reports how many keys it actually removed
// and that DbSize agrees with the survivors.
//
// +cache="never"
func (t *Tests) DelRemovesKeys(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	prefix, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)
	keys := []string{prefix + ":a", prefix + ":b", prefix + ":c"}
	for _, k := range keys {
		if err := client.Set(ctx, k, k); err != nil {
			return fmt.Errorf("seed %s: %w", k, err)
		}
	}
	// The third key is absent, so Del must report 2 removed, not 3.
	deleted, err := client.Del(ctx, []string{keys[0], keys[1], prefix + ":never-existed"})
	if err != nil {
		return fmt.Errorf("del: %w", err)
	}
	if deleted != 2 {
		return fmt.Errorf("expected Del to report 2 removed keys, got %d", deleted)
	}
	size, err := client.DbSize(ctx)
	if err != nil {
		return fmt.Errorf("dbsize: %w", err)
	}
	if size != 1 {
		return fmt.Errorf("expected 1 surviving key, DbSize reports %d", size)
	}
	return nil
}

// KeysScansPattern seeds ~1000 matching keys (plus a decoy prefix) and
// checks the full match set comes back. One SCAN page holds far fewer
// than that, so an implementation that returned the first page — or that
// stopped before the cursor wrapped to 0 — comes up short here.
//
// +cache="never"
func (t *Tests) KeysScansPattern(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	wanted, err := randHex(ctx)
	if err != nil {
		return err
	}
	decoy, err := randHex(ctx)
	if err != nil {
		return err
	}
	const n = 1000
	var seed strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&seed, "SET %s:%04d %d\n", wanted, i, i)
	}
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&seed, "SET %s:%04d %d\n", decoy, i, i)
	}

	client := plaintextClient(server)
	if err := client.ApplyFile(ctx, commandFile(seed.String())); err != nil {
		return fmt.Errorf("seed keys: %w", err)
	}
	keys, err := client.Keys(ctx, wanted+":*")
	if err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	if len(keys) != n {
		return fmt.Errorf("expected %d keys across every SCAN page, got %d", n, len(keys))
	}
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if !strings.HasPrefix(k, wanted+":") {
			return fmt.Errorf("expected only %s:* keys, got %q", wanted, k)
		}
		seen[k] = struct{}{}
	}
	if len(seen) != n {
		return fmt.Errorf("expected %d distinct keys, got %d (cursor pages likely overlap)", n, len(seen))
	}
	return nil
}

// DoReturnsJsonReplies verifies each RESP type survives the JSON
// encoding distinctly. The dangerous pair is nil versus empty string: a
// naive encoder collapses both to "".
//
// +cache="never"
func (t *Tests) DoReturnsJsonReplies(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	prefix, err := randHex(ctx)
	if err != nil {
		return err
	}
	value, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)

	cases := []struct {
		what string
		args []string
		want string
	}{
		{"status reply", []string{"SET", prefix + ":str", value}, strconv.Quote("OK")},
		{"bulk string reply", []string{"GET", prefix + ":str"}, strconv.Quote(value)},
		{"integer reply", []string{"INCR", prefix + ":n"}, "1"},
		{"nil reply", []string{"GET", prefix + ":missing"}, "null"},
		{"empty bulk string reply", []string{"SET", prefix + ":empty", ""}, strconv.Quote("OK")},
		{"stored empty string", []string{"GET", prefix + ":empty"}, `""`},
		{"array reply", []string{"RPUSH", prefix + ":list", "a", "b"}, "2"},
		{"array elements", []string{"LRANGE", prefix + ":list", "0", "-1"}, `["a","b"]`},
	}
	for _, tc := range cases {
		got, err := client.Do(ctx, tc.args)
		if err != nil {
			return fmt.Errorf("%s (%v): %w", tc.what, tc.args, err)
		}
		if got != tc.want {
			return fmt.Errorf("%s (%v): expected %s, got %s", tc.what, tc.args, tc.want, got)
		}
	}

	// Belt and braces on the pair that matters: nil and "" must not
	// decode to the same JSON value.
	nilReply, err := client.Do(ctx, []string{"GET", prefix + ":missing"})
	if err != nil {
		return fmt.Errorf("nil reply: %w", err)
	}
	var decoded any
	if err := json.Unmarshal([]byte(nilReply), &decoded); err != nil {
		return fmt.Errorf("expected a nil reply to be valid JSON, got %q: %w", nilReply, err)
	}
	if decoded != nil {
		return fmt.Errorf("expected a nil reply to decode as JSON null, got %#v", decoded)
	}
	return nil
}

// ApplyFileSeedsData verifies a command file lands as data on one
// connection, and that its quoting and line splitting survive: values
// with spaces, embedded quotes, blank lines, and `#` comments.
//
// +cache="never"
func (t *Tests) ApplyFileSeedsData(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	prefix, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)

	spaced := "hello there world"
	quoted := `say "hi"`
	seed := fmt.Sprintf(`# seed fixture for %s

SET %s:plain plain-value
SET %s:spaced "%s"
SET %s:quoted "say \"hi\""
SET %s:single 'single quoted value'
RPUSH %s:list one "two three"
`, prefix, prefix, prefix, spaced, prefix, prefix, prefix)

	if err := client.ApplyFile(ctx, commandFile(seed)); err != nil {
		return fmt.Errorf("apply file: %w", err)
	}

	for key, want := range map[string]string{
		prefix + ":plain":  "plain-value",
		prefix + ":spaced": spaced,
		prefix + ":quoted": quoted,
		prefix + ":single": "single quoted value",
	} {
		got, err := client.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("get %s: %w", key, err)
		}
		if got != want {
			return fmt.Errorf("expected %s to hold %q, got %q", key, want, got)
		}
	}

	list, err := client.Do(ctx, []string{"LRANGE", prefix + ":list", "0", "-1"})
	if err != nil {
		return fmt.Errorf("lrange: %w", err)
	}
	if want := `["one","two three"]`; list != want {
		return fmt.Errorf("expected the quoted argument to stay one element: wanted %s, got %s", want, list)
	}
	return nil
}

// ApplyFileReportsFailingCommand verifies a mid-file failure is not
// swallowed and that the error names the offending line.
//
// +cache="never"
func (t *Tests) ApplyFileReportsFailingCommand(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	prefix, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)

	// Line 3 stores a string; line 4 then asks Valkey to treat it as a
	// list, which fails with WRONGTYPE.
	seed := fmt.Sprintf("# fixture\n\nSET %s:k a-string\nLPUSH %s:k oops\nSET %s:after never-reached\n",
		prefix, prefix, prefix)

	err = client.ApplyFile(ctx, commandFile(seed))
	if err == nil {
		return fmt.Errorf("expected ApplyFile to fail on the WRONGTYPE command, but it succeeded")
	}
	if !strings.Contains(err.Error(), "line 4") {
		return fmt.Errorf("expected the error to name line 4, got: %v", err)
	}
	if _, err := client.Get(ctx, prefix+":after"); err == nil {
		return fmt.Errorf("expected ApplyFile to abort at the failing command, but a later command still ran")
	}
	return nil
}

// InfoReportsRole verifies INFO replication reports a standalone node as
// the primary. ReplicationReportsRoles is the multi-node counterpart,
// where a replica reports role:slave instead.
//
// +cache="never"
func (t *Tests) InfoReportsRole(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	info, err := plaintextClient(server).Info(ctx, dagger.ValkeyClientInfoOpts{Section: "replication"})
	if err != nil {
		return fmt.Errorf("info replication: %w", err)
	}
	if !strings.Contains(info, "role:master") {
		return fmt.Errorf("expected INFO replication to report role:master, got:\n%s", info)
	}
	return nil
}

// FlushAllClearsKeys verifies FlushAll empties the keyspace and DbSize
// returns to 0.
//
// +cache="never"
func (t *Tests) FlushAllClearsKeys(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	prefix, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := plaintextClient(server)
	seed := fmt.Sprintf("SET %s:a 1\nSET %s:b 2\nSET %s:c 3\n", prefix, prefix, prefix)
	if err := client.ApplyFile(ctx, commandFile(seed)); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	before, err := client.DbSize(ctx)
	if err != nil {
		return fmt.Errorf("dbsize before: %w", err)
	}
	if before != 3 {
		return fmt.Errorf("expected 3 seeded keys, got %d", before)
	}
	if err := client.FlushAll(ctx); err != nil {
		return fmt.Errorf("flushall: %w", err)
	}
	after, err := client.DbSize(ctx)
	if err != nil {
		return fmt.Errorf("dbsize after: %w", err)
	}
	if after != 0 {
		return fmt.Errorf("expected an empty keyspace after FlushAll, DbSize reports %d", after)
	}
	return nil
}

// ClientPingWrongPasswordFails proves requirepass is genuinely applied
// and the node is not wide open: a client holding a different password
// cannot authenticate.
//
// +cache="never"
func (t *Tests) ClientPingWrongPasswordFails(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	// Server.Client is what starts the service; without it the wrong
	// password would "fail" merely because nothing is listening.
	if err := plaintextClient(server).Ping(ctx); err != nil {
		return fmt.Errorf("boot ping: %w", err)
	}
	endpoint, err := server.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	host, _, _ := strings.Cut(endpoint, ":")
	wrong, err := randSecret(ctx)
	if err != nil {
		return err
	}
	err = dag.Valkey().Client(host, wrong, dag.Valkey().PlaintextClientSecurity()).Ping(ctx)
	if err == nil {
		return fmt.Errorf("expected Ping with the wrong password to fail, but the node accepted it")
	}
	msg := err.Error()
	if !strings.Contains(msg, "WRONGPASS") && !strings.Contains(msg, "NOAUTH") {
		return fmt.Errorf("expected a WRONGPASS/NOAUTH rejection, got: %v", err)
	}
	return nil
}

// DbSelectsLogicalDatabase verifies db is honoured end to end: a write
// on db 0 is invisible to a client on db 1, and each database keeps its
// own DbSize.
//
// +cache="never"
func (t *Tests) DbSelectsLogicalDatabase(ctx context.Context) error {
	server, _, err := bootServer(ctx)
	if err != nil {
		return err
	}
	key, err := randHex(ctx)
	if err != nil {
		return err
	}
	value, err := randHex(ctx)
	if err != nil {
		return err
	}
	zero := plaintextClient(server)
	if err := zero.Set(ctx, key, value); err != nil {
		return fmt.Errorf("set on db 0: %w", err)
	}
	endpoint, err := server.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	host, _, _ := strings.Cut(endpoint, ":")
	one := dag.Valkey().Client(
		host,
		server.Password(),
		dag.Valkey().PlaintextClientSecurity(),
		dagger.ValkeyClientOpts{Db: 1},
	)
	if _, err := one.Get(ctx, key); err == nil {
		return fmt.Errorf("expected the db 0 write to be invisible on db 1, but the key was readable")
	}
	size, err := one.DbSize(ctx)
	if err != nil {
		return fmt.Errorf("dbsize on db 1: %w", err)
	}
	if size != 0 {
		return fmt.Errorf("expected db 1 to be empty, DbSize reports %d", size)
	}
	// And the write is still there on db 0 — proving the isolation is the
	// database selection, not a lost write.
	got, err := zero.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get back on db 0: %w", err)
	}
	if got != value {
		return fmt.Errorf("expected %q back on db 0, got %q", value, got)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Replication tests — primary/replica topology.
//
// Replication is asynchronous, so every assertion about state that has to
// travel the link (a propagated write, the primary's replica count, the
// link status itself) polls to a deadline rather than reading once. A
// single read would be a stopwatch race against the initial full sync
// under a loaded parallel suite.
// -----------------------------------------------------------------------------

// replicationTimeout bounds every poll below. A full sync of an
// essentially empty keyspace takes well under a second once both nodes
// are up; the headroom is for image pulls and a contended engine.
const replicationTimeout = 90 * time.Second

// eventually polls check until it succeeds or the deadline passes,
// reporting the last failure so a timeout says what was still wrong
// rather than merely that time ran out.
func eventually(ctx context.Context, what string, check func(context.Context) error) error {
	deadline := time.Now().Add(replicationTimeout)
	for {
		err := check(ctx)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s within %s: %w", what, replicationTimeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// bootReplication mints a fresh primary/replica topology and returns it
// with the password secret every node shares. The topology name is a
// runtime-random value that folds into Valkey.Replication's
// +cache="session" key and into each node's hostname, so concurrent tests
// get independent topologies.
func bootReplication(ctx context.Context, replicas int) (*dagger.ValkeyReplication, *dagger.Secret, error) {
	name, err := randHex(ctx)
	if err != nil {
		return nil, nil, err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return nil, nil, err
	}
	rep := dag.Valkey().Replication(
		pass,
		dag.Valkey().PlaintextServerSecurity(),
		dagger.ValkeyReplicationOpts{Name: name, Replicas: replicas},
	)
	return rep, pass, nil
}

// replicaClients readies every replica and returns a plaintext client per
// replica. Pinging each one is what starts its backing service (and, as
// its dependency, the primary), so the caller's later assertions run
// against a topology that is actually up.
func replicaClients(ctx context.Context, rep *dagger.ValkeyReplication) ([]*dagger.ValkeyClient, error) {
	replicas, err := rep.Replicas(ctx)
	if err != nil {
		return nil, fmt.Errorf("replicas: %w", err)
	}
	clients := make([]*dagger.ValkeyClient, 0, len(replicas))
	for i := range replicas {
		client := plaintextClient(&replicas[i])
		if err := client.Ping(ctx); err != nil {
			return nil, fmt.Errorf("ping replica %d: %w", i, err)
		}
		clients = append(clients, client)
	}
	return clients, nil
}

// ReplicationPropagatesWritesToReplica is the core replication smoke
// path: a key written to the primary becomes readable from the replica.
// The read polls, because the link is asynchronous and a single read
// would race the initial sync.
//
// +cache="never"
func (t *Tests) ReplicationPropagatesWritesToReplica(ctx context.Context) error {
	rep, _, err := bootReplication(ctx, 1)
	if err != nil {
		return err
	}
	key, err := randHex(ctx)
	if err != nil {
		return err
	}
	want, err := randHex(ctx)
	if err != nil {
		return err
	}
	replicas, err := replicaClients(ctx, rep)
	if err != nil {
		return err
	}
	if len(replicas) != 1 {
		return fmt.Errorf("expected 1 replica, got %d", len(replicas))
	}
	if err := plaintextClient(rep.Primary()).Set(ctx, key, want); err != nil {
		return fmt.Errorf("set on primary: %w", err)
	}
	return eventually(ctx, "the primary's write to reach the replica", func(ctx context.Context) error {
		got, err := replicas[0].Get(ctx, key)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("expected %q on the replica, got %q", want, got)
		}
		return nil
	})
}

// ReplicationReportsRoles verifies INFO replication describes the
// topology from both ends: the primary is role:master with as many
// connected_slaves as there are replicas, and every replica reports the
// replica role. Two replicas rather than one, so a count that reported
// "some replica connected" instead of all of them fails here.
//
// +cache="never"
func (t *Tests) ReplicationReportsRoles(ctx context.Context) error {
	const wantReplicas = 2
	rep, _, err := bootReplication(ctx, wantReplicas)
	if err != nil {
		return err
	}
	replicas, err := replicaClients(ctx, rep)
	if err != nil {
		return err
	}
	if len(replicas) != wantReplicas {
		return fmt.Errorf("expected %d replicas, got %d", wantReplicas, len(replicas))
	}

	primary := plaintextClient(rep.Primary())
	err = eventually(ctx, "the primary to report both replicas", func(ctx context.Context) error {
		info, err := primary.Info(ctx, dagger.ValkeyClientInfoOpts{Section: "replication"})
		if err != nil {
			return err
		}
		if !strings.Contains(info, "role:master") {
			return fmt.Errorf("expected role:master on the primary, got:\n%s", info)
		}
		if want := fmt.Sprintf("connected_slaves:%d", wantReplicas); !strings.Contains(info, want) {
			return fmt.Errorf("expected %s on the primary, got:\n%s", want, info)
		}
		return nil
	})
	if err != nil {
		return err
	}

	for i, replica := range replicas {
		info, err := replica.Info(ctx, dagger.ValkeyClientInfoOpts{Section: "replication"})
		if err != nil {
			return fmt.Errorf("info replication on replica %d: %w", i, err)
		}
		// Valkey 9.1 still spells the replica role `slave` in INFO for
		// wire compatibility, so that — not `role:replica` — is what a
		// replica reports.
		if !strings.Contains(info, "role:slave") {
			return fmt.Errorf("expected the replica role on replica %d, got:\n%s", i, info)
		}
	}
	return nil
}

// ReplicationReplicaRejectsWrites verifies `--replica-read-only yes`
// holds: a write against a replica is refused with READONLY, so a test
// that writes to the wrong node fails loudly instead of silently losing
// the write on the next sync.
//
// +cache="never"
func (t *Tests) ReplicationReplicaRejectsWrites(ctx context.Context) error {
	rep, _, err := bootReplication(ctx, 1)
	if err != nil {
		return err
	}
	replicas, err := replicaClients(ctx, rep)
	if err != nil {
		return err
	}
	key, err := randHex(ctx)
	if err != nil {
		return err
	}
	value, err := randHex(ctx)
	if err != nil {
		return err
	}
	err = replicas[0].Set(ctx, key, value)
	if err == nil {
		return fmt.Errorf("expected a write against a read-only replica to be refused, but it succeeded")
	}
	if !strings.Contains(err.Error(), "READONLY") {
		return fmt.Errorf("expected a READONLY rejection, got: %v", err)
	}
	// The primary still takes the same write — proving the rejection is
	// the replica's read-only stance and not a broken topology.
	if err := plaintextClient(rep.Primary()).Set(ctx, key, value); err != nil {
		return fmt.Errorf("expected the primary to accept the same write: %w", err)
	}
	return nil
}

// ReplicationLinkAuthenticates verifies the replica actually authenticates
// to the password-protected primary: it polls for
// `master_link_status:up`, which a replica whose masterauth was missing or
// wrong never reaches — it stays `down`, retrying on NOAUTH, while every
// other assertion in this file would still pass against its (empty but
// readable) local keyspace.
//
// +cache="never"
func (t *Tests) ReplicationLinkAuthenticates(ctx context.Context) error {
	rep, _, err := bootReplication(ctx, 1)
	if err != nil {
		return err
	}
	replicas, err := replicaClients(ctx, rep)
	if err != nil {
		return err
	}
	primaryEndpoint, err := rep.Primary().Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("primary endpoint: %w", err)
	}
	primaryHost, _, _ := strings.Cut(primaryEndpoint, ":")

	return eventually(ctx, "the replication link to come up", func(ctx context.Context) error {
		info, err := replicas[0].Info(ctx, dagger.ValkeyClientInfoOpts{Section: "replication"})
		if err != nil {
			return err
		}
		if want := "master_host:" + primaryHost; !strings.Contains(info, want) {
			return fmt.Errorf("expected the replica to follow %s, got:\n%s", want, info)
		}
		if !strings.Contains(info, "master_link_status:up") {
			return fmt.Errorf("expected master_link_status:up (a wrong or missing masterauth leaves it down), got:\n%s", info)
		}
		return nil
	})
}

// ReplicationNodesHaveDistinctHostnames verifies every node gets its own
// pinned hostname and that two topologies built under different names do
// not collide. A shared hostname would silently fold two nodes onto one
// service: the replicas of one topology would answer for the other's, and
// a parallel suite would trade reads across tests.
//
// Endpoint is a pure accessor, so this asserts the addressing scheme
// without booting anything.
//
// +cache="never"
func (t *Tests) ReplicationNodesHaveDistinctHostnames(ctx context.Context) error {
	const replicas = 2
	first, _, err := bootReplication(ctx, replicas)
	if err != nil {
		return err
	}
	second, _, err := bootReplication(ctx, replicas)
	if err != nil {
		return err
	}

	seen := make(map[string]string)
	for _, topology := range []struct {
		label string
		rep   *dagger.ValkeyReplication
	}{{"first", first}, {"second", second}} {
		nodes, err := topology.rep.Replicas(ctx)
		if err != nil {
			return fmt.Errorf("%s replicas: %w", topology.label, err)
		}
		if len(nodes) != replicas {
			return fmt.Errorf("expected %d replicas in the %s topology, got %d", replicas, topology.label, len(nodes))
		}
		endpoints := make([]string, 0, replicas+1)
		primary, err := topology.rep.Primary().Endpoint(ctx)
		if err != nil {
			return fmt.Errorf("%s primary endpoint: %w", topology.label, err)
		}
		endpoints = append(endpoints, primary)
		for i := range nodes {
			endpoint, err := nodes[i].Endpoint(ctx)
			if err != nil {
				return fmt.Errorf("%s replica %d endpoint: %w", topology.label, i, err)
			}
			endpoints = append(endpoints, endpoint)
		}
		for i, endpoint := range endpoints {
			where := fmt.Sprintf("%s topology node %d", topology.label, i)
			if prior, ok := seen[endpoint]; ok {
				return fmt.Errorf("expected every node to get its own hostname, but %s and %s both answer at %s", prior, where, endpoint)
			}
			seen[endpoint] = where
		}
	}
	return nil
}

// ReplicationStopTerminatesEveryNode verifies no node in the topology
// answers once Stop returns. The post-Stop probes go through the
// standalone constructor on purpose: Server.Client would re-Start the
// service it is asked to dial and the test would pass no matter what Stop
// did.
//
// What this pins and what it cannot: a Stop that did nothing, that
// errored, or that stopped only the replicas is caught here. A Stop that
// stopped only the primary is NOT — measured against this engine, tearing
// down the primary also takes down the replicas bound to it, so
// reachability cannot separate "stopped every node" from "stopped the one
// every other node depends on". Replication.Stop still walks every node
// explicitly rather than leaning on that dependency teardown, which is
// not a documented guarantee.
//
// +cache="never"
func (t *Tests) ReplicationStopTerminatesEveryNode(ctx context.Context) error {
	rep, pass, err := bootReplication(ctx, 2)
	if err != nil {
		return err
	}
	// Ready every node, so a failed probe after Stop means Stop killed it
	// and not that it had never come up.
	if _, err := replicaClients(ctx, rep); err != nil {
		return err
	}
	if err := plaintextClient(rep.Primary()).Ping(ctx); err != nil {
		return fmt.Errorf("ping primary: %w", err)
	}

	replicas, err := rep.Replicas(ctx)
	if err != nil {
		return fmt.Errorf("replicas: %w", err)
	}
	nodes := make([]*dagger.ValkeyServer, 0, len(replicas)+1)
	nodes = append(nodes, rep.Primary())
	for i := range replicas {
		nodes = append(nodes, &replicas[i])
	}

	standalone := make([]*dagger.ValkeyClient, 0, len(nodes))
	for i, node := range nodes {
		endpoint, err := node.Endpoint(ctx)
		if err != nil {
			return fmt.Errorf("node %d endpoint: %w", i, err)
		}
		host, _, _ := strings.Cut(endpoint, ":")
		client := dag.Valkey().Client(host, pass, dag.Valkey().PlaintextClientSecurity())
		if err := client.Ping(ctx); err != nil {
			return fmt.Errorf("standalone ping of node %d before stop: %w", i, err)
		}
		standalone = append(standalone, client)
	}

	if err := rep.Stop(ctx); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	for i, client := range standalone {
		if err := client.Ping(ctx); err == nil {
			return fmt.Errorf("expected node %d to be down after Stop, but it still answered", i)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Security helpers — CA + leaf cert minting via certificate-management /
// crypto. Every key, password, and serial is minted at runtime; no
// literal credentials or PEM blobs appear in the suite.
// -----------------------------------------------------------------------------

// serverHost reproduces Valkey.Server's hostname derivation
// (`valkey-` + the first 12 hex chars of sha256(name)). Tests need it to
// mint a server certificate whose SAN matches the hostname the client
// dials — valkey-go pins ServerName to the dialed host and verifies it
// against the cert SAN, so a mismatch fails the handshake.
func serverHost(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "valkey-" + hex.EncodeToString(sum[:6])
}

// randNamedSecret mints a uniquely-named *dagger.Secret holding fresh
// random bytes. Used for the throwaway PKCS#12 passwords the
// certificate-management leaf issuers require (we consume the PEM cert /
// key directly, never the PKCS#12 archive, so the value is irrelevant).
func randNamedSecret(ctx context.Context, label string) (*dagger.Secret, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 32})
	if err != nil {
		return nil, err
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(label+"-"+suffix, h), nil
}

// freshCa mints a fresh per-test root CA via the certificate-management
// module from a runtime-random RSA key, password, and serial.
func freshCa(ctx context.Context, label string) (*dagger.CertificateManagementCertificateAuthority, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	key := dag.SetSecret(label+"-ca-key-"+suffix, keyPem)
	pwd, err := randNamedSecret(ctx, label+"-ca-pwd")
	if err != nil {
		return nil, fmt.Errorf("generate %s ca password: %w", label, err)
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s ca serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	return dag.CertificateManagement().CreateCertificateAuthority(nb, serial, pwd, key,
		dagger.CertificateManagementCreateCertificateAuthorityOpts{
			CommonName:   "valkey test ca " + label,
			ValidityDays: 30,
		}), nil
}

// leafKey mints a fresh RSA private key for a leaf certificate, wrapped
// in a uniquely-named *dagger.Secret (PEM PKCS#8, as the issuer expects).
func leafKey(ctx context.Context, label string) (*dagger.Secret, error) {
	keyPem, err := dag.Crypto().GenerateRsaKey(dagger.CryptoGenerateRsaKeyOpts{Bits: 2048}).Pem().Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate %s leaf key: %w", label, err)
	}
	suffix, err := randHex(ctx)
	if err != nil {
		return nil, err
	}
	return dag.SetSecret(label+"-leaf-key-"+suffix, keyPem), nil
}

// issueServerCert signs a server leaf certificate carrying host (and
// localhost / 127.0.0.1) as SANs, returning the PEM cert file and PEM
// key secret to hand to TlsServerSecurity / MtlsServerSecurity.
func issueServerCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, host, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label)
	if err != nil {
		return nil, nil, err
	}
	pwd, err := randNamedSecret(ctx, label+"-leaf-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	issued := ca.IssueServerCertificate(host, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueServerCertificateOpts{
			DNSSans:      []string{host, "localhost"},
			IPSans:       []string{"127.0.0.1"},
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// issueClientCert signs a client leaf certificate, returning the PEM
// cert file and PEM key secret to hand to MtlsClientSecurity. Valkey's
// mTLS only checks that the client cert chains to the trusted CA (unlike
// postgres, it does not additionally match the CN to the auth user), so
// the Common Name is a plain label.
func issueClientCert(ctx context.Context, ca *dagger.CertificateManagementCertificateAuthority, label string) (*dagger.File, *dagger.Secret, error) {
	key, err := leafKey(ctx, label)
	if err != nil {
		return nil, nil, err
	}
	pwd, err := randNamedSecret(ctx, label+"-leaf-pwd")
	if err != nil {
		return nil, nil, err
	}
	serial, err := dag.Random().Serial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s serial: %w", label, err)
	}
	nb := time.Now().UTC().Format(time.RFC3339)
	issued := ca.IssueClientCertificate(label, nb, serial, pwd, key,
		dagger.CertificateManagementCertificateAuthorityIssueClientCertificateOpts{
			ValidityDays: 30,
		})
	return issued.CertPemFile(), issued.PrivateKeyPem(), nil
}

// -----------------------------------------------------------------------------
// Security tests — TLS / mTLS listeners and clients. CA + leaf certs,
// passwords, and server names are all minted at runtime.
// -----------------------------------------------------------------------------

// ServerTlsRoundTripFromClient boots a one-way-TLS node and proves a
// matching TLS client — presenting NO client certificate — can Set + Get
// against it over the encrypted listener. That the certless client is
// accepted is the `--tls-auth-clients no` acceptance criterion: without
// that flag Valkey would demand a client cert and reject this dial.
//
// +cache="never"
func (t *Tests) ServerTlsRoundTripFromClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-tls")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, serverHost(name), "vk-tls-server")
	if err != nil {
		return err
	}
	server := dag.Valkey().Server(
		pass,
		dag.Valkey().TLSServerSecurity(cert, key),
		dagger.ValkeyServerOpts{Name: name},
	)
	clientSec := dag.Valkey().TLSClientSecurity(ca.CertPemFile())

	k, err := randHex(ctx)
	if err != nil {
		return err
	}
	want, err := randHex(ctx)
	if err != nil {
		return err
	}
	if err := server.Client(clientSec).Set(ctx, k, want); err != nil {
		return fmt.Errorf("set over TLS: %w", err)
	}
	got, err := server.Client(clientSec).Get(ctx, k)
	if err != nil {
		return fmt.Errorf("get over TLS: %w", err)
	}
	if got != want {
		return fmt.Errorf("expected %q over TLS round trip, got %q", want, got)
	}
	return nil
}

// ServerMtlsRoundTripFromClient boots a mutual-TLS node and proves a
// matching mTLS client (presenting a client cert signed by the trusted
// CA) can round-trip Set + Get.
//
// +cache="never"
func (t *Tests) ServerMtlsRoundTripFromClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	// One CA both signs the server leaf and anchors the accepted client
	// certs — the simplest symmetric mTLS trust setup.
	ca, err := freshCa(ctx, "vk-mtls")
	if err != nil {
		return err
	}
	serverCert, serverKey, err := issueServerCert(ctx, ca, serverHost(name), "vk-mtls-server")
	if err != nil {
		return err
	}
	clientCert, clientKey, err := issueClientCert(ctx, ca, "vk-mtls-client")
	if err != nil {
		return err
	}
	server := dag.Valkey().Server(
		pass,
		dag.Valkey().MtlsServerSecurity(serverCert, serverKey, ca.CertPemFile()),
		dagger.ValkeyServerOpts{Name: name},
	)
	clientSec := dag.Valkey().MtlsClientSecurity(ca.CertPemFile(), clientCert, clientKey)

	k, err := randHex(ctx)
	if err != nil {
		return err
	}
	want, err := randHex(ctx)
	if err != nil {
		return err
	}
	if err := server.Client(clientSec).Set(ctx, k, want); err != nil {
		return fmt.Errorf("set over mTLS: %w", err)
	}
	got, err := server.Client(clientSec).Get(ctx, k)
	if err != nil {
		return fmt.Errorf("get over mTLS: %w", err)
	}
	if got != want {
		return fmt.Errorf("expected %q over mTLS round trip, got %q", want, got)
	}
	return nil
}

// TlsServerRejectsPlaintextClient verifies the mode-coupling check:
// asking a TLS node for a plaintext client returns an error naming both
// modes, before any wire activity.
//
// +cache="never"
func (t *Tests) TlsServerRejectsPlaintextClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-tls-reject")
	if err != nil {
		return err
	}
	cert, key, err := issueServerCert(ctx, ca, serverHost(name), "vk-tls-reject-server")
	if err != nil {
		return err
	}
	server := dag.Valkey().Server(
		pass,
		dag.Valkey().TLSServerSecurity(cert, key),
		dagger.ValkeyServerOpts{Name: name},
	)
	err = server.Client(dag.Valkey().PlaintextClientSecurity()).Ping(ctx)
	if err == nil {
		return fmt.Errorf("expected plaintext client against TLS node to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "plaintext") || !strings.Contains(msg, "TLS") {
		return fmt.Errorf("expected mode-mismatch error naming both modes, got: %v", err)
	}
	return nil
}

// MtlsServerRejectsTlsOnlyClient verifies the mode-coupling check on the
// mTLS side: asking an mTLS node for a TLS-only client returns an error
// naming both modes, before any wire activity. The SAN value on the cert
// is irrelevant here — the guard fires in requireMode, ahead of any dial.
//
// +cache="never"
func (t *Tests) MtlsServerRejectsTlsOnlyClient(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-mtls-couple")
	if err != nil {
		return err
	}
	serverCert, serverKey, err := issueServerCert(ctx, ca, serverHost(name), "vk-mtls-couple-server")
	if err != nil {
		return err
	}
	server := dag.Valkey().Server(
		pass,
		dag.Valkey().MtlsServerSecurity(serverCert, serverKey, ca.CertPemFile()),
		dagger.ValkeyServerOpts{Name: name},
	)
	// TLS-only client (mode "TLS") against an mTLS listener (mode "MTLS").
	err = server.Client(dag.Valkey().TLSClientSecurity(ca.CertPemFile())).Ping(ctx)
	if err == nil {
		return fmt.Errorf("expected TLS-only client against mTLS node to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TLS") || !strings.Contains(msg, "mTLS") {
		return fmt.Errorf("expected mode-mismatch error naming both TLS and mTLS, got: %v", err)
	}
	return nil
}

// MtlsNodeDemandsClientCertAtWire is the test that pins the whole
// tls-auth-clients inversion: it boots an mTLS node, readies it with a
// valid mTLS client, then dials it with a TLS-only STANDALONE client
// (which carries no server reference and so bypasses the coupling check
// and reaches the wire). Presenting no client certificate, it must be
// rejected by the handshake — proving MTLS left `tls-auth-clients` at its
// default `yes`. A regression that passed `--tls-auth-clients no` for
// MTLS too would let this certless client through and go undetected by
// the round-trip tests.
//
// +cache="never"
func (t *Tests) MtlsNodeDemandsClientCertAtWire(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-mtls-wire")
	if err != nil {
		return err
	}
	host := serverHost(name)
	serverCert, serverKey, err := issueServerCert(ctx, ca, host, "vk-mtls-wire-server")
	if err != nil {
		return err
	}
	clientCert, clientKey, err := issueClientCert(ctx, ca, "vk-mtls-wire-client")
	if err != nil {
		return err
	}
	server := dag.Valkey().Server(
		pass,
		dag.Valkey().MtlsServerSecurity(serverCert, serverKey, ca.CertPemFile()),
		dagger.ValkeyServerOpts{Name: name},
	)
	// Start + ready the node using a valid mTLS client.
	mtlsSec := dag.Valkey().MtlsClientSecurity(ca.CertPemFile(), clientCert, clientKey)
	if err := server.Client(mtlsSec).Ping(ctx); err != nil {
		return fmt.Errorf("expected valid mTLS client to connect: %w", err)
	}
	// TLS-only standalone client: trusts the server CA but presents no
	// client cert. It bypasses the coupling check (no server reference) and
	// reaches the wire, where the mTLS handshake demands a client cert.
	tlsOnly := dag.Valkey().Client(host, pass, dag.Valkey().TLSClientSecurity(ca.CertPemFile()))
	err = tlsOnly.Ping(ctx)
	if err == nil {
		return fmt.Errorf("expected certless TLS-only client to be rejected by the mTLS listener")
	}
	// Valkey (via OpenSSL) aborts the handshake by dropping the connection
	// rather than sending a clean TLS alert, so valkey-go surfaces the
	// missing-client-cert rejection as a connection-level failure
	// ("connection reset by peer" / "broken pipe" / EOF) rather than a
	// parsed certificate error. Any of these confirms the wire-level mTLS
	// enforcement; a clean auth error (WRONGPASS/NOAUTH) would NOT, and is
	// deliberately excluded.
	low := strings.ToLower(err.Error())
	rejected := strings.Contains(low, "certificate") ||
		strings.Contains(low, "tls") ||
		strings.Contains(low, "handshake") ||
		strings.Contains(low, "eof") ||
		strings.Contains(low, "reset") ||
		strings.Contains(low, "broken pipe")
	if !rejected {
		return fmt.Errorf("expected a TLS handshake / connection-reset rejection, got: %v", err)
	}
	return nil
}

// PlaintextDialAgainstTlsNodeFails proves the plaintext listener is
// genuinely off (`--port 0`): a plaintext STANDALONE client (bypassing
// the coupling check) dialing the TLS node cannot speak the unencrypted
// wire protocol to the encrypted listener and fails. If `--port 0` were
// missing, a plaintext listener would still be up on 6379 and this dial
// would succeed.
//
// +cache="never"
func (t *Tests) PlaintextDialAgainstTlsNodeFails(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-tls-plain")
	if err != nil {
		return err
	}
	host := serverHost(name)
	cert, key, err := issueServerCert(ctx, ca, host, "vk-tls-plain-server")
	if err != nil {
		return err
	}
	server := dag.Valkey().Server(
		pass,
		dag.Valkey().TLSServerSecurity(cert, key),
		dagger.ValkeyServerOpts{Name: name},
	)
	// Ready the node over TLS first, so the failure below is a genuine
	// protocol/port rejection and not merely "nothing is listening yet".
	if err := server.Client(dag.Valkey().TLSClientSecurity(ca.CertPemFile())).Ping(ctx); err != nil {
		return fmt.Errorf("expected TLS client to ready the node: %w", err)
	}
	// Plaintext standalone dial against the TLS node.
	err = dag.Valkey().Client(host, pass, dag.Valkey().PlaintextClientSecurity()).Ping(ctx)
	if err == nil {
		return fmt.Errorf("expected a plaintext dial against a TLS node to fail (plaintext listener should be off), but it succeeded")
	}
	return nil
}

// TlsServerRejectsEmptyName verifies a TLS node rejects an empty `name`.
// The node hostname — and therefore the SAN the server cert must carry —
// derives from `name` alone, so an empty name would collapse every
// TLS/mTLS node onto the same sha256("") host and invite cert/SAN reuse.
// The guard fires in the constructor, before any service starts, so a
// placeholder SAN on the cert is fine here.
//
// +cache="never"
func (t *Tests) TlsServerRejectsEmptyName(ctx context.Context) error {
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-tls-emptyname")
	if err != nil {
		return err
	}
	// SAN value is irrelevant: the empty-name guard rejects before any dial
	// or TLS handshake.
	cert, key, err := issueServerCert(ctx, ca, "valkey-placeholder", "vk-tls-emptyname-server")
	if err != nil {
		return err
	}
	// No Name opt → defaults to "".
	server := dag.Valkey().Server(pass, dag.Valkey().TLSServerSecurity(cert, key))
	_, err = server.Endpoint(ctx)
	if err == nil {
		return fmt.Errorf("expected TLS node with empty name to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "name") || !strings.Contains(msg, "TLS") {
		return fmt.Errorf("expected empty-name rejection naming TLS, got: %v", err)
	}
	return nil
}

// BindServerReachableUnderTls binds a TLS node into an alpine-flavoured
// valkey container running valkey-cli: a `--tls --cacert` connection with
// the right CA gets a PONG, proving BindServer stays reachable under TLS
// from a consumer container. The password rides in as a secret env var so
// it never enters the exec's argv.
//
// +cache="never"
func (t *Tests) BindServerReachableUnderTls(ctx context.Context) error {
	name, err := randHex(ctx)
	if err != nil {
		return err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return err
	}
	ca, err := freshCa(ctx, "vk-tls-bind")
	if err != nil {
		return err
	}
	host := serverHost(name)
	cert, key, err := issueServerCert(ctx, ca, host, "vk-tls-bind-server")
	if err != nil {
		return err
	}
	server := dag.Valkey().Server(
		pass,
		dag.Valkey().TLSServerSecurity(cert, key),
		dagger.ValkeyServerOpts{Name: name},
	)
	ctr := server.BindServer(
		dag.Container().
			From("docker.io/valkey/valkey:9.1-alpine").
			WithFile("/tmp/ca.crt", ca.CertPemFile()).
			WithSecretVariable("VALKEY_PW", pass),
	)
	// valkey-cli flaps briefly while the node loads; retry the TLS PING.
	probe := fmt.Sprintf(
		`for i in $(seq 1 30); do `+
			`out=$(valkey-cli --no-auth-warning --tls --cacert /tmp/ca.crt -h %s -p %d -a "$VALKEY_PW" PING 2>/dev/null); `+
			`case "$out" in *PONG*) echo "$out"; exit 0;; esac; sleep 1; done; `+
			`echo TIMEOUT; exit 1`,
		host, 6379,
	)
	out, err := ctr.WithExec([]string{"sh", "-c", probe}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("valkey-cli TLS ping from consumer container: %w", err)
	}
	if !strings.Contains(out, "PONG") {
		return fmt.Errorf("expected PONG over TLS from %s, got %q", host, out)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Cluster tests — slot-sharded Valkey Cluster.
//
// Valkey.Cluster boots every node and bootstraps the slot assignment
// before it returns, so unlike the other topologies there is nothing to
// ready here: by the time a test can call a method on the cluster, the
// cluster is formed. What these tests pin is that the formation is real
// (every node agrees, every node advertises a routable identity of its
// own) and that the client addresses the whole sharded keyspace rather
// than the one shard it happened to seed from.
// -----------------------------------------------------------------------------

// bootCluster mints a fresh Valkey Cluster and returns it with the
// password secret every node shares. The cluster name is a runtime-random
// value that folds into Valkey.Cluster's +cache="session" key and into
// each node's hostname, so concurrent tests get independent clusters.
func bootCluster(ctx context.Context, shards, replicasPerShard int) (*dagger.ValkeyCluster, *dagger.Secret, error) {
	name, err := randHex(ctx)
	if err != nil {
		return nil, nil, err
	}
	pass, err := randSecret(ctx)
	if err != nil {
		return nil, nil, err
	}
	cluster := dag.Valkey().Cluster(
		pass,
		dag.Valkey().PlaintextServerSecurity(),
		dagger.ValkeyClusterOpts{Name: name, Shards: shards, ReplicasPerShard: replicasPerShard},
	)
	return cluster, pass, nil
}

// ClusterReportsFormedState verifies the bootstrap actually happened: the
// cluster reports cluster_state:ok (every one of the 16384 slots is owned
// by some primary) and knows exactly as many nodes as were booted.
//
// The node count is the half that catches a cluster which formed but
// didn't finish: a node whose gossip never reached the others still
// reports state:ok for the slots it can see, and only cluster_known_nodes
// gives it away.
//
// +cache="never"
func (t *Tests) ClusterReportsFormedState(ctx context.Context) error {
	const shards, replicasPerShard = 3, 1
	cluster, _, err := bootCluster(ctx, shards, replicasPerShard)
	if err != nil {
		return err
	}
	info, err := cluster.Client(dag.Valkey().PlaintextClientSecurity()).
		Do(ctx, []string{"CLUSTER", "INFO"})
	if err != nil {
		return fmt.Errorf("cluster info: %w", err)
	}
	if !strings.Contains(info, "cluster_state:ok") {
		return fmt.Errorf("expected cluster_state:ok, got:\n%s", info)
	}
	wantNodes := fmt.Sprintf("cluster_known_nodes:%d", shards*(1+replicasPerShard))
	if !strings.Contains(info, wantNodes) {
		return fmt.Errorf("expected %s, got:\n%s", wantNodes, info)
	}
	return nil
}

// ClusterAdvertisesPinnedHostnames verifies every node self-identifies
// with its own pinned hostname rather than falling back to localhost.
//
// This is the failure that looks like success. A node that cannot work
// out a routable identity announces the loopback address, and every peer
// dutifully records it — so each peer, following a MOVED redirect to
// "another" node, dials itself. The cluster never forms, or forms and
// then answers for the wrong shard, and CLUSTER INFO alone would not say
// why. Asserting the announced identity is what pins the fix.
//
// +cache="never"
func (t *Tests) ClusterAdvertisesPinnedHostnames(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx, 3, 1)
	if err != nil {
		return err
	}
	endpoints, err := cluster.Endpoints(ctx)
	if err != nil {
		return fmt.Errorf("endpoints: %w", err)
	}
	nodes, err := cluster.Client(dag.Valkey().PlaintextClientSecurity()).
		Do(ctx, []string{"CLUSTER", "NODES"})
	if err != nil {
		return fmt.Errorf("cluster nodes: %w", err)
	}
	for _, endpoint := range endpoints {
		host, _, _ := strings.Cut(endpoint, ":")
		if !strings.Contains(nodes, host) {
			return fmt.Errorf("expected %s to appear in CLUSTER NODES, got:\n%s", host, nodes)
		}
	}
	for _, loopback := range []string{"localhost", "127.0.0.1"} {
		if strings.Contains(nodes, loopback) {
			return fmt.Errorf("expected no node to advertise %s, got:\n%s", loopback, nodes)
		}
	}
	return nil
}

// ClusterRoundTripsKeysAcrossSlots verifies one Client addresses the
// whole sharded keyspace. The keys are chosen to land on different slots
// (distinct random names, no `{...}` hashtag to pin them together), so
// writing and reading them all back through a single client means the
// client followed MOVED redirects to whichever primary owns each slot —
// and that every one of those primaries was reachable at the hostname it
// advertised.
//
// A client that silently talked to one node would fail the write, not the
// read: a key that hashes elsewhere is refused with MOVED, never stored
// locally.
//
// +cache="never"
func (t *Tests) ClusterRoundTripsKeysAcrossSlots(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx, 3, 0)
	if err != nil {
		return err
	}
	client := cluster.Client(dag.Valkey().PlaintextClientSecurity())

	// Enough keys that the odds of all of them hashing into slots owned by
	// a single primary are negligible; ClusterKeysScansEveryShard is what
	// actually proves the spread happened.
	const keyCount = 24
	values := make(map[string]string, keyCount)
	for i := 0; i < keyCount; i++ {
		key, err := randHex(ctx)
		if err != nil {
			return err
		}
		value, err := randHex(ctx)
		if err != nil {
			return err
		}
		values[key] = value
		if err := client.Set(ctx, key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	for key, want := range values {
		got, err := client.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("get %s: %w", key, err)
		}
		if got != want {
			return fmt.Errorf("expected %q at %s, got %q", want, key, got)
		}
	}
	return nil
}

// clusterNodeClients returns a standalone (non-cluster) client per node,
// each pinned to the one node it names. Cluster-wide assertions can't see
// the shard split — a cluster client deliberately hides it — so anything
// that needs to know which node actually holds what goes through these.
func clusterNodeClients(ctx context.Context, cluster *dagger.ValkeyCluster, pass *dagger.Secret) ([]*dagger.ValkeyClient, error) {
	endpoints, err := cluster.Endpoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("endpoints: %w", err)
	}
	clients := make([]*dagger.ValkeyClient, 0, len(endpoints))
	for _, endpoint := range endpoints {
		host, _, _ := strings.Cut(endpoint, ":")
		clients = append(clients, dag.Valkey().Client(host, pass, dag.Valkey().PlaintextClientSecurity()))
	}
	return clients, nil
}

// ClusterKeysScansEveryShard verifies Keys reports the whole match set
// across the sharded keyspace, not just the shard its seed node owns.
//
// SCAN names no key, so a cluster client has no slot to route it by and
// the command is answered by whichever node it lands on — an
// implementation that scanned one node would return roughly 1/shards of
// the keys and look plausible. The test therefore asserts twice: that
// every seeded key comes back, and (via per-node DbSize) that the keys
// really were split across more than one primary, so the first assertion
// can't pass vacuously on a cluster that happened to pile everything into
// one shard.
//
// +cache="never"
func (t *Tests) ClusterKeysScansEveryShard(ctx context.Context) error {
	const shards = 3
	cluster, pass, err := bootCluster(ctx, shards, 0)
	if err != nil {
		return err
	}
	prefix, err := randHex(ctx)
	if err != nil {
		return err
	}
	decoy, err := randHex(ctx)
	if err != nil {
		return err
	}

	// No `{...}` hashtag anywhere, so each key hashes on its own and the
	// set spreads over the slot space — and therefore over the primaries.
	const n = 300
	var seed strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&seed, "SET %s:%04d %d\n", prefix, i, i)
	}
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&seed, "SET %s:%04d %d\n", decoy, i, i)
	}

	client := cluster.Client(dag.Valkey().PlaintextClientSecurity())
	if err := client.ApplyFile(ctx, commandFile(seed.String())); err != nil {
		return fmt.Errorf("seed keys: %w", err)
	}

	nodes, err := clusterNodeClients(ctx, cluster, pass)
	if err != nil {
		return err
	}
	holding := 0
	for i, node := range nodes {
		size, err := node.DbSize(ctx)
		if err != nil {
			return fmt.Errorf("dbsize on node %d: %w", i, err)
		}
		if size > 0 {
			holding++
		}
		if size == n+10 {
			return fmt.Errorf("expected the keyspace to be sharded, but node %d holds all %d keys", i, size)
		}
	}
	if holding < 2 {
		return fmt.Errorf("expected the seeded keys to span more than one primary, only %d node(s) hold any", holding)
	}

	keys, err := client.Keys(ctx, prefix+":*")
	if err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	if len(keys) != n {
		return fmt.Errorf("expected %d keys across every shard, got %d", n, len(keys))
	}
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix+":") {
			return fmt.Errorf("expected only %s:* keys, got %q", prefix, k)
		}
		seen[k] = struct{}{}
	}
	if len(seen) != n {
		return fmt.Errorf("expected %d distinct keys, got %d (shard results likely overlap)", n, len(seen))
	}
	return nil
}

// ClusterDelSpansMultipleSlots verifies Del handles keys that hash to
// different slots. A single DEL naming two slots is refused outright with
// CROSSSLOT — the slots may live on different primaries and Valkey will
// not split a command across them — so an implementation that passed the
// whole list through fails here rather than deleting a partial set.
//
// One key in the list never existed, so a Del that reported the number of
// keys it was asked about rather than the number it removed is caught
// too.
//
// +cache="never"
func (t *Tests) ClusterDelSpansMultipleSlots(ctx context.Context) error {
	cluster, _, err := bootCluster(ctx, 3, 0)
	if err != nil {
		return err
	}
	prefix, err := randHex(ctx)
	if err != nil {
		return err
	}
	client := cluster.Client(dag.Valkey().PlaintextClientSecurity())

	const seeded = 12
	keys := make([]string, 0, seeded+1)
	for i := 0; i < seeded; i++ {
		key := fmt.Sprintf("%s:%02d", prefix, i)
		if err := client.Set(ctx, key, key); err != nil {
			return fmt.Errorf("seed %s: %w", key, err)
		}
		keys = append(keys, key)
	}
	keys = append(keys, prefix+":never-existed")

	deleted, err := client.Del(ctx, keys)
	if err != nil {
		return fmt.Errorf("del across slots: %w", err)
	}
	if deleted != seeded {
		return fmt.Errorf("expected Del to report %d removed keys, got %d", seeded, deleted)
	}
	survivors, err := client.Keys(ctx, prefix+":*")
	if err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	if len(survivors) != 0 {
		return fmt.Errorf("expected no surviving keys, got %v", survivors)
	}
	return nil
}

// ClusterBindNodesReachableFromConsumer verifies BindNodes wires EVERY
// member into a consumer container, not just a seed. A cluster client is
// redirected to a node's advertised hostname, so a container that could
// only resolve one of them would work right up until the first key that
// hashes elsewhere. The probe therefore pings every endpoint in turn from
// inside the container, using valkey-cli rather than the module's own
// client so the reachability being tested is the container's.
//
// It then writes and reads a set of keys through `valkey-cli -c`, which
// follows MOVED redirects the way any cluster client would. That covers
// the second half of BindNodes' contract: the container meets a cluster
// whose slots are already assigned. Against an unbootstrapped cluster
// every one of those writes would come back CLUSTERDOWN, no matter how
// reachable the nodes were.
//
// The whole probe runs in one exec so a partial failure names the node it
// failed on rather than surfacing as an opaque non-zero exit.
//
// +cache="never"
func (t *Tests) ClusterBindNodesReachableFromConsumer(ctx context.Context) error {
	cluster, pass, err := bootCluster(ctx, 3, 0)
	if err != nil {
		return err
	}
	endpoints, err := cluster.Endpoints(ctx)
	if err != nil {
		return fmt.Errorf("endpoints: %w", err)
	}
	prefix, err := randHex(ctx)
	if err != nil {
		return err
	}
	ctr := cluster.BindNodes(
		dag.Container().
			From("docker.io/valkey/valkey:9.1-alpine").
			WithSecretVariable("VALKEY_PW", pass),
	)
	probe := fmt.Sprintf(`set -u
seed=""
for ep in %s; do
  host=${ep%%%%:*}; port=${ep#*:}
  out=$(valkey-cli --no-auth-warning -h "$host" -p "$port" -a "$VALKEY_PW" PING 2>&1)
  case "$out" in
    *PONG*) echo "$host PONG";;
    *) echo "$host UNREACHABLE: $out"; exit 1;;
  esac
  [ -n "$seed" ] || seed="$host $port"
done
set -- $seed
for i in 0 1 2 3 4 5 6 7 8 9; do
  key="%s:$i"
  valkey-cli -c --no-auth-warning -h "$1" -p "$2" -a "$VALKEY_PW" SET "$key" "$i" >/dev/null
  got=$(valkey-cli -c --no-auth-warning -h "$1" -p "$2" -a "$VALKEY_PW" GET "$key" 2>&1)
  [ "$got" = "$i" ] || { echo "expected $i at $key, got: $got"; exit 1; }
done
echo "CLUSTER ROUND TRIP OK"
`, strings.Join(endpoints, " "), prefix)
	out, err := ctr.WithExec([]string{"sh", "-c", probe}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("valkey-cli probe of every bound node: %w", err)
	}
	for _, endpoint := range endpoints {
		host, _, _ := strings.Cut(endpoint, ":")
		if !strings.Contains(out, host+" PONG") {
			return fmt.Errorf("expected PONG from %s, got:\n%s", host, out)
		}
	}
	if !strings.Contains(out, "CLUSTER ROUND TRIP OK") {
		return fmt.Errorf("expected a cluster-mode round trip from the bound container, got:\n%s", out)
	}
	return nil
}

// ClusterStopTerminatesEveryNode verifies no member answers once Stop
// returns. The post-Stop probes go through the standalone constructor on
// purpose: a cluster client would be free to route its command to
// whichever node it liked, so a Stop that missed one node could still
// look dead — or, worse, look alive.
//
// Cluster members have no service bindings between them, so unlike the
// replication topology there is no dependency teardown to hide behind:
// every node that is still up after Stop is a node Stop failed to kill.
//
// +cache="never"
func (t *Tests) ClusterStopTerminatesEveryNode(ctx context.Context) error {
	cluster, pass, err := bootCluster(ctx, 3, 0)
	if err != nil {
		return err
	}
	// Bring the cluster up first: Valkey.Cluster starts nothing, so until
	// something drives the bootstrap there is no service for a standalone
	// client to even resolve.
	if err := cluster.Client(dag.Valkey().PlaintextClientSecurity()).Ping(ctx); err != nil {
		return fmt.Errorf("ping cluster: %w", err)
	}
	nodes, err := clusterNodeClients(ctx, cluster, pass)
	if err != nil {
		return err
	}
	// Ready every node, so a failed probe after Stop means Stop killed it
	// and not that it had never come up.
	for i, node := range nodes {
		if err := node.Ping(ctx); err != nil {
			return fmt.Errorf("ping node %d before stop: %w", i, err)
		}
	}
	if err := cluster.Stop(ctx); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	for i, node := range nodes {
		if err := node.Ping(ctx); err == nil {
			return fmt.Errorf("expected node %d to be down after Stop, but it still answered", i)
		}
	}
	return nil
}
