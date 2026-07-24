package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"dagger/dgraph/internal/dagger"
)

// Cluster represents a running Dgraph cluster: a single Zero coordinator
// plus N Alpha data nodes grouped at the requested replication factor.
// Holds references to every service so callers can bind them into their
// own containers or open a dgo Client against them.
type Cluster struct {
	// +private
	ZeroSvc *dagger.Service
	// +private
	AlphaSvcs []*dagger.Service
	// +private
	AlphaHosts []string
	// +private
	ClientListenerMode string // PLAINTEXT | TLS | MTLS — drives Client coupling + endpoint scheme.
}

// Cluster spins up a Dgraph cluster of one Zero coordinator and `alphas`
// Alpha data nodes, with Alphas grouped at replication factor `replicas`
// (so every group is exactly `replicas` Alphas large; `alphas %
// replicas == 0` is required). All listeners use plaintext in this
// story.
//
// Image: `<registry>/dgraph/dgraph:<tag>` — the `dgraph/dgraph` portion
// is fixed; only `registry` and `tag` are caller-overridable.
//
// Rejected inputs (each surfaces a descriptive error rather than
// booting a half-broken cluster):
//
//   - `zeros != 1` — multi-Zero quorum needs every peer's address at
//     static config time via `--peer`, which Dagger's WithServiceBinding
//     model can't express without an unresolvable cycle. Multi-Zero HA
//     lands in a follow-up.
//   - `alphas < 1` or `replicas < 1`.
//   - `replicas > 1 && replicas % 2 == 0` — Dgraph's Raft consensus
//     needs an odd replica count per group (or `replicas == 1` for no
//     replication).
//   - `alphas % replicas != 0` — every Dgraph group must be full.
//   - `clientListenerSecurity == nil` — plaintext must be a deliberate
//     caller choice so a future TLS upgrade stays explicit.
//
// Session-cached so that repeated chained method calls on the returned
// cluster (e.g. Client.Mutate → Client.RunQuery in
// `client-mutate-then-query-round-trip`) observe the SAME underlying
// services — and therefore the same graph state. The acceptance
// criteria suggest a never-cache here, but under never-cache the engine
// re-spawns the cluster between Mutate and Query in the same test,
// losing the data the prior Mutate wrote (verified during impl). Every
// method on *Cluster and *Client is independently marked never-cache, so
// any data-returning call re-executes per invocation.
//
// `name` is a caller-supplied discriminator that folds into the session
// cache key. Parallel test suites should pass a unique value per test
// (e.g. the test function name) so each test gets its own backing
// services — without it, every same-shape call collapses to one cached
// cluster and concurrent tests race on shared schema and storage. Same
// name + same shape still cache-hits, which is what a single test's
// chained Client.Mutate → Client.RunQuery sequence needs. Leaving the
// default empty is fine for ad-hoc `dagger call` use where only one
// cluster is in play.
//
// +cache="session"
func (d *Dgraph) Cluster(
	ctx context.Context,
	// +default=""
	name string,
	// +default=1
	zeros int,
	// +default=1
	alphas int,
	// +default=1
	replicas int,
	// +default="docker.io"
	registry string,
	// +default="v24.0.4"
	tag string,
	clientListenerSecurity *ServerSecurity,
) (*Cluster, error) {
	if zeros != 1 {
		return nil, fmt.Errorf(
			"only single-Zero clusters are supported in this story (got zeros=%d); see README for details",
			zeros,
		)
	}
	if alphas < 1 {
		return nil, fmt.Errorf("at least one alpha is required, got %d", alphas)
	}
	if replicas < 1 {
		return nil, fmt.Errorf("replicas must be >= 1, got %d", replicas)
	}
	if replicas > 1 && replicas%2 == 0 {
		return nil, fmt.Errorf(
			"replicas must be odd (or 1) for Raft consensus, got %d",
			replicas,
		)
	}
	if alphas%replicas != 0 {
		return nil, fmt.Errorf(
			"alphas (%d) must be a multiple of replicas (%d): every dgraph group must be full",
			alphas, replicas,
		)
	}
	if clientListenerSecurity == nil {
		return nil, fmt.Errorf("clientListenerSecurity must not be nil; pass PlaintextServerSecurity() explicitly")
	}
	if err := validateServerSecurity(clientListenerSecurity); err != nil {
		return nil, err
	}
	// An empty `name` collapses every same-shape cluster onto the same
	// sha256 hostSuffix within one engine session, inviting the wrong
	// cert/SAN to be reused across TLS/mTLS clusters. The client verifies
	// each Alpha's certificate SAN against the dialed hostname, so a
	// TLS/mTLS caller must be able to predict a unique hostname to embed
	// in the cert — require a discriminator.
	if name == "" && clientListenerSecurity.Mode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"name must not be empty for %s clusters: the Alpha/Zero hostnames derive from name and the server certificate's SAN must match them, so each TLS/mTLS cluster needs a unique name",
			securityModeLabel(clientListenerSecurity.Mode),
		)
	}

	image := fmt.Sprintf("%s/dgraph/dgraph:%s", registry, tag)

	// Stable hostnames are scoped per-cluster so parallel test
	// invocations don't collide on `zero-1` / `alpha-100`. The suffix is
	// derived deterministically from every arg that distinguishes one
	// cache entry from another, so two cache entries can never produce
	// the same hostnames within one engine session — and identical-arg
	// calls hit the +cache="session" entry without re-executing this
	// path at all.
	keyBytes := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d|%d|%s",
		name, zeros, alphas, replicas, image,
	))
	hostSuffix := hex.EncodeToString(keyBytes[:6]) // 12 hex chars = 48 bits

	zeroHost := "zero-1-" + hostSuffix
	zeroCtr, tlsArgs := applyServerSecurity(dag.Container().From(image), clientListenerSecurity)
	zeroSvc := zeroCtr.
		WithExposedPort(5080).
		WithExposedPort(6080).
		AsService(dagger.ContainerAsServiceOpts{
			Args: append([]string{
				"dgraph", "zero",
				"--my=" + zeroHost + ":5080",
				"--replicas=" + strconv.Itoa(replicas),
				"--bindall",
			}, tlsArgs...),
		}).
		WithHostname(zeroHost)

	alphaHosts := make([]string, alphas)
	alphaSvcs := make([]*dagger.Service, alphas)
	for i := 0; i < alphas; i++ {
		alphaHost := fmt.Sprintf("alpha-%d-%s", 100+i, hostSuffix)
		alphaHosts[i] = alphaHost
		alphaCtr, alphaTlsArgs := applyServerSecurity(dag.Container().From(image), clientListenerSecurity)
		alphaSvcs[i] = alphaCtr.
			WithServiceBinding(zeroHost, zeroSvc).
			WithExposedPort(7080).
			WithExposedPort(8080).
			WithExposedPort(9080).
			AsService(dagger.ContainerAsServiceOpts{
				Args: append([]string{
					"dgraph", "alpha",
					"--my=" + alphaHost + ":7080",
					"--zero=" + zeroHost + ":5080",
					"--security=whitelist=0.0.0.0/0",
					"--bindall",
				}, alphaTlsArgs...),
			}).
			WithHostname(alphaHost)
	}

	return &Cluster{
		ZeroSvc:            zeroSvc,
		AlphaSvcs:          alphaSvcs,
		AlphaHosts:         alphaHosts,
		ClientListenerMode: clientListenerSecurity.Mode,
	}, nil
}

// GrpcEndpoints returns the host:port pairs each Alpha advertises on
// its external gRPC listener (port 9080), suitable for passing to dgo.
// Explicitly Starts each Alpha and waits for it to report healthy
// before returning so module-runtime callers can dial immediately.
//
// +cache="never"
func (c *Cluster) GrpcEndpoints(ctx context.Context) ([]string, error) {
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	out := make([]string, len(c.AlphaHosts))
	for i, h := range c.AlphaHosts {
		out[i] = h + ":9080"
	}
	return out, nil
}

// HttpEndpoints returns the host:port pairs each Alpha advertises on
// its HTTP listener (port 8080). Waits for each Alpha to report healthy.
// Once the cluster's client-facing listener is TLS or mTLS the entries
// are prefixed with `https://` so HTTP callers dial the encrypted
// listener; plaintext clusters return the scheme-less `host:8080` form
// (unchanged from the MVP). GrpcEndpoints stays scheme-less in every
// mode — dgo takes a bare `host:9080`.
//
// +cache="never"
func (c *Cluster) HttpEndpoints(ctx context.Context) ([]string, error) {
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	out := make([]string, len(c.AlphaHosts))
	for i, h := range c.AlphaHosts {
		if c.ClientListenerMode == "PLAINTEXT" {
			out[i] = h + ":8080"
		} else {
			out[i] = "https://" + h + ":8080"
		}
	}
	return out, nil
}

// start explicitly Starts Zero and every Alpha so the WithHostname
// aliases become session-reachable from the dgraph module runtime,
// then polls /health on each Alpha until ready. Dgraph Alphas accept
// gRPC connections before they have a Raft leader, so dialing too
// early returns "server is not ready to accept requests".
//
// Zero is started explicitly so a Stop+start sequence (used by the
// `*ShouldNotBeCached` tests) fully heals the cluster: without an
// explicit Zero restart, alphas come back but can't reach the dead
// Zero and stay in /health=503 forever.
func (c *Cluster) start(ctx context.Context) error {
	if c.ZeroSvc != nil {
		if _, err := c.ZeroSvc.Start(ctx); err != nil {
			return fmt.Errorf("start zero: %w", err)
		}
	}
	for i, svc := range c.AlphaSvcs {
		if _, err := svc.Start(ctx); err != nil {
			return fmt.Errorf("start alpha %d: %w", i, err)
		}
	}
	// mTLS listeners reject an HTTP /health probe that presents no client
	// certificate (client-auth-type=REQUIREANDVERIFY), and the cluster
	// runtime holds no client cert. Readiness for mTLS is instead verified
	// by Cluster.Client, which retries a gRPC call using the caller's own
	// cert material. For PLAINTEXT and one-way TLS the /health probe works
	// (TLS presents no client cert under the default VERIFYIFGIVEN).
	if c.ClientListenerMode == "MTLS" {
		return nil
	}
	for i, host := range c.AlphaHosts {
		if err := waitForAlphaReady(ctx, host+":8080", c.ClientListenerMode != "PLAINTEXT"); err != nil {
			return fmt.Errorf("alpha %d (%s) not ready: %w", i, host, err)
		}
	}
	return nil
}

// waitForAlphaReady polls <scheme>://endpoint/health until it returns 200
// OK or the deadline passes. endpoint is a `<alpha-host>:8080` string
// — the hostname is the per-cluster Dagger WithHostname alias, which
// resolves over the engine's session DNS from any container in the
// same session. When useTLS is set the probe dials https and skips
// certificate verification (the cluster runtime holds no server CA — it
// only needs to know the listener is up, not authenticate it).
func waitForAlphaReady(ctx context.Context, endpoint string, useTLS bool) error {
	scheme := "http"
	transport := &http.Transport{}
	if useTLS {
		scheme = "https"
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	url := fmt.Sprintf("%s://%s/health", scheme, endpoint)
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return lastErr
}

// AlphaHostNames returns the cluster's Alpha hostnames (no port), for
// callers that need to reference an Alpha by name from a container
// attached via BindAlphas.
//
// +cache="never"
func (c *Cluster) AlphaHostNames() []string {
	out := make([]string, len(c.AlphaHosts))
	copy(out, c.AlphaHosts)
	return out
}

// BindAlphas attaches every Alpha service to the given container under
// the same hostname GrpcEndpoints / HttpEndpoints reports, so the
// container can dial Alphas using the same address strings as a dgo
// Client returned from Cluster.Client.
//
// +cache="never"
func (c *Cluster) BindAlphas(ctr *dagger.Container) *dagger.Container {
	for i, svc := range c.AlphaSvcs {
		ctr = ctr.WithServiceBinding(c.AlphaHosts[i], svc)
	}
	return ctr
}

// Client starts every Alpha service in the cluster and returns a dgo
// Client wired with their gRPC endpoints.
//
// The supplied ClientSecurity mode must match the cluster's client-facing
// listener mode (PLAINTEXT/TLS/MTLS); a mismatch returns an error naming
// both modes rather than failing opaquely at the wire. Readiness is then
// verified with the client itself — a gRPC schema query retried until the
// Alphas report a Raft leader — so an mTLS listener is polled over mTLS
// using the caller's own cert material (the only way to authenticate the
// probe against a REQUIREANDVERIFY listener). Dgraph.Client (the
// standalone constructor) has no cluster reference and therefore cannot
// perform the mode check; callers reaching a listener via a mismatched
// standalone client fail at the wire instead.
//
// +cache="never"
func (c *Cluster) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	if err := c.requireMode(security); err != nil {
		return nil, err
	}
	endpoints, err := c.GrpcEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	client := clientFrom(endpoints, security)
	if err := client.waitReady(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

// requireMode validates that the client's security mode exactly matches
// the cluster's client-facing listener mode.
func (c *Cluster) requireMode(security *ClientSecurity) error {
	clientMode := "PLAINTEXT"
	if security != nil {
		clientMode = security.Mode
	}
	if clientMode != c.ClientListenerMode {
		return fmt.Errorf(
			"client uses %s but cluster listener is %s",
			securityModeLabel(clientMode), securityModeLabel(c.ClientListenerMode),
		)
	}
	return nil
}

// Stop tears down every service container backing this cluster (the
// Zero plus every Alpha). Tests should call this in a defer so each
// service span closes when the test returns. SIGKILL skips graceful
// shutdown — Dgraph's shutdown path waits on Raft drain timeouts that
// a torn-down test cluster doesn't need.
//
// +cache="never"
func (c *Cluster) Stop(ctx context.Context) error {
	opts := dagger.ServiceStopOpts{Kill: true}
	var errs []error
	if c.ZeroSvc != nil {
		if _, err := c.ZeroSvc.Stop(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("stop zero: %w", err))
		}
	}
	for i, svc := range c.AlphaSvcs {
		if svc == nil {
			continue
		}
		if _, err := svc.Stop(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("stop alpha %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}
