// Package main is the grafana-stack Dagger module: spins up Loki, Tempo, and
// Mimir as Dagger services for local development and testing. Each backend
// runs in single-binary / monolithic mode with optional caller-supplied
// persistence and exposes both its native ingest API and an OTLP/HTTP
// receiver. Plaintext is the only supported transport on every listener.
package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dagger/grafana-stack/internal/dagger"
)

// GrafanaStack is the module entry point. Use the per-backend constructor
// functions (Loki, Tempo, Mimir) to obtain a service handle.
type GrafanaStack struct{}

//go:embed configs/loki.yaml
var defaultLokiConfig []byte

//go:embed configs/tempo.yaml
var defaultTempoConfig []byte

//go:embed configs/mimir.yaml
var defaultMimirConfig []byte

//go:embed configs/grafana.ini
var defaultGrafanaConfig []byte

const lokiDataDir = "/var/lib/loki"
const lokiConfigPath = "/etc/loki/loki.yaml"
const lokiHTTPPort = 3100

const tempoDataDir = "/var/lib/tempo"
const tempoConfigPath = "/etc/tempo/tempo.yaml"
const tempoHTTPPort = 3200

const mimirDataDir = "/var/lib/mimir"
const mimirConfigPath = "/etc/mimir/mimir.yaml"
const mimirHTTPPort = 9009

const grafanaHTTPPort = 3000
const grafanaDataDir = "/var/lib/grafana"
const grafanaDashboardsDir = grafanaDataDir + "/dashboards"
const grafanaConfigPath = "/etc/grafana/grafana.ini"
const grafanaDsProvPath = "/etc/grafana/provisioning/datasources/datasources.yaml"
const grafanaDbProvPath = "/etc/grafana/provisioning/dashboards/dashboards.yaml"
const grafanaAdminPwdPath = "/run/secrets/grafana-admin-password"

// Loki wraps a configured grafana/loki container. Use Service() to obtain
// the *dagger.Service for binding into other containers, and Endpoint() /
// OtlpHttpEndpoint() to derive client URLs.
type Loki struct {
	// Image is the resolved <registry>/grafana/loki:<tag> reference.
	Image string
	// ConfigFile is the Loki YAML config: either the caller-supplied
	// override or the embedded default staged into the module workdir.
	ConfigFile *dagger.File
	// Storage is the optional persistence volume for /var/lib/loki.
	// When nil the data dir is mounted as an empty Directory (ephemeral).
	Storage *dagger.CacheVolume
}

// Loki configures a grafana/loki service running in monolithic mode with
// the OTLP HTTP ingester enabled and filesystem chunk/index storage rooted
// at the mounted data dir. Listens on :3100 plaintext.
//
// registry defaults to docker.io. tag defaults to a known-good upstream
// version. configFile fully replaces the embedded default when supplied.
// storage, when non-nil, is mounted at /var/lib/loki for persistence;
// when nil, an ephemeral empty Directory is mounted instead.
func (g *GrafanaStack) Loki(
	// Container registry hosting the grafana/loki image.
	// +default="docker.io"
	registry string,
	// Image tag for grafana/loki.
	// +default="3.4.1"
	tag string,
	// Loki YAML config; replaces the embedded default when supplied.
	// +optional
	configFile *dagger.File,
	// Persistence volume mounted at /var/lib/loki. When nil the data
	// dir is ephemeral.
	// +optional
	storage *dagger.CacheVolume,
) (*Loki, error) {
	cfg, err := resolveConfig(configFile, "loki.yaml", defaultLokiConfig)
	if err != nil {
		return nil, fmt.Errorf("resolve loki config: %w", err)
	}
	return &Loki{
		Image:      fmt.Sprintf("%s/grafana/loki:%s", registry, tag),
		ConfigFile: cfg,
		Storage:    storage,
	}, nil
}

// Service returns the Loki Dagger service. Bind it via WithServiceBinding
// or call .Start(ctx) to launch ahead-of-time.
//
// The container is run as root so it can write to the mounted data dir
// without us having to second-guess the upstream image's USER. This is
// safe for ephemeral dev/test services and avoids per-image UID drift
// across Loki / Tempo / Mimir.
func (l *Loki) Service() *dagger.Service {
	ctr := dag.Container().From(l.Image).
		WithUser("0:0").
		WithMountedFile(lokiConfigPath, l.ConfigFile).
		WithExposedPort(3100)
	ctr = mountDataDir(ctr, lokiDataDir, l.Storage)
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
		Args:          []string{"-config.file=" + lokiConfigPath},
	})
}

// Endpoint returns the Loki HTTP base URL, e.g. http://<host>:3100.
//
// +cache="never"
func (l *Loki) Endpoint(ctx context.Context) (string, error) {
	return l.Service().Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   3100,
		Scheme: "http",
	})
}

// OtlpHttpEndpoint returns the Loki OTLP/HTTP logs receiver URL, suitable
// as the `endpoint` for an OpenTelemetry exporter posting log data.
//
// +cache="never"
func (l *Loki) OtlpHttpEndpoint(ctx context.Context) (string, error) {
	base, err := l.Endpoint(ctx)
	if err != nil {
		return "", err
	}
	return base + "/otlp/v1/logs", nil
}

// Tempo wraps a configured grafana/tempo container running in monolithic
// mode with the OTLP gRPC and HTTP receivers enabled and local filesystem
// trace storage.
type Tempo struct {
	// Image is the resolved <registry>/grafana/tempo:<tag> reference.
	Image string
	// ConfigFile is the Tempo YAML config: either the caller-supplied
	// override or the embedded default.
	ConfigFile *dagger.File
	// Storage is the optional persistence volume for /var/lib/tempo.
	Storage *dagger.CacheVolume
}

// Tempo configures a grafana/tempo service running in monolithic mode
// with both OTLP receivers (gRPC :4317, HTTP :4318) enabled and local
// filesystem trace storage. Tempo's HTTP query API listens on :3200.
//
// registry defaults to docker.io. tag defaults to a known-good upstream
// version. configFile fully replaces the embedded default when supplied.
// storage, when non-nil, is mounted at /var/lib/tempo for persistence;
// when nil, an ephemeral empty Directory is mounted instead.
func (g *GrafanaStack) Tempo(
	// Container registry hosting the grafana/tempo image.
	// +default="docker.io"
	registry string,
	// Image tag for grafana/tempo.
	// +default="2.7.1"
	tag string,
	// Tempo YAML config; replaces the embedded default when supplied.
	// +optional
	configFile *dagger.File,
	// Persistence volume mounted at /var/lib/tempo. When nil the data
	// dir is ephemeral.
	// +optional
	storage *dagger.CacheVolume,
) (*Tempo, error) {
	cfg, err := resolveConfig(configFile, "tempo.yaml", defaultTempoConfig)
	if err != nil {
		return nil, fmt.Errorf("resolve tempo config: %w", err)
	}
	return &Tempo{
		Image:      fmt.Sprintf("%s/grafana/tempo:%s", registry, tag),
		ConfigFile: cfg,
		Storage:    storage,
	}, nil
}

// Service returns the Tempo Dagger service. See Loki.Service for notes
// on the WithUser("0:0") choice.
func (t *Tempo) Service() *dagger.Service {
	ctr := dag.Container().From(t.Image).
		WithUser("0:0").
		WithMountedFile(tempoConfigPath, t.ConfigFile).
		WithExposedPort(3200).
		WithExposedPort(4317).
		WithExposedPort(4318)
	ctr = mountDataDir(ctr, tempoDataDir, t.Storage)
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
		Args:          []string{"-config.file=" + tempoConfigPath},
	})
}

// HttpEndpoint returns the Tempo HTTP query/push base URL,
// e.g. http://<host>:3200.
//
// +cache="never"
func (t *Tempo) HttpEndpoint(ctx context.Context) (string, error) {
	return t.Service().Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   3200,
		Scheme: "http",
	})
}

// OtlpGrpcEndpoint returns the Tempo OTLP/gRPC receiver address,
// e.g. <host>:4317. No URL scheme — gRPC clients want host:port.
//
// +cache="never"
func (t *Tempo) OtlpGrpcEndpoint(ctx context.Context) (string, error) {
	return t.Service().Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port: 4317,
	})
}

// OtlpHttpEndpoint returns the Tempo OTLP/HTTP receiver base URL,
// e.g. http://<host>:4318. The OpenTelemetry HTTP exporter appends
// the per-signal path itself (e.g. /v1/traces).
//
// +cache="never"
func (t *Tempo) OtlpHttpEndpoint(ctx context.Context) (string, error) {
	return t.Service().Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   4318,
		Scheme: "http",
	})
}

// Mimir wraps a configured grafana/mimir container running in monolithic
// (single-binary, target=all) mode with the OTLP HTTP ingester enabled,
// anonymous tenant, and filesystem block storage.
type Mimir struct {
	// Image is the resolved <registry>/grafana/mimir:<tag> reference.
	Image string
	// ConfigFile is the Mimir YAML config: either the caller-supplied
	// override or the embedded default.
	ConfigFile *dagger.File
	// Storage is the optional persistence volume for /var/lib/mimir.
	Storage *dagger.CacheVolume
}

// Mimir configures a grafana/mimir service in monolithic mode (the binary
// is invoked with -target=all). Multitenancy is disabled so callers can
// push and query without an X-Scope-OrgID header. Listens on :9009 plain
// HTTP, exposing both the Prometheus-compatible API and the OTLP HTTP
// metrics ingester at /otlp/v1/metrics.
//
// registry defaults to docker.io. tag defaults to a known-good upstream
// version. configFile fully replaces the embedded default when supplied.
// storage, when non-nil, is mounted at /var/lib/mimir for persistence;
// when nil, an ephemeral empty Directory is mounted instead.
func (g *GrafanaStack) Mimir(
	// Container registry hosting the grafana/mimir image.
	// +default="docker.io"
	registry string,
	// Image tag for grafana/mimir.
	// +default="2.15.1"
	tag string,
	// Mimir YAML config; replaces the embedded default when supplied.
	// +optional
	configFile *dagger.File,
	// Persistence volume mounted at /var/lib/mimir. When nil the data
	// dir is ephemeral.
	// +optional
	storage *dagger.CacheVolume,
) (*Mimir, error) {
	cfg, err := resolveConfig(configFile, "mimir.yaml", defaultMimirConfig)
	if err != nil {
		return nil, fmt.Errorf("resolve mimir config: %w", err)
	}
	return &Mimir{
		Image:      fmt.Sprintf("%s/grafana/mimir:%s", registry, tag),
		ConfigFile: cfg,
		Storage:    storage,
	}, nil
}

// Service returns the Mimir Dagger service. The args explicitly include
// `-target=all` so the binary runs in monolithic mode regardless of the
// upstream image's default CMD. See Loki.Service for notes on the
// WithUser("0:0") choice.
func (m *Mimir) Service() *dagger.Service {
	ctr := dag.Container().From(m.Image).
		WithUser("0:0").
		WithMountedFile(mimirConfigPath, m.ConfigFile).
		WithExposedPort(9009)
	ctr = mountDataDir(ctr, mimirDataDir, m.Storage)
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
		Args:          []string{"-config.file=" + mimirConfigPath, "-target=all"},
	})
}

// Endpoint returns the Mimir HTTP base URL, e.g. http://<host>:9009.
// This endpoint serves both the Prometheus-compatible query API and
// the OTLP HTTP metrics ingester.
//
// +cache="never"
func (m *Mimir) Endpoint(ctx context.Context) (string, error) {
	return m.Service().Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   9009,
		Scheme: "http",
	})
}

// OtlpHttpEndpoint returns the Mimir OTLP/HTTP metrics receiver URL,
// suitable as the `endpoint` for an OpenTelemetry exporter posting
// metric data.
//
// +cache="never"
func (m *Mimir) OtlpHttpEndpoint(ctx context.Context) (string, error) {
	base, err := m.Endpoint(ctx)
	if err != nil {
		return "", err
	}
	return base + "/otlp/v1/metrics", nil
}

// Grafana wraps a configured grafana/grafana container with file-based
// datasource and dashboard provisioning. Datasources and dashboards are
// accumulated via the WithX builder methods and rendered into the
// container's /etc/grafana/provisioning tree at Service() time.
type Grafana struct {
	// Image is the resolved <registry>/grafana/grafana:<tag> reference.
	Image string
	// ConfigFile is the grafana.ini config: caller-supplied override or
	// the embedded default.
	ConfigFile *dagger.File
	// AdminPassword is mounted into the container as a file and pointed
	// at via GF_SECURITY_ADMIN_PASSWORD__FILE so plaintext never enters
	// generated bindings.
	AdminPassword *dagger.Secret
	// Storage is the optional persistence volume for /var/lib/grafana.
	Storage *dagger.CacheVolume

	// LokiNames[i] is the in-network hostname bound to LokiSvcs[i] and
	// is also used as the Grafana datasource name + uid. Same shape for
	// Tempo and Mimir.
	LokiNames  []string
	LokiSvcs   []*dagger.Service
	TempoNames []string
	TempoSvcs  []*dagger.Service
	MimirNames []string
	MimirSvcs  []*dagger.Service

	// Dashboards is the accumulated set of dashboard JSON files, mounted
	// at /var/lib/grafana/dashboards on the Grafana container at
	// Service() time. Starts empty.
	Dashboards *dagger.Directory
}

// Grafana configures a grafana/grafana service with file-based datasource
// and dashboard provisioning. Listens on :3000 plaintext.
//
// registry defaults to docker.io. tag defaults to a known-good upstream
// version. configFile fully replaces the embedded default when supplied.
// adminPassword is required and is mounted into the container at a fixed
// path; Grafana reads it via GF_SECURITY_ADMIN_PASSWORD__FILE so the
// plaintext never enters generated bindings. storage, when non-nil, is
// mounted at /var/lib/grafana for persistence; when nil, an ephemeral
// empty Directory is mounted instead.
func (g *GrafanaStack) Grafana(
	// Container registry hosting the grafana/grafana image.
	// +default="docker.io"
	registry string,
	// Image tag for grafana/grafana.
	// +default="12.0.0"
	tag string,
	// grafana.ini config; replaces the embedded default when supplied.
	// +optional
	configFile *dagger.File,
	// Admin password supplied to GF_SECURITY_ADMIN_PASSWORD__FILE.
	adminPassword *dagger.Secret,
	// Persistence volume mounted at /var/lib/grafana. When nil the data
	// dir is ephemeral.
	// +optional
	storage *dagger.CacheVolume,
) (*Grafana, error) {
	cfg, err := resolveConfig(configFile, "grafana.ini", defaultGrafanaConfig)
	if err != nil {
		return nil, fmt.Errorf("resolve grafana config: %w", err)
	}
	return &Grafana{
		Image:         fmt.Sprintf("%s/grafana/grafana:%s", registry, tag),
		ConfigFile:    cfg,
		AdminPassword: adminPassword,
		Storage:       storage,
		Dashboards:    dag.Directory(),
	}, nil
}

// WithLokiDatasource binds loki into Grafana's network at hostname `name`
// and accumulates a Loki datasource entry under the same name (which is
// also used as the datasource uid, so callers can hit
// /api/datasources/proxy/uid/<name>/...).
//
// `name` must be a valid DNS label: it is used as the in-network
// hostname (enforced by Dagger's WithServiceBinding), as the Grafana
// datasource uid, and is interpolated into provisioning YAML.
func (g *Grafana) WithLokiDatasource(name string, loki *Loki) *Grafana {
	out := *g
	out.LokiNames = append(append([]string{}, g.LokiNames...), name)
	out.LokiSvcs = append(append([]*dagger.Service{}, g.LokiSvcs...), loki.Service())
	return &out
}

// WithTempoDatasource binds tempo into Grafana's network at hostname
// `name` and accumulates a Tempo datasource entry under the same name.
// See WithLokiDatasource for the constraints on `name`.
func (g *Grafana) WithTempoDatasource(name string, tempo *Tempo) *Grafana {
	out := *g
	out.TempoNames = append(append([]string{}, g.TempoNames...), name)
	out.TempoSvcs = append(append([]*dagger.Service{}, g.TempoSvcs...), tempo.Service())
	return &out
}

// WithMimirDatasource binds mimir into Grafana's network at hostname
// `name` and accumulates a Prometheus-type datasource entry pointing at
// Mimir's Prometheus-compatible API endpoint. See WithLokiDatasource
// for the constraints on `name`.
func (g *Grafana) WithMimirDatasource(name string, mimir *Mimir) *Grafana {
	out := *g
	out.MimirNames = append(append([]string{}, g.MimirNames...), name)
	out.MimirSvcs = append(append([]*dagger.Service{}, g.MimirSvcs...), mimir.Service())
	return &out
}

// WithDashboard adds a single dashboard JSON file to the provisioned
// dashboards directory under the supplied name. `.json` is appended if
// the supplied name does not already end with it.
func (g *Grafana) WithDashboard(name string, file *dagger.File) *Grafana {
	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}
	out := *g
	out.Dashboards = g.Dashboards.WithFile(name, file)
	return &out
}

// WithDashboards adds every *.json entry in dir to the provisioned
// dashboards directory, preserving filenames.
func (g *Grafana) WithDashboards(dir *dagger.Directory) *Grafana {
	out := *g
	out.Dashboards = g.Dashboards.WithDirectory(".", dir, dagger.DirectoryWithDirectoryOpts{
		Include: []string{"*.json"},
	})
	return &out
}

// Service returns the Grafana Dagger service. The container is run as
// root (see Loki.Service for the rationale). The accumulated datasource
// and dashboard state is rendered into the container's provisioning
// tree at this point — subsequent WithX calls on the same *Grafana
// receiver are not visible to the returned service.
func (g *Grafana) Service() *dagger.Service {
	ctr := dag.Container().From(g.Image).
		WithUser("0:0").
		WithExposedPort(grafanaHTTPPort).
		WithMountedFile(grafanaConfigPath, g.ConfigFile).
		WithMountedSecret(grafanaAdminPwdPath, g.AdminPassword).
		WithEnvVariable("GF_SECURITY_ADMIN_PASSWORD__FILE", grafanaAdminPwdPath)

	ctr = mountDataDir(ctr, grafanaDataDir, g.Storage)

	for i, n := range g.LokiNames {
		ctr = ctr.WithServiceBinding(n, g.LokiSvcs[i])
	}
	for i, n := range g.TempoNames {
		ctr = ctr.WithServiceBinding(n, g.TempoSvcs[i])
	}
	for i, n := range g.MimirNames {
		ctr = ctr.WithServiceBinding(n, g.MimirSvcs[i])
	}

	dsYAML := renderDatasourcesYAML(g.LokiNames, g.TempoNames, g.MimirNames)
	dsFile := dag.Directory().WithNewFile("datasources.yaml", dsYAML).File("datasources.yaml")
	ctr = ctr.WithMountedFile(grafanaDsProvPath, dsFile)

	dbFile := dag.Directory().WithNewFile("dashboards.yaml", dashboardsProviderYAML).File("dashboards.yaml")
	ctr = ctr.
		WithMountedFile(grafanaDbProvPath, dbFile).
		WithMountedDirectory(grafanaDashboardsDir, g.Dashboards)

	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
	})
}

// Endpoint returns the Grafana HTTP base URL, e.g. http://<host>:3000.
//
// +cache="never"
func (g *Grafana) Endpoint(ctx context.Context) (string, error) {
	return g.Service().Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   grafanaHTTPPort,
		Scheme: "http",
	})
}

// dashboardsProviderYAML is a fixed single-provider config pointing at
// the on-disk dashboards directory inside the Grafana container. All
// provisioned dashboards land in one flat folder at the Grafana UI;
// callers wanting folder grouping should embed it in the dashboard JSON.
const dashboardsProviderYAML = `apiVersion: 1
providers:
  - name: default
    orgId: 1
    folder: ''
    type: file
    disableDeletion: false
    updateIntervalSeconds: 10
    allowUiUpdates: false
    options:
      path: ` + grafanaDashboardsDir + `
`

// renderDatasourcesYAML emits the file-based provisioning YAML for the
// accumulated set of {Loki, Tempo, Mimir} datasources. Each entry sets
// uid == name so callers can address datasources via the proxy API at
// /api/datasources/proxy/uid/<name>/... without an extra lookup.
func renderDatasourcesYAML(lokiNames, tempoNames, mimirNames []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: 1\ndatasources:\n")
	for _, n := range lokiNames {
		fmt.Fprintf(&b, "  - name: %s\n    uid: %s\n    type: loki\n    access: proxy\n    url: http://%s:%d\n    isDefault: false\n",
			n, n, n, lokiHTTPPort)
	}
	for _, n := range tempoNames {
		fmt.Fprintf(&b, "  - name: %s\n    uid: %s\n    type: tempo\n    access: proxy\n    url: http://%s:%d\n    isDefault: false\n",
			n, n, n, tempoHTTPPort)
	}
	for _, n := range mimirNames {
		fmt.Fprintf(&b, "  - name: %s\n    uid: %s\n    type: prometheus\n    access: proxy\n    url: http://%s:%d/prometheus\n    isDefault: false\n",
			n, n, n, mimirHTTPPort)
	}
	return b.String()
}

// resolveConfig returns the caller-supplied config file when non-nil,
// otherwise stages the embedded default bytes into the module's workdir
// as a *dagger.File. Same content always lands at the same path so
// repeated calls inside a session reuse the file ID.
func resolveConfig(supplied *dagger.File, name string, fallback []byte) (*dagger.File, error) {
	if supplied != nil {
		return supplied, nil
	}
	return writeWorkdirFile(name, fallback)
}

// mountDataDir mounts storage at path when non-nil, otherwise mounts an
// empty *dagger.Directory so the path is writable but ephemeral.
func mountDataDir(ctr *dagger.Container, path string, storage *dagger.CacheVolume) *dagger.Container {
	if storage != nil {
		return ctr.WithMountedCache(path, storage)
	}
	return ctr.WithMountedDirectory(path, dag.Directory())
}

// writeWorkdirFile writes content to a content-addressed subdir of the
// module's scratch workdir and returns it as a *dagger.File. The subdir
// name is derived from a hash of the content so distinct outputs land at
// distinct WorkdirFile paths and identical outputs are idempotent.
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
