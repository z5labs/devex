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
// the primary. This is the assertion the replication follow-up builds
// on: a replica reports role:slave.
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
