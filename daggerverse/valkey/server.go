package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dagger/valkey/internal/dagger"
)

// valkeyPort is the port the node's client-facing listener binds. Valkey
// has no reason to move off 6379 in a throwaway test topology, so it is
// fixed rather than caller-overridable.
const valkeyPort = 6379

// Server represents a running single-node Valkey server plus the
// connection metadata callers need to reach it. Holds a reference to the
// backing service so callers can bind it into their own containers or
// open a valkey-go Client against it.
type Server struct {
	// +private
	Svc *dagger.Service
	// +private
	Host string
	// +private
	UserName string
	// +private
	Pass *dagger.Secret
	// +private
	ClientListenerMode string // PLAINTEXT | TLS | MTLS — drives Client coupling validation.
}

// Server spins up a single-node Valkey server listening on 6379 with
// `requirepass` auth. The listener mode (plaintext / TLS / mTLS) is
// chosen by clientListenerSecurity: PLAINTEXT keeps the plaintext TCP
// listener; TLS / MTLS swap it for an encrypted `--tls-port` listener
// with the plaintext port turned off (`--port 0`).
//
// Image: `<registry>/valkey/valkey:<tag>` — the `valkey/valkey` portion
// is fixed; only `registry` and `tag` are caller-overridable. The default
// tag `"9.1"` pins this story to Valkey 9.1.
//
// Rejected inputs (each surfaces a descriptive error rather than booting
// a half-broken or wide-open node):
//
//   - `password == nil` — an unauthenticated Valkey node is reachable by
//     anything that can route to it, so a password is mandatory.
//   - `clientListenerSecurity == nil` — plaintext must be a deliberate
//     caller choice, so a nil profile is rejected rather than defaulted.
//   - an incomplete TLS / MTLS profile (missing cert, key, or client CA)
//     — validateServerSecurity rejects it before boot.
//   - `name == ""` for a TLS / MTLS node — the hostname (and therefore
//     the SAN the server cert must carry) derives from `name`, so each
//     encrypted node needs a unique discriminator.
//
// Session-cached so that repeated chained method calls on the returned
// server (e.g. Client.Set → Client.Get across two Server.Client() calls
// in `set-get-round-trip`) observe the SAME underlying service — and
// therefore the same keyspace. Every method on *Server and *Client is
// independently marked never-cache, so any data-returning call
// re-executes per invocation.
//
// `name` is a caller-supplied discriminator that folds into the session
// cache key. Parallel test suites should pass a unique value per test so
// each test gets its own backing service — without it, every same-shape
// call collapses to one cached node and concurrent tests race on a
// shared keyspace. Same name + same shape still cache-hits, which is
// what a single test's chained Client calls need. Leaving the default
// empty is fine for ad-hoc `dagger call` use where only one node is in
// play.
//
// +cache="session"
func (v *Valkey) Server(
	ctx context.Context,
	// +default=""
	name string,
	// +default="docker.io"
	registry string,
	// +default="9.1"
	tag string,
	password *dagger.Secret,
	clientListenerSecurity *ServerSecurity,
) (*Server, error) {
	if password == nil {
		return nil, fmt.Errorf("password must not be nil; pass a *dagger.Secret with the requirepass value")
	}
	if clientListenerSecurity == nil {
		return nil, fmt.Errorf("clientListenerSecurity must not be nil; pass PlaintextServerSecurity() explicitly")
	}
	if err := validateServerSecurity(clientListenerSecurity); err != nil {
		return nil, err
	}
	// For TLS / mTLS the hostname is derived from `name` alone (so the
	// caller can mint a server cert whose SAN matches the dialed host).
	// An empty `name` collapses every such node onto the same sha256("")
	// hostname, colliding within one engine session and inviting the wrong
	// cert/SAN to be reused — so require a discriminator. valkey-go pins
	// ServerName to the dialed host and verifies it against the cert SAN,
	// exactly as postgres' sslmode=verify-full does.
	if name == "" && clientListenerSecurity.Mode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"name must not be empty for %s servers: the hostname derives from name and the server certificate's SAN must match it, so each TLS/mTLS server needs a unique name",
			securityModeLabel(clientListenerSecurity.Mode),
		)
	}

	return buildServer(name, valkeyImage(registry, tag), password, clientListenerSecurity, nil, nil), nil
}

// valkeyImage renders the image reference a node boots from. The
// `valkey/valkey` portion is fixed; only the registry and tag are
// caller-overridable.
func valkeyImage(registry, tag string) string {
	return fmt.Sprintf("%s/valkey/valkey:%s", registry, tag)
}

// serverHostname derives a node's stable hostname from its name. The
// hostname is scoped per-node so parallel invocations don't collide on a
// single `valkey` alias, and it is derived from `name` alone so a caller
// minting a TLS server certificate can predict the hostname and embed it
// as the cert's SAN — the same derivation postgres and kafka use.
func serverHostname(name string) string {
	keyBytes := sha256.Sum256([]byte(name))
	return "valkey-" + hex.EncodeToString(keyBytes[:6]) // 12 hex chars = 48 bits
}

// serviceBinding is a service a node must be able to dial by hostname
// before it starts — a replica's primary, for instance. It is a slice
// element rather than a map entry so the resulting container's args and
// mounts stay in a deterministic order and the LLB digest is stable
// across invocations.
type serviceBinding struct {
	host string
	svc  *dagger.Service
}

// buildServer assembles a single valkey-server node: the container, the
// security-derived listener flags, any extra `valkey-server` arguments
// (replication flags, say), and the service bindings the node needs in
// order to dial its peers. Inputs are assumed already validated —
// Valkey.Server and Valkey.Replication each validate before calling.
func buildServer(
	name string,
	image string,
	password *dagger.Secret,
	security *ServerSecurity,
	extraArgs []string,
	bindings []serviceBinding,
) *Server {
	host := serverHostname(name)

	// The password reaches valkey-server through a secret environment
	// variable rather than a literal argv entry: a plaintext
	// `--requirepass <pw>` would be baked into the container's args and
	// surface in the Dagger graph and traces.
	ctr := dag.Container().
		From(image).
		WithSecretVariable("VALKEY_PASSWORD", password).
		WithExposedPort(valkeyPort)

	// Bindings go on before AsService so the node resolves its peers from
	// /etc/hosts at boot, and so Dagger starts those peers as dependencies
	// of this node rather than leaving it to retry against a dead name.
	for _, b := range bindings {
		ctr = ctr.WithServiceBinding(b.host, b.svc)
	}

	ctr, args := applyServerSecurity(ctr, security)
	args = append(args, extraArgs...)

	// A shell wrapper is what makes "$VALKEY_PASSWORD" expand at boot;
	// AsService args are exec'd directly, with no shell to expand them.
	// docker-entrypoint.sh is retained (it prepares /data and drops to the
	// valkey user) and `exec` keeps valkey-server as PID 1's successor so
	// Service.Stop's signal reaches the server, not a shell.
	//
	// Long-running commands belong in AsService(Args), never WithExec —
	// valkey-server never exits, so a WithExec would deadlock the chain.
	svc := ctr.
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{"sh", "-c", "exec docker-entrypoint.sh valkey-server " + strings.Join(args, " ")},
		}).
		WithHostname(host)

	return &Server{
		Svc:                svc,
		Host:               host,
		UserName:           defaultUser,
		Pass:               password,
		ClientListenerMode: security.Mode,
	}
}

// Endpoint returns the node's `host:6379` address. It does NOT start the
// service: it is a pure accessor, mirroring postgres' Endpoint.
// BindServer is what makes that address reachable from a consumer
// container (WithServiceBinding starts the service as the consumer's
// dependency and wires its IP into /etc/hosts). For module-runtime
// access use Server.Client, which starts the service itself.
//
// Pre-starting the service from this module before a consumer binds it
// would register the service in the module's DNS domain, which the
// binding's host-file lookup can't resolve from a session-domain
// consumer — so the start must be driven by the binding, not here.
//
// +cache="never"
func (s *Server) Endpoint() string {
	return s.Host + ":" + strconv.Itoa(valkeyPort)
}

// User returns the ACL user clients authenticate as. `requirepass` sets
// the password of the built-in `default` user, so this is always
// "default" in this story; an ACL-file follow-up is what makes it vary.
//
// +cache="never"
func (s *Server) User() string {
	return s.UserName
}

// Password returns the `requirepass` secret the node was provisioned
// with, so callers can re-use it via Valkey.Client against the same
// endpoint.
//
// +cache="never"
func (s *Server) Password() *dagger.Secret {
	return s.Pass
}

// BindServer attaches the Valkey service to the given container under
// the same hostname Endpoint reports, so the container can dial the node
// at `Endpoint()` (e.g. `valkey-cli -h <host> -a <pw> PING`).
//
// +cache="never"
func (s *Server) BindServer(ctr *dagger.Container) *dagger.Container {
	return ctr.WithServiceBinding(s.Host, s.Svc)
}

// Client starts the node and returns a valkey-go Client wired with its
// endpoint, user, and password on logical database 0.
//
// The supplied ClientSecurity mode must match the node's listener mode
// (PLAINTEXT/TLS/MTLS); a mismatch returns an error naming both modes
// rather than failing opaquely at the wire. Readiness is then probed
// with the client itself, so a TLS / mTLS listener would be polled over
// TLS using the caller's own cert material.
//
// +cache="never"
func (s *Server) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	if err := s.requireMode(security); err != nil {
		return nil, err
	}
	client := clientFrom(s.Host, valkeyPort, s.UserName, s.Pass, 0, security)
	if err := s.start(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// requireMode validates that the client's security mode exactly matches
// the node's client-facing listener mode. Valkey.Client (the standalone
// constructor) has no server reference and therefore cannot perform this
// check — callers reaching a listener via a mismatched standalone client
// fail at the wire instead.
func (s *Server) requireMode(security *ClientSecurity) error {
	clientMode := "PLAINTEXT"
	if security != nil {
		clientMode = security.Mode
	}
	if clientMode != s.ClientListenerMode {
		return fmt.Errorf(
			"client uses %s but server listener is %s",
			securityModeLabel(clientMode), securityModeLabel(s.ClientListenerMode),
		)
	}
	return nil
}

// Stop tears down the service container backing this node. Tests should
// call this in a defer so the service span closes when the test returns.
// SIGKILL skips graceful shutdown — Valkey's save-on-shutdown path is
// wasted work for a torn-down test node.
//
// +cache="never"
func (s *Server) Stop(ctx context.Context) error {
	if s.Svc == nil {
		return nil
	}
	if _, err := s.Svc.Stop(ctx, dagger.ServiceStopOpts{Kill: true}); err != nil {
		return fmt.Errorf("stop valkey: %w", err)
	}
	return nil
}

// start explicitly Starts the service so its WithHostname alias becomes
// session-reachable from the valkey module runtime, then polls the
// supplied probe Client until the node accepts authenticated commands.
// Probing through the Client means the dial honours the listener's
// security mode using the caller's own credentials.
//
// valkey-server binds 6379 only after it finishes loading, so an early
// dial returns "connection refused" or LOADING; the retry loop absorbs
// both. This is the pure-Go analogue of dgraph's HTTP /health poll — no
// helper container in the module runtime.
func (s *Server) start(ctx context.Context, probe *Client) error {
	if s.Svc == nil {
		return fmt.Errorf("server has no backing service")
	}
	if _, err := s.Svc.Start(ctx); err != nil {
		return fmt.Errorf("start valkey: %w", err)
	}

	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := probe.Ping(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return fmt.Errorf("valkey %s not ready: %w", s.Host, lastErr)
}
