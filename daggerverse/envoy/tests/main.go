// Package main is the envoy-tests Dagger module: round-trip and unit
// checks for the envoy daggerverse module.
package main

import (
	"context"
	"fmt"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
	"gopkg.in/yaml.v3"
)

type Tests struct{}

// All runs every envoy test inside this suite.
//
// parallel caps how many tests run concurrently. Defaults to 1 (sequential)
// to mirror `go test` package-level semantics; pass 0 to fan out every test
// with no limit, or any positive integer to opt into a specific level of
// concurrency.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
	// +default=1
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("RejectsInvalidComponentName", t.RejectsInvalidComponentName)
	jobs = jobs.WithJob("RejectsUnknownClusterType", t.RejectsUnknownClusterType)
	jobs = jobs.WithJob("DefaultClusterTypeIsStrictDns", t.DefaultClusterTypeIsStrictDns)
	jobs = jobs.WithJob("RejectsDuplicateListenerName", t.RejectsDuplicateListenerName)
	jobs = jobs.WithJob("RejectsUnknownClusterReference", t.RejectsUnknownClusterReference)
	jobs = jobs.WithJob("CustomListenerBodyIsSpliced", t.CustomListenerBodyIsSpliced)
	jobs = jobs.WithJob("CustomHttpFilterBodyIsSpliced", t.CustomHttpFilterBodyIsSpliced)
	jobs = jobs.WithJob("ConfigFileOverridesRendered", t.ConfigFileOverridesRendered)
	jobs = jobs.WithJob("ServiceWithoutConfigFails", func(ctx context.Context) error {
		return t.ServiceWithoutConfigFails(ctx, envoyTag)
	})
	jobs = jobs.WithJob("AdminEndpointServesReady", func(ctx context.Context) error {
		return t.AdminEndpointServesReady(ctx, envoyTag)
	})
	jobs = jobs.WithJob("L7HttpRoundTrip", func(ctx context.Context) error {
		return t.L7HttpRoundTrip(ctx, envoyTag)
	})
	jobs = jobs.WithJob("L4TcpRoundTrip", func(ctx context.Context) error {
		return t.L4TcpRoundTrip(ctx, envoyTag)
	})
	jobs = jobs.WithJob("TlsServerSecurityRendersDownstreamTlsContext", t.TlsServerSecurityRendersDownstreamTlsContext)
	jobs = jobs.WithJob("MtlsServerSecurityRequiresClientCert", t.MtlsServerSecurityRequiresClientCert)
	jobs = jobs.WithJob("PlaintextServerSecurityRendersNoTransportSocket", t.PlaintextServerSecurityRendersNoTransportSocket)
	jobs = jobs.WithJob("TlsUpstreamSecurityRendersUpstreamTlsContext", t.TlsUpstreamSecurityRendersUpstreamTlsContext)
	jobs = jobs.WithJob("MtlsUpstreamSecurityIncludesClientLeaf", t.MtlsUpstreamSecurityIncludesClientLeaf)
	jobs = jobs.WithJob("PlaintextUpstreamSecurityRendersNoTransportSocket", t.PlaintextUpstreamSecurityRendersNoTransportSocket)
	jobs = jobs.WithJob("L7HttpsRoundTrip", func(ctx context.Context) error {
		return t.L7HttpsRoundTrip(ctx, envoyTag)
	})
	jobs = jobs.WithJob("L7HttpsMtlsRejectsAnonymousClient", func(ctx context.Context) error {
		return t.L7HttpsMtlsRejectsAnonymousClient(ctx, envoyTag)
	})
	jobs = jobs.WithJob("L7HttpsMtlsAcceptsAuthorizedClient", func(ctx context.Context) error {
		return t.L7HttpsMtlsAcceptsAuthorizedClient(ctx, envoyTag)
	})
	jobs = jobs.WithJob("UpstreamTlsRoundTrip", func(ctx context.Context) error {
		return t.UpstreamTlsRoundTrip(ctx, envoyTag)
	})
	jobs = jobs.WithJob("UpstreamMtlsRoundTrip", func(ctx context.Context) error {
		return t.UpstreamMtlsRoundTrip(ctx, envoyTag)
	})
	jobs = jobs.WithJob("L4TcpTlsRoundTrip", func(ctx context.Context) error {
		return t.L4TcpTlsRoundTrip(ctx, envoyTag)
	})
	return jobs.Run(ctx)
}

const (
	curlImage    = "curlimages/curl:8.10.1"
	probeImage   = "alpine:3"
	defaultAdmin = 9901
)

// marker returns a fresh hex marker suitable for tagging payloads
// pushed through Envoy in a round-trip test.
func marker(ctx context.Context) (string, error) {
	h, err := dag.Random().Sha256(ctx)
	if err != nil {
		return "", fmt.Errorf("random sha256: %w", err)
	}
	if len(h) < 16 {
		return "", fmt.Errorf("random sha256 too short: %d", len(h))
	}
	return h[:16], nil
}

// RejectsInvalidComponentName asserts that the typed component
// factories reject names that don't match [A-Za-z0-9_-]+ with a
// non-nil error. AC calls out Envoy.Cluster and Envoy.VirtualHost
// explicitly; RoutePrefix's cluster arg shares the same validator.
func (t *Tests) RejectsInvalidComponentName(ctx context.Context) error {
	e := dag.Envoy()

	if _, err := e.Cluster("bad name").ID(ctx); err == nil {
		return fmt.Errorf("Cluster(\"bad name\"): expected error, got nil")
	}
	if _, err := e.Cluster("ok").ID(ctx); err != nil {
		return fmt.Errorf("Cluster(\"ok\"): expected nil, got %w", err)
	}

	if _, err := e.VirtualHost("bad name", []string{"*"}).ID(ctx); err == nil {
		return fmt.Errorf("VirtualHost(\"bad name\"): expected error, got nil")
	}
	if _, err := e.VirtualHost("ok", []string{"*"}).ID(ctx); err != nil {
		return fmt.Errorf("VirtualHost(\"ok\"): expected nil, got %w", err)
	}

	if _, err := e.RoutePrefix("/", "bad name").ID(ctx); err == nil {
		return fmt.Errorf("RoutePrefix(\"/\", \"bad name\"): expected error, got nil")
	}
	if _, err := e.RoutePrefix("/", "ok").ID(ctx); err != nil {
		return fmt.Errorf("RoutePrefix(\"/\", \"ok\"): expected nil, got %w", err)
	}

	return nil
}

// RejectsUnknownClusterType asserts Envoy.Cluster rejects clusterType
// values outside {STATIC, STRICT_DNS, LOGICAL_DNS} with a non-nil
// error.
func (t *Tests) RejectsUnknownClusterType(ctx context.Context) error {
	e := dag.Envoy()

	if _, err := e.Cluster("ok", dagger.EnvoyClusterOpts{ClusterType: "DOES_NOT_EXIST"}).ID(ctx); err == nil {
		return fmt.Errorf("Cluster(\"ok\", DOES_NOT_EXIST): expected error, got nil")
	}
	for _, kind := range []string{"STATIC", "STRICT_DNS", "LOGICAL_DNS"} {
		if _, err := e.Cluster("ok", dagger.EnvoyClusterOpts{ClusterType: kind}).ID(ctx); err != nil {
			return fmt.Errorf("Cluster(\"ok\", %q): expected nil, got %w", kind, err)
		}
	}
	return nil
}

// DefaultClusterTypeIsStrictDns asserts a cluster built with default
// clusterType renders as `type: STRICT_DNS` in the rendered
// bootstrap YAML.
func (t *Tests) DefaultClusterTypeIsStrictDns(ctx context.Context) error {
	e := dag.Envoy()
	contents, err := e.Proxy().
		WithCluster(e.Cluster("upstream")).
		ConfigFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile().Contents: %w", err)
	}
	var cfg struct {
		StaticResources struct {
			Clusters []struct {
				Name string `yaml:"name"`
				Type string `yaml:"type"`
			} `yaml:"clusters"`
		} `yaml:"static_resources"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return fmt.Errorf("parse bootstrap yaml: %w\n---\n%s", err, contents)
	}
	if len(cfg.StaticResources.Clusters) != 1 {
		return fmt.Errorf("expected 1 cluster, got %d: %s", len(cfg.StaticResources.Clusters), contents)
	}
	c := cfg.StaticResources.Clusters[0]
	if c.Type != "STRICT_DNS" {
		return fmt.Errorf("expected clusters[0].type == STRICT_DNS, got %q", c.Type)
	}
	return nil
}

// RejectsDuplicateListenerName asserts that wiring two listeners
// sharing the same name into a Proxy causes ConfigFile() to return a
// non-nil error.
func (t *Tests) RejectsDuplicateListenerName(ctx context.Context) error {
	e := dag.Envoy()
	l := e.CustomListener("dup", "address: { socket_address: { address: 0.0.0.0, port_value: 18080 } }\nfilter_chains: []\n")
	_, err := e.Proxy().
		WithListener(l).
		WithListener(l).
		ConfigFile().
		Contents(ctx)
	if err == nil {
		return fmt.Errorf("expected ConfigFile() error for duplicate listener name, got nil")
	}
	return nil
}

// RejectsUnknownClusterReference asserts a listener whose filter
// chain references a cluster not registered via WithCluster causes
// ConfigFile() to return a non-nil error.
func (t *Tests) RejectsUnknownClusterReference(ctx context.Context) error {
	e := dag.Envoy()
	tcp := e.TCPProxy("tcp", "missing")
	listener := e.TCPListener("ingress", 14000, tcp)
	_, err := e.Proxy().
		WithListener(listener).
		ConfigFile().
		Contents(ctx)
	if err == nil {
		return fmt.Errorf("expected ConfigFile() error for unknown cluster ref, got nil")
	}
	return nil
}

// CustomListenerBodyIsSpliced asserts that a CustomListener's
// caller-supplied YAML body round-trips verbatim under
// static_resources.listeners with the builder-supplied name keyed
// in.
func (t *Tests) CustomListenerBodyIsSpliced(ctx context.Context) error {
	e := dag.Envoy()
	body := "address:\n" +
		"  socket_address:\n" +
		"    address: 0.0.0.0\n" +
		"    port_value: 18080\n" +
		"filter_chains: []\n"
	contents, err := e.Proxy().
		WithListener(e.CustomListener("manual", body)).
		ConfigFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile().Contents: %w", err)
	}
	var cfg struct {
		StaticResources struct {
			Listeners []map[string]any `yaml:"listeners"`
		} `yaml:"static_resources"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return fmt.Errorf("parse yaml: %w\n---\n%s", err, contents)
	}
	if len(cfg.StaticResources.Listeners) != 1 {
		return fmt.Errorf("expected 1 listener, got %d: %s", len(cfg.StaticResources.Listeners), contents)
	}
	l := cfg.StaticResources.Listeners[0]
	if l["name"] != "manual" {
		return fmt.Errorf("expected listeners[0].name == manual, got %v", l["name"])
	}
	addr, _ := l["address"].(map[string]any)
	sa, _ := addr["socket_address"].(map[string]any)
	if got := fmt.Sprintf("%v", sa["port_value"]); got != "18080" {
		return fmt.Errorf("expected port_value 18080, got %q", got)
	}
	if _, ok := l["filter_chains"]; !ok {
		return fmt.Errorf("expected filter_chains key spliced in, got %v", l)
	}
	return nil
}

// CustomHttpFilterBodyIsSpliced asserts that a CustomHttpFilter's
// caller-supplied YAML body lands as the filter's typed_config in
// the rendered HCM http_filters chain.
func (t *Tests) CustomHttpFilterBodyIsSpliced(ctx context.Context) error {
	e := dag.Envoy()
	rc := e.RouteConfig("rc").
		WithVirtualHost(e.VirtualHost("vh", []string{"*"}).
			WithRoute(e.RoutePrefix("/", "upstream")))
	hcm := e.HTTPConnectionManager("ingress", rc).
		WithHTTPFilter(e.CustomHTTPFilter("ext", "key: value\nnested:\n  inner: 42\n")).
		WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18080, hcm)
	contents, err := e.Proxy().
		WithListener(listener).
		WithCluster(e.Cluster("upstream")).
		ConfigFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile().Contents: %w", err)
	}
	var cfg struct {
		StaticResources struct {
			Listeners []map[string]any `yaml:"listeners"`
		} `yaml:"static_resources"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return fmt.Errorf("parse yaml: %w\n---\n%s", err, contents)
	}
	if len(cfg.StaticResources.Listeners) != 1 {
		return fmt.Errorf("expected 1 listener, got %d", len(cfg.StaticResources.Listeners))
	}
	fc, _ := cfg.StaticResources.Listeners[0]["filter_chains"].([]any)
	if len(fc) != 1 {
		return fmt.Errorf("expected 1 filter chain, got %d", len(fc))
	}
	filters, _ := fc[0].(map[string]any)["filters"].([]any)
	hcmFilter, _ := filters[0].(map[string]any)
	hcmTyped, _ := hcmFilter["typed_config"].(map[string]any)
	httpFilters, _ := hcmTyped["http_filters"].([]any)
	if len(httpFilters) != 2 {
		return fmt.Errorf("expected 2 http filters, got %d: %v", len(httpFilters), httpFilters)
	}
	ext, _ := httpFilters[0].(map[string]any)
	if ext["name"] != "ext" {
		return fmt.Errorf("expected http_filters[0].name == ext, got %v", ext["name"])
	}
	tc, _ := ext["typed_config"].(map[string]any)
	if fmt.Sprintf("%v", tc["key"]) != "value" {
		return fmt.Errorf("expected typed_config.key == value, got %v", tc["key"])
	}
	nested, _ := tc["nested"].(map[string]any)
	if fmt.Sprintf("%v", nested["inner"]) != "42" {
		return fmt.Errorf("expected typed_config.nested.inner == 42, got %v", nested["inner"])
	}
	router, _ := httpFilters[1].(map[string]any)
	if router["name"] != "envoy.filters.http.router" {
		return fmt.Errorf("expected last filter to be router, got %v", router["name"])
	}
	return nil
}

// ConfigFileOverridesRendered asserts that WithConfigFile fully
// replaces the rendered bootstrap; listeners and clusters added via
// WithListener/WithCluster are ignored when an override is set.
func (t *Tests) ConfigFileOverridesRendered(ctx context.Context) error {
	const overrideText = "admin:\n  address:\n    socket_address:\n      address: 0.0.0.0\n      port_value: 1234\n"
	override := dag.Directory().
		WithNewFile("override.yaml", overrideText).
		File("override.yaml")
	e := dag.Envoy()
	listener := e.CustomListener("manual", "address: { socket_address: { address: 0.0.0.0, port_value: 18080 } }\nfilter_chains: []\n")
	contents, err := e.Proxy().
		WithListener(listener).
		WithCluster(e.Cluster("upstream")).
		WithConfigFile(override).
		ConfigFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile().Contents: %w", err)
	}
	if contents != overrideText {
		return fmt.Errorf("expected override returned verbatim, got:\n---\n%s\n---\nwanted:\n---\n%s", contents, overrideText)
	}
	return nil
}

// proxyOpts builds a Proxy options struct pinning the envoy tag for
// service-level tests. AdminPort is left at the module default.
func proxyOpts(envoyTag string) dagger.EnvoyProxyOpts {
	return dagger.EnvoyProxyOpts{Tag: envoyTag}
}

// ServiceWithoutConfigFails asserts that Service() on a Proxy with
// no listeners, clusters, or override produces a container whose
// admin port never opens (the envoy binary refuses to start without
// -c).
func (t *Tests) ServiceWithoutConfigFails(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	svc := dag.Envoy().Proxy(proxyOpts(envoyTag)).Service()
	_, err := dag.Container().From(probeImage).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-extras"}).
		WithServiceBinding("envoy", svc).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 20); do
  if nc -z envoy 9901 2>/dev/null; then
    echo "admin port unexpectedly came up after ${i}s"
    exit 0
  fi
  sleep 1
done
echo "admin port stayed closed for 20s (expected)" >&2
exit 1
`}).
		Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected envoy without config to fail; admin port came up")
	}
	return nil
}

// AdminEndpointServesReady asserts the admin /ready endpoint
// returns HTTP 200 from a fresh probe container service-bound to a
// proxy whose minimal valid config is one HTTP listener wired to one
// cluster.
func (t *Tests) AdminEndpointServesReady(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	e := dag.Envoy()
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "upstream")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18080, hcm)
	cluster := e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 80))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithListener(listener).
		WithCluster(cluster).
		Service()
	_, err := dag.Container().From(curlImage).
		WithServiceBinding("envoy", svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf(`
set -eu
for i in $(seq 1 60); do
  CODE=$(curl -sS -o /dev/null -w '%%{http_code}' http://envoy:%d/ready || echo 000)
  if [ "$CODE" = "200" ]; then
    echo "admin /ready returned 200 after ${i}s"
    exit 0
  fi
  sleep 1
done
echo "admin /ready never returned 200" >&2
exit 1
`, defaultAdmin)}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("probe /ready: %w", err)
	}
	return nil
}

// pythonHttpUpstream returns a *dagger.Service running a tiny
// python:3-alpine HTTP server that responds 200 with the marker as
// its body on every GET. Using python avoids the per-image
// entrypoint / netcat-flag guesswork that bit earlier attempts.
func pythonHttpUpstream(mark string, port int) *dagger.Service {
	script := fmt.Sprintf(`import http.server, socketserver
MARKER = %q
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = MARKER.encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a, **k): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("", %d), H) as srv:
    srv.serve_forever()
`, mark, port)
	return dag.Container().From("python:3-alpine").
		WithExposedPort(port).
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{"python", "-u", "-c", script},
		})
}

// L7HttpRoundTrip stands up an HTTP upstream behind an Envoy
// HttpListener and asserts a request through Envoy returns a fresh
// random marker served by the upstream.
func (t *Tests) L7HttpRoundTrip(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	upstream := pythonHttpUpstream(mark, 5678)

	e := dag.Envoy()
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "upstream")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18080, hcm)
	cluster := e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5678))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("upstream", upstream).
		WithListener(listener).
		WithCluster(cluster).
		Service()
	_, err = dag.Container().From(curlImage).
		WithServiceBinding("envoy", svc).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  BODY=$(curl -sS http://envoy:18080/ || true)
  case "${BODY}" in *"${MARKER}"*) echo "marker observed after ${i}s"; exit 0 ;; esac
  sleep 1
done
echo "marker ${MARKER} never appeared in envoy response" >&2
echo "last body: ${BODY}" >&2
exit 1
`}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("L7 round-trip: %w", err)
	}
	return nil
}

// L4TcpRoundTrip stands up an alpine `nc` echo upstream behind an
// Envoy TcpListener and asserts that bytes sent through Envoy come
// back on the same TCP connection.
func (t *Tests) L4TcpRoundTrip(
	ctx context.Context,
	// +default="v1.32.1"
	envoyTag string,
) error {
	mark, err := marker(ctx)
	if err != nil {
		return err
	}
	upstream := dag.Container().From(probeImage).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-extras"}).
		WithExposedPort(5000).
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{"sh", "-c", "while true; do nc -l -p 5000 -e /bin/cat; done"},
		})

	e := dag.Envoy()
	tcp := e.TCPProxy("tcp", "upstream")
	listener := e.TCPListener("ingress", 14000, tcp)
	cluster := e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5000))
	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("upstream", upstream).
		WithListener(listener).
		WithCluster(cluster).
		Service()

	_, err = dag.Container().From(probeImage).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-extras"}).
		WithServiceBinding("envoy", svc).
		WithEnvVariable("MARKER", mark).
		WithExec([]string{"sh", "-c", `
set -eu
for i in $(seq 1 60); do
  OUT=$(printf "%s" "${MARKER}" | nc -w 3 envoy 14000 || true)
  case "${OUT}" in *"${MARKER}"*) echo "marker echoed after ${i}s"; exit 0 ;; esac
  sleep 1
done
echo "marker ${MARKER} never echoed back" >&2
echo "last out: ${OUT}" >&2
exit 1
`}).
		Sync(ctx)
	if err != nil {
		return fmt.Errorf("L4 round-trip: %w", err)
	}
	return nil
}
