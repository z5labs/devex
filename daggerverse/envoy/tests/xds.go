package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	"gopkg.in/yaml.v3"
)

// DynamicResourcesRendersDynamicBootstrap asserts WithDynamicResources
// renders a bootstrap whose dynamic_resources block points lds/cds at
// the mounted xds directory and which carries no static_resources.
func (t *Tests) DynamicResourcesRendersDynamicBootstrap(ctx context.Context) error {
	xds := dag.Directory().
		WithNewFile("lds.yaml", "resources: []\n").
		WithNewFile("cds.yaml", "resources: []\n")
	contents, err := dag.Envoy().Proxy().
		WithDynamicResources(xds).
		ConfigFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("ConfigFile().Contents: %w", err)
	}
	var cfg struct {
		StaticResources map[string]any `yaml:"static_resources"`
		Admin           struct {
			Address struct {
				SocketAddress struct {
					PortValue int `yaml:"port_value"`
				} `yaml:"socket_address"`
			} `yaml:"address"`
		} `yaml:"admin"`
		DynamicResources struct {
			LdsConfig struct {
				PathConfigSource struct {
					Path string `yaml:"path"`
				} `yaml:"path_config_source"`
				ResourceAPIVersion string `yaml:"resource_api_version"`
			} `yaml:"lds_config"`
			CdsConfig struct {
				PathConfigSource struct {
					Path string `yaml:"path"`
				} `yaml:"path_config_source"`
				ResourceAPIVersion string `yaml:"resource_api_version"`
			} `yaml:"cds_config"`
		} `yaml:"dynamic_resources"`
	}
	if err := yaml.Unmarshal([]byte(contents), &cfg); err != nil {
		return fmt.Errorf("parse bootstrap yaml: %w\n---\n%s", err, contents)
	}
	if cfg.StaticResources != nil {
		return fmt.Errorf("expected no static_resources in dynamic bootstrap, got %v\n---\n%s", cfg.StaticResources, contents)
	}
	if got := cfg.Admin.Address.SocketAddress.PortValue; got != defaultAdmin {
		return fmt.Errorf("expected admin port %d, got %d\n---\n%s", defaultAdmin, got, contents)
	}
	dyn := cfg.DynamicResources
	if got := dyn.LdsConfig.PathConfigSource.Path; got != "/etc/envoy/xds/lds.yaml" {
		return fmt.Errorf("expected lds_config path /etc/envoy/xds/lds.yaml, got %q\n---\n%s", got, contents)
	}
	if got := dyn.CdsConfig.PathConfigSource.Path; got != "/etc/envoy/xds/cds.yaml" {
		return fmt.Errorf("expected cds_config path /etc/envoy/xds/cds.yaml, got %q\n---\n%s", got, contents)
	}
	for name, got := range map[string]string{
		"lds_config": dyn.LdsConfig.ResourceAPIVersion,
		"cds_config": dyn.CdsConfig.ResourceAPIVersion,
	} {
		if got != "V3" {
			return fmt.Errorf("expected %s.resource_api_version == V3, got %q\n---\n%s", name, got, contents)
		}
	}
	return nil
}

// L7HttpXdsRoundTrip stands up an HTTP upstream behind an Envoy
// proxy whose listeners and clusters arrive over file-based xDS
// rather than static_resources, and asserts a request through Envoy
// returns a fresh random marker served by the upstream.
func (t *Tests) L7HttpXdsRoundTrip(
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
	resources := e.XdsResources().
		WithListener(e.HTTPListener("http", 18080, hcm)).
		WithCluster(e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5678)))

	svc := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("upstream", upstream).
		WithDynamicResources(resources.Directory()).
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
		return fmt.Errorf("L7 xDS round-trip: %w", err)
	}
	return nil
}

// L4TcpXdsRoundTrip stands up an alpine `nc` echo upstream behind an
// Envoy TcpListener delivered over file-based xDS and asserts bytes
// sent through Envoy come back on the same TCP connection. Also
// asserts ListenerEndpoint resolves the listener's port out of the
// mounted lds.yaml, since no Listener is registered on the Proxy in
// this mode.
func (t *Tests) L4TcpXdsRoundTrip(
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
	resources := e.XdsResources().
		WithListener(e.TCPListener("ingress", 14000, e.TCPProxy("tcp", "upstream"))).
		WithCluster(e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5000)))

	proxy := e.Proxy(proxyOpts(envoyTag)).
		WithServiceBinding("upstream", upstream).
		WithDynamicResources(resources.Directory())

	endpoint, err := proxy.ListenerEndpoint(ctx, "ingress")
	if err != nil {
		return fmt.Errorf("ListenerEndpoint(ingress): %w", err)
	}
	if !strings.HasSuffix(endpoint, ":14000") {
		return fmt.Errorf("expected ListenerEndpoint(ingress) to end in :14000, got %q", endpoint)
	}

	_, err = dag.Container().From(probeImage).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-extras"}).
		WithServiceBinding("envoy", proxy.Service()).
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
		return fmt.Errorf("L4 xDS round-trip: %w", err)
	}
	return nil
}

// XdsResourcesMatchStaticResources asserts that the lds.yaml /
// cds.yaml discovery responses rendered by XdsResources.Directory()
// are structurally equivalent to the static_resources block the same
// components produce in static mode — identical apart from the
// per-resource `@type` discriminator that only the xDS shape needs.
func (t *Tests) XdsResourcesMatchStaticResources(ctx context.Context) error {
	e := dag.Envoy()
	rc := e.RouteConfig("rc").WithVirtualHost(
		e.VirtualHost("vh", []string{"*"}).WithRoute(e.RoutePrefix("/", "upstream")),
	)
	hcm := e.HTTPConnectionManager("ingress", rc).WithHTTPFilter(e.RouterHTTPFilter())
	listener := e.HTTPListener("http", 18080, hcm)
	cluster := e.Cluster("upstream").WithEndpoint(e.Endpoint("upstream", 5678))

	static, err := e.Proxy().
		WithListener(listener).
		WithCluster(cluster).
		ConfigFile().
		Contents(ctx)
	if err != nil {
		return fmt.Errorf("static ConfigFile().Contents: %w", err)
	}
	var staticCfg struct {
		StaticResources struct {
			Listeners []map[string]any `yaml:"listeners"`
			Clusters  []map[string]any `yaml:"clusters"`
		} `yaml:"static_resources"`
	}
	if err := yaml.Unmarshal([]byte(static), &staticCfg); err != nil {
		return fmt.Errorf("parse static bootstrap: %w\n---\n%s", err, static)
	}

	dir := e.XdsResources().
		WithListener(listener).
		WithCluster(cluster).
		Directory()

	for _, tc := range []struct {
		file    string
		typeURL string
		want    []map[string]any
	}{
		{"lds.yaml", "type.googleapis.com/envoy.config.listener.v3.Listener", staticCfg.StaticResources.Listeners},
		{"cds.yaml", "type.googleapis.com/envoy.config.cluster.v3.Cluster", staticCfg.StaticResources.Clusters},
	} {
		contents, err := dir.File(tc.file).Contents(ctx)
		if err != nil {
			return fmt.Errorf("%s contents: %w", tc.file, err)
		}
		var resp struct {
			Resources []map[string]any `yaml:"resources"`
		}
		if err := yaml.Unmarshal([]byte(contents), &resp); err != nil {
			return fmt.Errorf("parse %s: %w\n---\n%s", tc.file, err, contents)
		}
		if len(resp.Resources) != len(tc.want) {
			return fmt.Errorf("%s: expected %d resources, got %d\n---\n%s", tc.file, len(tc.want), len(resp.Resources), contents)
		}
		for i, got := range resp.Resources {
			if got["@type"] != tc.typeURL {
				return fmt.Errorf("%s: resources[%d].@type == %v, want %q", tc.file, i, got["@type"], tc.typeURL)
			}
			stripped := make(map[string]any, len(got))
			for k, v := range got {
				if k != "@type" {
					stripped[k] = v
				}
			}
			gotYAML, err := yaml.Marshal(stripped)
			if err != nil {
				return fmt.Errorf("%s: re-marshal resources[%d]: %w", tc.file, i, err)
			}
			wantYAML, err := yaml.Marshal(tc.want[i])
			if err != nil {
				return fmt.Errorf("%s: re-marshal static[%d]: %w", tc.file, i, err)
			}
			if string(gotYAML) != string(wantYAML) {
				return fmt.Errorf("%s: resources[%d] differs from static equivalent:\n--- xds ---\n%s\n--- static ---\n%s", tc.file, i, gotYAML, wantYAML)
			}
		}
	}
	return nil
}

// XdsResourcesRejectsInvalidResourceSet asserts Directory() surfaces
// the same two errors (*Proxy).ConfigFile() does: two listeners
// sharing a name, and a listener whose filter chain references a
// cluster that isn't in the resource set.
func (t *Tests) XdsResourcesRejectsInvalidResourceSet(ctx context.Context) error {
	e := dag.Envoy()

	dup := e.CustomListener("dup", "address: { socket_address: { address: 0.0.0.0, port_value: 18080 } }\nfilter_chains: []\n")
	if _, err := e.XdsResources().
		WithListener(dup).
		WithListener(dup).
		Directory().
		ID(ctx); err == nil {
		return fmt.Errorf("duplicate listener name: expected Directory() error, got nil")
	}

	dangling := e.TCPListener("ingress", 14000, e.TCPProxy("tcp", "missing"))
	if _, err := e.XdsResources().
		WithListener(dangling).
		Directory().
		ID(ctx); err == nil {
		return fmt.Errorf("unknown cluster reference: expected Directory() error, got nil")
	}

	// Same listener with its cluster registered must render cleanly.
	if _, err := e.XdsResources().
		WithListener(e.TCPListener("ingress", 14000, e.TCPProxy("tcp", "upstream"))).
		WithCluster(e.Cluster("upstream")).
		Directory().
		ID(ctx); err != nil {
		return fmt.Errorf("valid resource set: expected nil, got %w", err)
	}
	return nil
}

// XdsResourcesRejectsSecureComponents asserts Directory() refuses
// TLS / mTLS listeners and clusters: their rendered resources point
// at key material under /etc/envoy/secrets that only the static
// Service() path mounts, so a proxy fed them through an opaque
// resource directory would boot and then fail every handshake.
func (t *Tests) XdsResourcesRejectsSecureComponents(ctx context.Context) error {
	e := dag.Envoy()

	ca, caPwd, err := testCa(ctx, "xds-tls")
	if err != nil {
		return err
	}
	tlsListener := minimalHttpListener("https", 18443, e.TLSServerSecurity(ca.KeyStore().Pkcs12(), caPwd))
	if _, err := e.XdsResources().
		WithListener(tlsListener).
		WithCluster(e.Cluster("upstream")).
		Directory().
		ID(ctx); err == nil {
		return fmt.Errorf("TLS listener: expected Directory() error, got nil")
	}

	upstreamCa, upstreamPwd, err := testCa(ctx, "xds-upstream-tls")
	if err != nil {
		return err
	}
	tlsCluster := e.Cluster("upstream", dagger.EnvoyClusterOpts{
		Upstream: e.TLSUpstreamSecurity(upstreamCa.TrustStore().Pkcs12(), upstreamPwd),
	})
	if _, err := e.XdsResources().
		WithCluster(tlsCluster).
		Directory().
		ID(ctx); err == nil {
		return fmt.Errorf("TLS cluster: expected Directory() error, got nil")
	}
	return nil
}

// DynamicResourcesRequiresLdsAndCds asserts Service() rejects a
// resource directory missing either of the two files the bootstrap's
// dynamic_resources block points at, rather than booting an Envoy
// that silently discovers nothing.
func (t *Tests) DynamicResourcesRequiresLdsAndCds(ctx context.Context) error {
	for _, tc := range []struct {
		name string
		dir  *dagger.Directory
	}{
		{"missing cds.yaml", dag.Directory().WithNewFile("lds.yaml", "resources: []\n")},
		{"missing lds.yaml", dag.Directory().WithNewFile("cds.yaml", "resources: []\n")},
		{"empty", dag.Directory()},
	} {
		if _, err := dag.Envoy().Proxy().
			WithDynamicResources(tc.dir).
			Service().
			ID(ctx); err == nil {
			return fmt.Errorf("%s: expected Service() error, got nil", tc.name)
		}
	}
	return nil
}

// DynamicResourcesConflictsWithStatic asserts that mixing
// WithDynamicResources with any of WithListener / WithCluster /
// WithConfigFile on the same Proxy makes ConfigFile() return a
// non-nil error — the two configuration modes are exclusive.
func (t *Tests) DynamicResourcesConflictsWithStatic(ctx context.Context) error {
	e := dag.Envoy()
	xds := dag.Directory().
		WithNewFile("lds.yaml", "resources: []\n").
		WithNewFile("cds.yaml", "resources: []\n")

	listener := e.CustomListener("manual", "address: { socket_address: { address: 0.0.0.0, port_value: 18080 } }\nfilter_chains: []\n")
	if _, err := e.Proxy().
		WithDynamicResources(xds).
		WithListener(listener).
		ConfigFile().
		Contents(ctx); err == nil {
		return fmt.Errorf("WithDynamicResources + WithListener: expected ConfigFile() error, got nil")
	}

	if _, err := e.Proxy().
		WithDynamicResources(xds).
		WithCluster(e.Cluster("upstream")).
		ConfigFile().
		Contents(ctx); err == nil {
		return fmt.Errorf("WithDynamicResources + WithCluster: expected ConfigFile() error, got nil")
	}

	override := dag.Directory().
		WithNewFile("override.yaml", "admin: {}\n").
		File("override.yaml")
	if _, err := e.Proxy().
		WithDynamicResources(xds).
		WithConfigFile(override).
		ConfigFile().
		Contents(ctx); err == nil {
		return fmt.Errorf("WithDynamicResources + WithConfigFile: expected ConfigFile() error, got nil")
	}

	// The same directory on its own must still render cleanly.
	if _, err := e.Proxy().WithDynamicResources(xds).ConfigFile().Contents(ctx); err != nil {
		return fmt.Errorf("WithDynamicResources alone: expected nil, got %w", err)
	}
	return nil
}
