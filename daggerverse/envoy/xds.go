package main

import (
	"context"
	"fmt"
	"path/filepath"

	"dagger/envoy/internal/dagger"

	"gopkg.in/yaml.v3"
)

// xdsMountPath is where a Proxy's dynamic-resource directory is
// mounted inside the running envoy container. The bootstrap's
// dynamic_resources block points its lds/cds config sources at files
// under this directory, so the paths are part of the module's
// contract with callers who bring their own discovery-resource
// directory.
const xdsMountPath = "/etc/envoy/xds"

// The two discovery-response files the bootstrap's dynamic_resources
// block subscribes to. An rds.yaml alongside them is loaded by Envoy
// only if a listener resource names it in an `rds` config source; the
// bootstrap never points at it directly, and the v1 builders — whose
// HttpConnectionManagers always carry an inline route_config — never
// emit one.
const (
	xdsLdsFile = "lds.yaml"
	xdsCdsFile = "cds.yaml"
)

// Discovery-response resource type URLs. Each resource in a
// file-based xDS discovery response carries its own `@type` so Envoy
// can decode the google.protobuf.Any.
const (
	listenerTypeURL = "type.googleapis.com/envoy.config.listener.v3.Listener"
	clusterTypeURL  = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
)

func xdsPath(name string) string {
	return filepath.Join(xdsMountPath, name)
}

// WithDynamicResources switches the proxy from a rendered
// static_resources bootstrap to file-based xDS: the bootstrap's
// dynamic_resources block points lds_config at
// /etc/envoy/xds/lds.yaml and cds_config at /etc/envoy/xds/cds.yaml,
// and dir is mounted at /etc/envoy/xds. dir must contain at minimum
// lds.yaml and cds.yaml, each a v3 discovery response; rds.yaml is
// loaded only when a listener's HttpConnectionManager references it
// via an `rds` config source (the bootstrap does not wire RDS
// itself).
//
// This mode is exclusive with WithListener / WithCluster /
// WithConfigFile: calling both makes ConfigFile() (and therefore
// Service()) return a non-nil error.
//
// Hot reload is NOT exercised. Dagger's WithMountedDirectory is
// content-addressed and immutable at mount time, so the mounted
// resource files never change for the lifetime of the container —
// only Envoy's initial discovery path runs. Callers wanting to test
// reload semantics need to launch a fresh Proxy per resource
// snapshot.
func (p *Proxy) WithDynamicResources(dir *dagger.Directory) *Proxy {
	out := *p
	out.DynamicResources = dir
	return &out
}

// validateDynamicProxy enforces that file-based xDS and the static
// builders are never mixed on the same Proxy. Silently preferring one
// over the other would hand callers a proxy that ignores half of what
// they configured.
func validateDynamicProxy(p *Proxy) error {
	if p.Override != nil {
		return fmt.Errorf("proxy: WithDynamicResources and WithConfigFile are exclusive")
	}
	if len(p.Listeners) > 0 {
		return fmt.Errorf("proxy: WithDynamicResources and WithListener are exclusive (supply listeners via the mounted %s directory)", xdsLdsFile)
	}
	if len(p.Clusters) > 0 {
		return fmt.Errorf("proxy: WithDynamicResources and WithCluster are exclusive (supply clusters via the mounted %s directory)", xdsCdsFile)
	}
	return nil
}

// xdsNodeID and xdsNodeCluster identify this Envoy to a management
// server. Envoy refuses to initialize a dynamic_resources config
// without them ("node 'id' and 'cluster' are required"), even for a
// filesystem subscription that has no management server to identify
// itself to. Fixed values keep the rendered bootstrap deterministic;
// nothing in file-based xDS keys off them.
const (
	xdsNodeID      = "envoy"
	xdsNodeCluster = "envoy"
)

// renderDynamicBootstrap composes the file-based xDS bootstrap: a
// node identity, an admin listener, and a dynamic_resources block
// whose lds/cds config sources are filesystem paths under the mounted
// xds directory.
func renderDynamicBootstrap(adminPort int) ([]byte, error) {
	root := map[string]any{
		"node": map[string]any{
			"id":      xdsNodeID,
			"cluster": xdsNodeCluster,
		},
		"admin": map[string]any{
			"address": map[string]any{
				"socket_address": map[string]any{
					"address":    "0.0.0.0",
					"port_value": adminPort,
				},
			},
		},
		"dynamic_resources": map[string]any{
			"lds_config": pathConfigSource(xdsPath(xdsLdsFile)),
			"cds_config": pathConfigSource(xdsPath(xdsCdsFile)),
		},
	}
	return yaml.Marshal(root)
}

// XdsResources composes the same v1 component types the static
// builders take (Listener, Cluster) into the discovery-resource files
// a file-based xDS Proxy consumes. Composition mirrors Proxy: WithX
// methods return shallow copies.
type XdsResources struct {
	Listeners []*Listener
	Clusters  []*Cluster
}

// XdsResources returns an empty discovery-resource set. Feed it
// listeners and clusters, then hand Directory() to
// (*Proxy).WithDynamicResources.
func (e *Envoy) XdsResources() *XdsResources {
	return &XdsResources{}
}

// WithListener appends a listener to the LDS resource set.
func (x *XdsResources) WithListener(l *Listener) *XdsResources {
	out := *x
	out.Listeners = append(append([]*Listener{}, x.Listeners...), l)
	return &out
}

// WithCluster appends a cluster to the CDS resource set.
func (x *XdsResources) WithCluster(c *Cluster) *XdsResources {
	out := *x
	out.Clusters = append(append([]*Cluster{}, x.Clusters...), c)
	return &out
}

// Directory renders lds.yaml and cds.yaml — v3 discovery responses
// carrying the registered listeners and clusters — into a directory
// suitable for (*Proxy).WithDynamicResources. Each resource is
// structurally identical to the static_resources entry the same
// component produces in static mode, plus the `@type` discriminator
// the discovery response requires.
//
// Route configurations stay inline in each listener's
// HttpConnectionManager, so no rds.yaml is emitted: the v1 builders
// have no way to express an RDS config source. Callers who need RDS
// bring their own directory containing rds.yaml plus listeners that
// reference it.
//
// Returns a non-nil error under the same conditions as
// (*Proxy).ConfigFile() — a listener referencing a cluster that isn't
// registered here, or two listeners sharing a name — and also when
// any component carries TLS / mTLS security, whose key material only
// (*Proxy).Service() can mount and which the Proxy cannot see through
// an opaque resource directory.
func (x *XdsResources) Directory() (*dagger.Directory, error) {
	if err := validateResources(x.Listeners, x.Clusters, "resource set", "WithCluster"); err != nil {
		return nil, err
	}
	if err := validateXdsPlaintext(x); err != nil {
		return nil, err
	}
	lds, err := renderDiscoveryResponse(listenerTypeURL, func() ([]map[string]any, error) {
		out := make([]map[string]any, 0, len(x.Listeners))
		for _, l := range x.Listeners {
			res, err := renderListener(l)
			if err != nil {
				return nil, err
			}
			out = append(out, res)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	cds, err := renderDiscoveryResponse(clusterTypeURL, func() ([]map[string]any, error) {
		out := make([]map[string]any, 0, len(x.Clusters))
		for _, c := range x.Clusters {
			out = append(out, renderCluster(c))
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return dag.Directory().
		WithNewFile(xdsLdsFile, string(lds)).
		WithNewFile(xdsCdsFile, string(cds)), nil
}

// validateXdsPlaintext rejects TLS / mTLS components. A rendered
// resource for a secured listener or cluster points at certificate
// and key files under /etc/envoy/secrets, but only the static path
// mounts them: WithDynamicResources hands the Proxy an opaque
// directory it cannot introspect for the material to mount. Failing
// here beats a proxy that boots and then fails every handshake on a
// missing file.
func validateXdsPlaintext(x *XdsResources) error {
	for _, l := range x.Listeners {
		if l.SecurityMode == "TLS" || l.SecurityMode == "MTLS" {
			return fmt.Errorf("listener %q: %s listeners are not supported in file-based xDS mode (the mounted resource directory carries no key material)", l.Name, l.SecurityMode)
		}
	}
	for _, c := range x.Clusters {
		if c.UpstreamMode == "TLS" || c.UpstreamMode == "MTLS" {
			return fmt.Errorf("cluster %q: %s upstreams are not supported in file-based xDS mode (the mounted resource directory carries no key material)", c.Name, c.UpstreamMode)
		}
	}
	return nil
}

// renderDiscoveryResponse marshals resources into the DiscoveryResponse
// shape Envoy's filesystem subscription expects, stamping each entry
// with typeURL.
func renderDiscoveryResponse(typeURL string, resources func() ([]map[string]any, error)) ([]byte, error) {
	items, err := resources()
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		entry := map[string]any{"@type": typeURL}
		for k, v := range item {
			entry[k] = v
		}
		out = append(out, entry)
	}
	return yaml.Marshal(map[string]any{
		"version_info": "1",
		"resources":    out,
	})
}

// pathConfigSource builds the non-deprecated filesystem ConfigSource
// shape. The bare `path` field Envoy accepted historically is
// deprecated in favour of path_config_source.
func pathConfigSource(path string) map[string]any {
	return map[string]any{
		"path_config_source": map[string]any{
			"path": path,
		},
		"resource_api_version": "V3",
	}
}

// xdsListenerPorts reads the ports Envoy will bind from the LDS
// discovery response in dir. In static mode the ports to expose come
// from the registered Listener values; in dynamic mode the resource
// directory is the only place they exist, so Service() has to read it
// to know which container ports to open. Also doubles as the
// arrival check for the two files the bootstrap points at.
func xdsListenerPorts(ctx context.Context, dir *dagger.Directory) ([]int, error) {
	entries, err := dir.Entries(ctx)
	if err != nil {
		return nil, fmt.Errorf("dynamic resources: read directory: %w", err)
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e] = true
	}
	for _, required := range []string{xdsLdsFile, xdsCdsFile} {
		if !present[required] {
			return nil, fmt.Errorf("dynamic resources: directory must contain %s (got %v)", required, entries)
		}
	}
	contents, err := dir.File(xdsLdsFile).Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("dynamic resources: read %s: %w", xdsLdsFile, err)
	}
	var resp struct {
		Resources []map[string]any `yaml:"resources"`
	}
	if err := yaml.Unmarshal([]byte(contents), &resp); err != nil {
		return nil, fmt.Errorf("dynamic resources: parse %s: %w", xdsLdsFile, err)
	}
	seen := map[int]bool{}
	var ports []int
	for _, res := range resp.Resources {
		body, err := yaml.Marshal(res)
		if err != nil {
			return nil, fmt.Errorf("dynamic resources: re-marshal %s resource: %w", xdsLdsFile, err)
		}
		port, ok := extractListenerPort(string(body))
		if !ok || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	return ports, nil
}

// xdsListenerPort resolves a single named listener's port from the
// LDS discovery response in dir, so ListenerEndpoint works the same
// way in dynamic mode as it does against registered Listeners.
func xdsListenerPort(ctx context.Context, dir *dagger.Directory, name string) (int, error) {
	contents, err := dir.File(xdsLdsFile).Contents(ctx)
	if err != nil {
		return 0, fmt.Errorf("dynamic resources: read %s: %w", xdsLdsFile, err)
	}
	var resp struct {
		Resources []map[string]any `yaml:"resources"`
	}
	if err := yaml.Unmarshal([]byte(contents), &resp); err != nil {
		return 0, fmt.Errorf("dynamic resources: parse %s: %w", xdsLdsFile, err)
	}
	for _, res := range resp.Resources {
		if res["name"] != name {
			continue
		}
		body, err := yaml.Marshal(res)
		if err != nil {
			return 0, fmt.Errorf("dynamic resources: re-marshal %s resource: %w", xdsLdsFile, err)
		}
		port, ok := extractListenerPort(string(body))
		if !ok {
			return 0, fmt.Errorf("listener %q: cannot extract socket_address.port_value from %s", name, xdsLdsFile)
		}
		return port, nil
	}
	return 0, fmt.Errorf("listener %q: not present in %s", name, xdsLdsFile)
}
