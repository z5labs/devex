// Package main is the envoy Dagger module: spins up the Envoy proxy
// (envoyproxy/envoy) as a service for local development and testing,
// with a builder API for composing L7 (HTTP) and L4 (TCP) listeners
// and their referenced clusters into a static-resources Envoy
// bootstrap without writing the YAML by hand. TLS / mTLS terminates
// on listeners (downstream) and authenticates clusters (upstream)
// via per-listener / per-cluster *ServerSecurity / *UpstreamSecurity
// profiles. Dynamic configuration (xDS) lands in a separate
// follow-up.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"dagger/envoy/internal/dagger"

	"gopkg.in/yaml.v3"
)

const (
	envoyImagePath   = "envoyproxy/envoy"
	defaultRegistry  = "docker.io"
	defaultTag       = "v1.32.1"
	defaultAdminPort = 9901
	configMountPath  = "/etc/envoy/envoy.yaml"
)

// Envoy is the top-level builder type. All component factories hang
// off of it.
type Envoy struct{}

// nameRe matches the component names and Custom* kinds the bootstrap
// permits without YAML-quoting hazards.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateName(field, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%s %q: must match [A-Za-z0-9_-]+", field, name)
	}
	return nil
}

func validatePort(field string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s %d: must be in 1..65535", field, port)
	}
	return nil
}

// validClusterTypes is the allow-list of Envoy cluster discovery
// types this module renders. Anything else returns a non-nil error
// from Cluster() at factory time so unknown values can't reach the
// rendered bootstrap.
var validClusterTypes = map[string]bool{
	"STATIC":      true,
	"STRICT_DNS":  true,
	"LOGICAL_DNS": true,
}

func validateClusterType(t string) error {
	if !validClusterTypes[t] {
		return fmt.Errorf("clusterType %q: must be one of STATIC, STRICT_DNS, LOGICAL_DNS", t)
	}
	return nil
}

// Endpoint is a single upstream address (host + port) that a Cluster
// resolves to.
type Endpoint struct {
	Host string
	Port int
}

// Endpoint builds an Endpoint, validating host (non-empty) and port
// (1..65535).
func (e *Envoy) Endpoint(host string, port int) (*Endpoint, error) {
	if host == "" {
		return nil, fmt.Errorf("host: must be non-empty")
	}
	if err := validatePort("port", port); err != nil {
		return nil, err
	}
	return &Endpoint{Host: host, Port: port}, nil
}

// Cluster is a named upstream cluster of Endpoints, optionally
// configured with TLS / mTLS to the upstream side.
type Cluster struct {
	Name      string
	Kind      string
	Endpoints []*Endpoint

	// +private
	UpstreamMode string // "" | "TLS" | "MTLS" — drives transport_socket rendering and Service() mounts
	// +private
	UpstreamTrustPem *dagger.File // PEM of the upstream TrustStore's CA cert(s) (TLS + MTLS)
	// +private
	UpstreamLeafCertPem *dagger.File // PEM cert chain of Envoy's client leaf for the upstream (MTLS only)
	// +private
	UpstreamLeafKeyPem *dagger.File // PEM PKCS#1 ("RSA PRIVATE KEY") private key for the upstream client leaf (MTLS only)
}

// Cluster builds a Cluster optionally configured with upstream
// TLS / mTLS via an UpstreamSecurity profile. clusterType defaults
// to "STRICT_DNS"; unknown values return a non-nil error. A nil or
// plaintext upstream produces the same plaintext cluster as before.
func (e *Envoy) Cluster(
	ctx context.Context,
	name string,
	// +default="STRICT_DNS"
	clusterType string,
	// +optional
	upstream *UpstreamSecurity,
) (*Cluster, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	if err := validateClusterType(clusterType); err != nil {
		return nil, err
	}
	c := &Cluster{Name: name, Kind: clusterType}
	mode := effectiveUpstreamMode(upstream)
	if mode == "TLS" || mode == "MTLS" {
		if upstream.TrustStore == nil || upstream.TrustStorePassword == nil {
			return nil, fmt.Errorf("cluster %q: %s upstream requires TrustStore + TrustStorePassword", name, mode)
		}
		trustPem, err := extractCaPemFromPkcs12(ctx, "upstream-"+name+"-trust", upstream.TrustStore, upstream.TrustStorePassword)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: %w", name, err)
		}
		c.UpstreamMode = mode
		c.UpstreamTrustPem = trustPem
	}
	if mode == "MTLS" {
		if upstream.KeyStore == nil || upstream.KeyStorePassword == nil {
			return nil, fmt.Errorf("cluster %q: MTLS upstream requires KeyStore + KeyStorePassword", name)
		}
		certPem, keyPem, err := extractUpstreamLeafPemFromPkcs12(ctx, "upstream-"+name+"-leaf", upstream.KeyStore, upstream.KeyStorePassword)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: %w", name, err)
		}
		c.UpstreamLeafCertPem = certPem
		c.UpstreamLeafKeyPem = keyPem
	}
	return c, nil
}

// WithEndpoint appends an Endpoint to the cluster and returns a new
// cluster value.
func (c *Cluster) WithEndpoint(ep *Endpoint) *Cluster {
	out := *c
	out.Endpoints = append(append([]*Endpoint{}, c.Endpoints...), ep)
	return &out
}

// Route is a single HTTP route (prefix match → cluster).
type Route struct {
	Prefix  string
	Cluster string
}

// RoutePrefix builds a route matching paths with the given prefix and
// forwarding to cluster.
func (e *Envoy) RoutePrefix(prefix, cluster string) (*Route, error) {
	if prefix == "" {
		return nil, fmt.Errorf("prefix: must be non-empty")
	}
	if err := validateName("cluster", cluster); err != nil {
		return nil, err
	}
	return &Route{Prefix: prefix, Cluster: cluster}, nil
}

// VirtualHost is a named virtual host with a domain list and an
// ordered set of routes.
type VirtualHost struct {
	Name    string
	Domains []string
	Routes  []*Route
}

// VirtualHost builds an empty VirtualHost.
func (e *Envoy) VirtualHost(name string, domains []string) (*VirtualHost, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("domains: must be non-empty")
	}
	for i, d := range domains {
		if d == "" {
			return nil, fmt.Errorf("domains[%d]: must be non-empty", i)
		}
	}
	return &VirtualHost{Name: name, Domains: append([]string{}, domains...)}, nil
}

// WithRoute appends a route to the virtual host.
func (v *VirtualHost) WithRoute(route *Route) *VirtualHost {
	out := *v
	out.Routes = append(append([]*Route{}, v.Routes...), route)
	return &out
}

// RouteConfig is a named route configuration (a set of virtual hosts).
type RouteConfig struct {
	Name         string
	VirtualHosts []*VirtualHost
}

// RouteConfig builds an empty RouteConfig.
func (e *Envoy) RouteConfig(name string) (*RouteConfig, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	return &RouteConfig{Name: name}, nil
}

// WithVirtualHost appends a VirtualHost to the route config.
func (rc *RouteConfig) WithVirtualHost(v *VirtualHost) *RouteConfig {
	out := *rc
	out.VirtualHosts = append(append([]*VirtualHost{}, rc.VirtualHosts...), v)
	return &out
}

// HttpFilter is a single HTTP filter that participates in the
// HttpConnectionManager's filter chain. Body is the YAML body of the
// filter's `typed_config` map; for the terminal router filter Body
// is empty (the typed_config has a fixed shape and is filled in at
// render time).
type HttpFilter struct {
	Name string
	Body string
}

const (
	routerFilterName    = "envoy.filters.http.router"
	routerFilterTypeURL = "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"
	hcmFilterName       = "envoy.filters.network.http_connection_manager"
	hcmFilterTypeURL    = "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager"
)

// RouterHttpFilter returns the terminal envoy.filters.http.router
// filter. Per the Envoy contract this must be the last filter in the
// http_filters chain; the builder does NOT enforce ordering — Envoy
// rejects the bootstrap at startup if violated.
func (e *Envoy) RouterHttpFilter() *HttpFilter {
	return &HttpFilter{Name: routerFilterName}
}

// CustomHttpFilter builds an HTTP filter whose typed_config body is
// the caller-supplied YAML, spliced verbatim. yamlBody is validated
// via yaml.Unmarshal at construction time.
func (e *Envoy) CustomHttpFilter(name, yamlBody string) (*HttpFilter, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	var body map[string]any
	if err := yaml.Unmarshal([]byte(yamlBody), &body); err != nil {
		return nil, fmt.Errorf("custom http filter %q: invalid YAML body: %w", name, err)
	}
	if body == nil {
		return nil, fmt.Errorf("custom http filter %q: YAML body must be a mapping", name)
	}
	return &HttpFilter{Name: name, Body: yamlBody}, nil
}

// HttpConnectionManager is the L7 network filter that decodes HTTP
// frames, applies an ordered filter chain, and dispatches to a
// route_config.
type HttpConnectionManager struct {
	StatPrefix  string
	RouteConfig *RouteConfig
	HttpFilters []*HttpFilter
}

// HttpConnectionManager builds an HCM bound to routeConfig.
func (e *Envoy) HttpConnectionManager(statPrefix string, routeConfig *RouteConfig) (*HttpConnectionManager, error) {
	if statPrefix == "" {
		return nil, fmt.Errorf("statPrefix: must be non-empty")
	}
	if routeConfig == nil {
		return nil, fmt.Errorf("routeConfig: must be non-nil")
	}
	return &HttpConnectionManager{StatPrefix: statPrefix, RouteConfig: routeConfig}, nil
}

// WithHttpFilter appends an HTTP filter to the HCM.
func (h *HttpConnectionManager) WithHttpFilter(f *HttpFilter) *HttpConnectionManager {
	out := *h
	out.HttpFilters = append(append([]*HttpFilter{}, h.HttpFilters...), f)
	return &out
}

// HttpListener builds an L7 listener bound at address:port whose
// filter chain delegates to hcm. A nil or plaintext security profile
// renders the existing plaintext listener; TLS / MTLS mints a
// per-listener server leaf certificate from the supplied CA at
// factory time and embeds the transport_socket block in the filter
// chain.
//
// +cache="session"
func (e *Envoy) HttpListener(
	ctx context.Context,
	name string,
	// +default="0.0.0.0"
	address string,
	port int,
	hcm *HttpConnectionManager,
	// +optional
	security *ServerSecurity,
) (*Listener, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	if address == "" {
		return nil, fmt.Errorf("address: must be non-empty")
	}
	if err := validatePort("port", port); err != nil {
		return nil, err
	}
	if hcm == nil {
		return nil, fmt.Errorf("hcm: must be non-nil")
	}
	hcmTyped, err := renderHttpConnectionManager(hcm)
	if err != nil {
		return nil, fmt.Errorf("http listener %q: %w", name, err)
	}
	mode := effectiveServerMode(security)
	fc0 := map[string]any{
		"filters": []any{
			map[string]any{
				"name":         hcmFilterName,
				"typed_config": hcmTyped,
			},
		},
	}
	if ts := renderDownstreamTransportSocket(name, mode); ts != nil {
		fc0["transport_socket"] = ts
	}
	body := map[string]any{
		"address": map[string]any{
			"socket_address": map[string]any{
				"address":    address,
				"port_value": port,
			},
		},
		"filter_chains": []any{fc0},
	}
	bodyYAML, err := yaml.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("http listener %q: marshal body: %w", name, err)
	}
	l := &Listener{
		Name:        name,
		Body:        string(bodyYAML),
		ClusterRefs: collectHcmClusterRefs(hcm),
	}
	if err := bakeListenerSecurity(ctx, l, name, security); err != nil {
		return nil, fmt.Errorf("http listener %q: %w", name, err)
	}
	return l, nil
}

func renderHttpConnectionManager(h *HttpConnectionManager) (map[string]any, error) {
	out := map[string]any{
		"@type":        hcmFilterTypeURL,
		"stat_prefix":  h.StatPrefix,
		"route_config": renderRouteConfig(h.RouteConfig),
	}
	if len(h.HttpFilters) > 0 {
		filters := make([]any, 0, len(h.HttpFilters))
		for _, f := range h.HttpFilters {
			entry, err := renderHttpFilter(f)
			if err != nil {
				return nil, err
			}
			filters = append(filters, entry)
		}
		out["http_filters"] = filters
	}
	return out, nil
}

func renderHttpFilter(f *HttpFilter) (map[string]any, error) {
	entry := map[string]any{"name": f.Name}
	switch f.Name {
	case routerFilterName:
		entry["typed_config"] = map[string]any{"@type": routerFilterTypeURL}
		return entry, nil
	}
	var typedConfig map[string]any
	if f.Body != "" {
		if err := yaml.Unmarshal([]byte(f.Body), &typedConfig); err != nil {
			return nil, fmt.Errorf("http filter %q: parse body: %w", f.Name, err)
		}
	}
	if typedConfig == nil {
		typedConfig = map[string]any{}
	}
	entry["typed_config"] = typedConfig
	return entry, nil
}

func renderRouteConfig(rc *RouteConfig) map[string]any {
	out := map[string]any{"name": rc.Name}
	if len(rc.VirtualHosts) > 0 {
		vhs := make([]any, 0, len(rc.VirtualHosts))
		for _, v := range rc.VirtualHosts {
			vhs = append(vhs, renderVirtualHost(v))
		}
		out["virtual_hosts"] = vhs
	}
	return out
}

func renderVirtualHost(v *VirtualHost) map[string]any {
	out := map[string]any{
		"name":    v.Name,
		"domains": v.Domains,
	}
	if len(v.Routes) > 0 {
		routes := make([]any, 0, len(v.Routes))
		for _, r := range v.Routes {
			routes = append(routes, map[string]any{
				"match": map[string]any{"prefix": r.Prefix},
				"route": map[string]any{"cluster": r.Cluster},
			})
		}
		out["routes"] = routes
	}
	return out
}

func collectHcmClusterRefs(h *HttpConnectionManager) []string {
	seen := map[string]bool{}
	var refs []string
	for _, v := range h.RouteConfig.VirtualHosts {
		for _, r := range v.Routes {
			if !seen[r.Cluster] {
				seen[r.Cluster] = true
				refs = append(refs, r.Cluster)
			}
		}
	}
	return refs
}

// TcpProxy is the network-level filter that forwards bytes from a
// TcpListener to a single cluster.
type TcpProxy struct {
	StatPrefix string
	Cluster    string
}

// TcpProxy builds a TcpProxy network filter targeting cluster.
func (e *Envoy) TcpProxy(statPrefix, cluster string) (*TcpProxy, error) {
	if statPrefix == "" {
		return nil, fmt.Errorf("statPrefix: must be non-empty")
	}
	if err := validateName("cluster", cluster); err != nil {
		return nil, err
	}
	return &TcpProxy{StatPrefix: statPrefix, Cluster: cluster}, nil
}

// TcpListener builds an L4 listener bound at address:port whose
// filter chain delegates to proxy. Accepts the same *ServerSecurity
// union as HttpListener — TLS / mTLS terminates on the listener and
// the resulting transport_socket lives on the filter chain alongside
// the tcp_proxy filter.
//
// +cache="session"
func (e *Envoy) TcpListener(
	ctx context.Context,
	name string,
	// +default="0.0.0.0"
	address string,
	port int,
	proxy *TcpProxy,
	// +optional
	security *ServerSecurity,
) (*Listener, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	if address == "" {
		return nil, fmt.Errorf("address: must be non-empty")
	}
	if err := validatePort("port", port); err != nil {
		return nil, err
	}
	if proxy == nil {
		return nil, fmt.Errorf("proxy: must be non-nil")
	}
	mode := effectiveServerMode(security)
	fc0 := map[string]any{
		"filters": []any{
			map[string]any{
				"name": "envoy.filters.network.tcp_proxy",
				"typed_config": map[string]any{
					"@type":       "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
					"stat_prefix": proxy.StatPrefix,
					"cluster":     proxy.Cluster,
				},
			},
		},
	}
	if ts := renderDownstreamTransportSocket(name, mode); ts != nil {
		fc0["transport_socket"] = ts
	}
	body := map[string]any{
		"address": map[string]any{
			"socket_address": map[string]any{
				"address":    address,
				"port_value": port,
			},
		},
		"filter_chains": []any{fc0},
	}
	bodyYAML, err := yaml.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("tcp listener %q: marshal body: %w", name, err)
	}
	l := &Listener{
		Name:        name,
		Body:        string(bodyYAML),
		ClusterRefs: []string{proxy.Cluster},
	}
	if err := bakeListenerSecurity(ctx, l, name, security); err != nil {
		return nil, fmt.Errorf("tcp listener %q: %w", name, err)
	}
	return l, nil
}

// Listener is a single Envoy listener — either typed
// (HttpListener/TcpListener) or opaque (CustomListener). Body is the
// YAML body for the listener excluding the top-level `name:` key,
// which is keyed in at render time from Name. ClusterRefs lists
// cluster names this listener references in its filter chain so
// (*Proxy).ConfigFile() can validate references against the
// registered cluster set; it is empty for CustomListener whose body
// is opaque.
type Listener struct {
	Name        string
	Body        string
	ClusterRefs []string

	// +private
	SecurityMode string // "" | "TLS" | "MTLS" — drives Service() mount fan-out
	// +private
	LeafCertPem *dagger.File // PEM cert chain of the per-listener server leaf (TLS + MTLS)
	// +private
	LeafKeyPem *dagger.File // PEM-encoded PKCS#1 ("RSA PRIVATE KEY") private key for the leaf (TLS + MTLS)
	// +private
	ClientTrustPem *dagger.File // PEM of the clientTrustStore's CA cert(s) (MTLS only)
}

// bakeListenerSecurity mints the per-listener server leaf certificate
// (TLS + MTLS) and extracts the client trust PEM (MTLS only),
// stashing them on the Listener for Service() to mount. No-op for
// PLAINTEXT mode.
func bakeListenerSecurity(ctx context.Context, l *Listener, name string, sec *ServerSecurity) error {
	mode := effectiveServerMode(sec)
	if mode == "PLAINTEXT" {
		return nil
	}
	certPem, keyPem, err := mintListenerLeaf(ctx, name, sec)
	if err != nil {
		return err
	}
	l.SecurityMode = mode
	l.LeafCertPem = certPem
	l.LeafKeyPem = keyPem
	if mode == "MTLS" {
		if sec.ClientTrustStore == nil || sec.ClientTrustStorePassword == nil {
			return fmt.Errorf("listener %q: MTLS requires ClientTrustStore + ClientTrustStorePassword", name)
		}
		trustPem, err := extractCaPemFromPkcs12(ctx, "listener-"+name+"-trust", sec.ClientTrustStore, sec.ClientTrustStorePassword)
		if err != nil {
			return err
		}
		l.ClientTrustPem = trustPem
	}
	return nil
}

// CustomListener builds a Listener whose body is the caller-supplied
// YAML, spliced verbatim under static_resources.listeners with `name`
// keyed in by the builder. The yamlBody must NOT include a top-level
// `name:` key (the builder splices that from the argument).
func (e *Envoy) CustomListener(name, yamlBody string) (*Listener, error) {
	if err := validateName("name", name); err != nil {
		return nil, err
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(yamlBody), &parsed); err != nil {
		return nil, fmt.Errorf("custom listener %q: invalid YAML body: %w", name, err)
	}
	if _, ok := parsed["name"]; ok {
		return nil, fmt.Errorf("custom listener %q: yamlBody must not include a top-level `name:` key (the builder splices it from the argument)", name)
	}
	return &Listener{Name: name, Body: yamlBody}, nil
}

// Proxy is a running Envoy instance with a composed (or
// caller-supplied) static-resources bootstrap.
type Proxy struct {
	Registry     string
	Tag          string
	AdminPort    int
	Override     *dagger.File
	Listeners    []*Listener
	Clusters     []*Cluster
	BindingHosts []string
	BindingSvcs  []*dagger.Service
}

// Proxy returns a Proxy backed by the envoyproxy/envoy image at
// <registry>/envoyproxy/envoy:<tag>. configFile, when supplied, fully
// replaces the rendered bootstrap; clusters added via WithCluster are
// ignored when an override is set, and listeners added via WithListener
// are ignored for rendering but still drive container port exposure so
// callers can expose ports from an override config.
func (e *Envoy) Proxy(
	// +default="docker.io"
	registry string,
	// +default="v1.32.1"
	tag string,
	// +default=9901
	adminPort int,
	// +optional
	configFile *dagger.File,
) *Proxy {
	return &Proxy{
		Registry:  registry,
		Tag:       tag,
		AdminPort: adminPort,
		Override:  configFile,
	}
}

// WithServiceBinding binds an upstream service into Envoy's network
// so cluster endpoints can reach it by hostname.
func (p *Proxy) WithServiceBinding(host string, svc *dagger.Service) *Proxy {
	out := *p
	out.BindingHosts = append(append([]string{}, p.BindingHosts...), host)
	out.BindingSvcs = append(append([]*dagger.Service{}, p.BindingSvcs...), svc)
	return &out
}

// WithListener appends a listener to the proxy.
func (p *Proxy) WithListener(l *Listener) *Proxy {
	out := *p
	out.Listeners = append(append([]*Listener{}, p.Listeners...), l)
	return &out
}

// WithCluster appends a cluster to the proxy.
func (p *Proxy) WithCluster(c *Cluster) *Proxy {
	out := *p
	out.Clusters = append(append([]*Cluster{}, p.Clusters...), c)
	return &out
}

// WithConfigFile fully replaces the rendered bootstrap.
func (p *Proxy) WithConfigFile(f *dagger.File) *Proxy {
	out := *p
	out.Override = f
	return &out
}

// ConfigFile returns the file that will be mounted as Envoy's -c
// argument: either the caller-supplied override or the rendered
// bootstrap. Returns a non-nil error if any listener references an
// unregistered cluster, or if two listeners share a name.
func (p *Proxy) ConfigFile() (*dagger.File, error) {
	if p.Override != nil {
		return p.Override, nil
	}
	if len(p.Listeners) == 0 && len(p.Clusters) == 0 {
		return nil, nil
	}
	if err := validateProxy(p); err != nil {
		return nil, err
	}
	adminPort, err := p.effectiveAdminPort()
	if err != nil {
		return nil, err
	}
	body, err := renderBootstrap(adminPort, p.Listeners, p.Clusters)
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile("envoy.yaml", body)
}

// Service returns the running Envoy container. Listens on
// AdminPort (admin) plus each registered listener's port. When no
// override and no listeners/clusters are registered, launches with
// no `-c` flag so the envoy binary exits non-zero — exposed verbatim
// so callers can detect the misconfig via service-binding probes.
func (p *Proxy) Service() (*dagger.Service, error) {
	cfg, err := p.ConfigFile()
	if err != nil {
		return nil, err
	}
	registry := p.Registry
	if registry == "" {
		registry = defaultRegistry
	}
	tag := p.Tag
	if tag == "" {
		tag = defaultTag
	}
	adminPort, err := p.effectiveAdminPort()
	if err != nil {
		return nil, err
	}
	image := fmt.Sprintf("%s/%s:%s", registry, envoyImagePath, tag)
	ctr := dag.Container().From(image).
		WithUser("0:0").
		WithoutDefaultArgs().
		WithExposedPort(adminPort)
	// When Override is set, WithListener registrations are ignored for
	// rendering; they are honored here solely as port-exposure hints so
	// callers can expose ports from an override config by registering
	// matching Listeners. When Override is unset, listener ports come
	// from the rendered bootstrap's listeners list.
	for _, l := range p.Listeners {
		if port, ok := extractListenerPort(l.Body); ok {
			ctr = ctr.WithExposedPort(port)
		}
		if l.SecurityMode == "TLS" || l.SecurityMode == "MTLS" {
			ctr = ctr.
				WithFile(listenerCertPath(l.Name), l.LeafCertPem, dagger.ContainerWithFileOpts{Permissions: 0o644}).
				WithFile(listenerKeyPath(l.Name), l.LeafKeyPem, dagger.ContainerWithFileOpts{Permissions: 0o644})
		}
		if l.SecurityMode == "MTLS" {
			ctr = ctr.WithFile(listenerTrustPath(l.Name), l.ClientTrustPem, dagger.ContainerWithFileOpts{Permissions: 0o644})
		}
	}
	for _, c := range p.Clusters {
		if c.UpstreamMode == "TLS" || c.UpstreamMode == "MTLS" {
			ctr = ctr.WithFile(upstreamTrustPath(c.Name), c.UpstreamTrustPem, dagger.ContainerWithFileOpts{Permissions: 0o644})
		}
		if c.UpstreamMode == "MTLS" {
			ctr = ctr.
				WithFile(upstreamCertPath(c.Name), c.UpstreamLeafCertPem, dagger.ContainerWithFileOpts{Permissions: 0o644}).
				WithFile(upstreamKeyPath(c.Name), c.UpstreamLeafKeyPem, dagger.ContainerWithFileOpts{Permissions: 0o644})
		}
	}
	for i, host := range p.BindingHosts {
		ctr = ctr.WithServiceBinding(host, p.BindingSvcs[i])
	}
	var args []string
	if cfg != nil {
		ctr = ctr.WithFile(configMountPath, cfg)
		args = []string{"-c", configMountPath}
	}
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
		Args:          args,
	}), nil
}

// AdminEndpoint returns host:adminPort for the running proxy.
//
// +cache="never"
func (p *Proxy) AdminEndpoint(ctx context.Context) (string, error) {
	svc, err := p.Service()
	if err != nil {
		return "", err
	}
	host, err := svc.Hostname(ctx)
	if err != nil {
		return "", err
	}
	adminPort, err := p.effectiveAdminPort()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, adminPort), nil
}

// effectiveAdminPort returns the admin port that ConfigFile, Service,
// and AdminEndpoint all agree on: defaults a zero AdminPort to
// defaultAdminPort, then enforces the 1..65535 range so an
// out-of-range value fails fast with a clear error instead of
// surfacing as a confusing Envoy/container startup failure.
func (p *Proxy) effectiveAdminPort() (int, error) {
	port := p.AdminPort
	if port == 0 {
		port = defaultAdminPort
	}
	if err := validatePort("adminPort", port); err != nil {
		return 0, err
	}
	return port, nil
}

// ListenerEndpoint returns host:port for the named listener on the
// running proxy. Returns a non-nil error if no listener matches name
// or if the listener's body has no recognizable socket_address.port_value.
//
// +cache="never"
func (p *Proxy) ListenerEndpoint(ctx context.Context, name string) (string, error) {
	var match *Listener
	for _, l := range p.Listeners {
		if l.Name == name {
			match = l
			break
		}
	}
	if match == nil {
		return "", fmt.Errorf("listener %q: not registered on proxy", name)
	}
	port, ok := extractListenerPort(match.Body)
	if !ok {
		return "", fmt.Errorf("listener %q: cannot extract socket_address.port_value from body", name)
	}
	svc, err := p.Service()
	if err != nil {
		return "", err
	}
	host, err := svc.Hostname(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, port), nil
}

// extractListenerPort walks the listener body's YAML for
// address.socket_address.port_value. Returns (0, false) if absent —
// CustomListener bodies that don't use the typical shape simply skip
// the WithExposedPort step (the listener still serves; callers just
// have to bind ports themselves if probing).
func extractListenerPort(body string) (int, bool) {
	if body == "" {
		return 0, false
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
		return 0, false
	}
	addr, _ := parsed["address"].(map[string]any)
	sa, _ := addr["socket_address"].(map[string]any)
	switch v := sa["port_value"].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// validateProxy checks the listener/cluster graph for the two
// errors ConfigFile() is contractually required to surface:
// duplicate listener names and listener filter-chain references to
// clusters that aren't registered on the proxy.
func validateProxy(p *Proxy) error {
	seen := make(map[string]bool, len(p.Listeners))
	for _, l := range p.Listeners {
		if seen[l.Name] {
			return fmt.Errorf("listener %q: declared more than once", l.Name)
		}
		seen[l.Name] = true
	}
	known := make(map[string]bool, len(p.Clusters))
	for _, c := range p.Clusters {
		known[c.Name] = true
	}
	for _, l := range p.Listeners {
		for _, ref := range l.ClusterRefs {
			if !known[ref] {
				return fmt.Errorf("listener %q references cluster %q, which is not registered on the proxy (add it via WithCluster)", l.Name, ref)
			}
		}
	}
	return nil
}

// renderBootstrap composes the static-resources Envoy bootstrap from
// the supplied admin port, listeners, and clusters. Listener bodies
// are re-unmarshaled and have `name` keyed in at render time so they
// fold into static_resources.listeners structurally.
func renderBootstrap(adminPort int, listeners []*Listener, clusters []*Cluster) ([]byte, error) {
	root := map[string]any{
		"admin": map[string]any{
			"address": map[string]any{
				"socket_address": map[string]any{
					"address":    "0.0.0.0",
					"port_value": adminPort,
				},
			},
		},
	}
	static := map[string]any{}
	if len(listeners) > 0 {
		ll := make([]any, 0, len(listeners))
		for _, l := range listeners {
			var body map[string]any
			if l.Body != "" {
				if err := yaml.Unmarshal([]byte(l.Body), &body); err != nil {
					return nil, fmt.Errorf("listener %q: parse body: %w", l.Name, err)
				}
			}
			if body == nil {
				body = map[string]any{}
			}
			out := map[string]any{"name": l.Name}
			for k, v := range body {
				out[k] = v
			}
			ll = append(ll, out)
		}
		static["listeners"] = ll
	}
	if len(clusters) > 0 {
		cl := make([]any, 0, len(clusters))
		for _, c := range clusters {
			cl = append(cl, renderCluster(c))
		}
		static["clusters"] = cl
	}
	if len(static) > 0 {
		root["static_resources"] = static
	}
	return yaml.Marshal(root)
}

func renderCluster(c *Cluster) map[string]any {
	out := map[string]any{
		"name": c.Name,
		"type": c.Kind,
	}
	if ts := renderUpstreamTransportSocket(c.Name, c.UpstreamMode); ts != nil {
		out["transport_socket"] = ts
	}
	if len(c.Endpoints) > 0 {
		lbEndpoints := make([]any, 0, len(c.Endpoints))
		for _, ep := range c.Endpoints {
			lbEndpoints = append(lbEndpoints, map[string]any{
				"endpoint": map[string]any{
					"address": map[string]any{
						"socket_address": map[string]any{
							"address":    ep.Host,
							"port_value": ep.Port,
						},
					},
				},
			})
		}
		out["load_assignment"] = map[string]any{
			"cluster_name": c.Name,
			"endpoints": []any{
				map[string]any{"lb_endpoints": lbEndpoints},
			},
		}
	}
	return out
}

// writeWorkdirFile writes content to a content-addressed subdir of
// the module's scratch workdir and returns it as a *dagger.File.
// Identical content lands at the same path so re-entry is idempotent.
func writeWorkdirFile(name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "out-" + hex.EncodeToString(sum[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}
