package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
	ClientSecurityMode string
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
//   - `alphas % replicas != 0` — every Dgraph group must be full.
//   - `clientListenerSecurity == nil` — plaintext must be a deliberate
//     caller choice so a future TLS upgrade stays explicit.
//
// Session-cached so that repeated chained method calls on the returned
// cluster (e.g. Client.Mutate → Client.RunQuery in
// `client-mutate-then-query-round-trip`) observe the SAME underlying
// services — and therefore the same graph state. The acceptance
// criteria suggest +cache="never" here, but with `never` the engine
// re-spawns the cluster between Mutate and Query in the same test,
// losing the data the prior Mutate wrote (verified during impl). Every
// method on *Cluster and *Client still carries +cache="never" on its
// own line so any data-returning call re-executes per invocation.
//
// +cache="session"
func (d *Dgraph) Cluster(
	ctx context.Context,
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
	if clientListenerSecurity.Mode != "PLAINTEXT" {
		return nil, fmt.Errorf(
			"only PLAINTEXT clientListenerSecurity is supported in this story, got %q",
			clientListenerSecurity.Mode,
		)
	}

	// Stable hostnames are scoped per-cluster so parallel test
	// invocations don't collide on `zero-1` / `alpha-100`. The suffix is
	// derived from random material at construction time so distinct
	// Cluster() calls within one engine session also get distinct
	// service DNS, even though +cache="session" lets identical args
	// share one service.
	suffix, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 8})
	if err != nil {
		return nil, fmt.Errorf("mint cluster suffix: %w", err)
	}
	hostSuffix := strings.ToLower(suffix[:12])

	image := fmt.Sprintf("%s/dgraph/dgraph:%s", registry, tag)

	zeroHost := "zero-1-" + hostSuffix
	zeroSvc := dag.Container().
		From(image).
		WithExposedPort(5080).
		WithExposedPort(6080).
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{
				"dgraph", "zero",
				"--my=" + zeroHost + ":5080",
				"--replicas=" + strconv.Itoa(replicas),
				"--bindall",
			},
		}).
		WithHostname(zeroHost)

	alphaHosts := make([]string, alphas)
	alphaSvcs := make([]*dagger.Service, alphas)
	for i := 0; i < alphas; i++ {
		alphaHost := fmt.Sprintf("alpha-%d-%s", 100+i, hostSuffix)
		alphaHosts[i] = alphaHost
		alphaSvcs[i] = dag.Container().
			From(image).
			WithServiceBinding(zeroHost, zeroSvc).
			WithExposedPort(7080).
			WithExposedPort(8080).
			WithExposedPort(9080).
			AsService(dagger.ContainerAsServiceOpts{
				Args: []string{
					"dgraph", "alpha",
					"--my=" + alphaHost + ":7080",
					"--zero=" + zeroHost + ":5080",
					"--security=whitelist=0.0.0.0/0",
					"--bindall",
				},
			}).
			WithHostname(alphaHost)
	}

	return &Cluster{
		ZeroSvc:            zeroSvc,
		AlphaSvcs:          alphaSvcs,
		AlphaHosts:         alphaHosts,
		ClientSecurityMode: clientListenerSecurity.Mode,
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
// its HTTP listener (port 8080). Waits for each Alpha to report
// healthy.
//
// +cache="never"
func (c *Cluster) HttpEndpoints(ctx context.Context) ([]string, error) {
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	out := make([]string, len(c.AlphaHosts))
	for i, h := range c.AlphaHosts {
		out[i] = h + ":8080"
	}
	return out, nil
}

// start explicitly Starts every Alpha service so the WithHostname alias
// becomes session-reachable from the dgraph module runtime, then polls
// /health on each Alpha until ready. Dgraph Alphas accept gRPC
// connections before they have a Raft leader, so dialing too early
// returns "server is not ready to accept requests".
func (c *Cluster) start(ctx context.Context) error {
	for i, svc := range c.AlphaSvcs {
		if _, err := svc.Start(ctx); err != nil {
			return fmt.Errorf("start alpha %d: %w", i, err)
		}
	}
	for i, host := range c.AlphaHosts {
		if err := waitForAlphaReady(ctx, host+":8080"); err != nil {
			return fmt.Errorf("alpha %d (%s) not ready: %w", i, host, err)
		}
	}
	return nil
}

// waitForAlphaReady polls http://endpoint/health until it returns 200
// OK or the deadline passes. endpoint is the host:port pair returned
// by Service.Endpoint(ctx, port=8080).
func waitForAlphaReady(ctx context.Context, endpoint string) error {
	url := fmt.Sprintf("http://%s/health", endpoint)
	client := &http.Client{Timeout: 3 * time.Second}
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
// +cache="never"
func (c *Cluster) Client(ctx context.Context, security *ClientSecurity) (*Client, error) {
	endpoints, err := c.GrpcEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	return clientFrom(endpoints, security), nil
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
