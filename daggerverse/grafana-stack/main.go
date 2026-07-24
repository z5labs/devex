// Package main is the grafana-stack Dagger module: spins up Loki, Tempo, and
// Mimir as Dagger services for local development and testing. Each backend
// runs in single-binary / monolithic mode with optional caller-supplied
// persistence and exposes both its native ingest API and an OTLP/HTTP
// receiver.
//
// Every listener defaults to plaintext. WithTls enables TLS — and WithMtls
// optional mutual TLS — on every listener a backend exposes (the native HTTP
// API plus the OTLP receivers), and on the Grafana UI's :3000 listener.
// Datasource provisioning is TLS-aware: WithLokiDatasource / WithTempoDatasource
// / WithMimirDatasource detect a TLS-enabled backend and render the datasource
// YAML with an https:// URL and the CA (plus client cert for an mTLS backend)
// so the Grafana proxy chain still works end-to-end.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dagger/grafana-stack/internal/dagger"

	"gopkg.in/yaml.v3"
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

// TLS material mount directories, one per backend / UI. Each holds the
// server certificate (tls.crt, a file), the server key (tls.key, a secret),
// and — under mTLS — the client CA (ca.crt, a file). The rendered backend
// config references these exact paths, and Service() mounts the caller's
// cert material onto them.
const lokiTLSDir = "/etc/loki/tls"
const tempoTLSDir = "/etc/tempo/tls"
const mimirTLSDir = "/etc/mimir/tls"
const grafanaTLSDir = "/etc/grafana/tls"

// grafanaDsTLSDir roots the per-datasource client TLS material Grafana uses
// to reach a TLS/mTLS backend; the provisioned datasources.yaml references
// files under <dir>/<datasource-name>/ via Grafana's $__file{} expansion.
const grafanaDsTLSDir = "/etc/grafana/tls/datasources"

// Fixed filenames inside a TLS material directory.
const tlsCertName = "tls.crt"
const tlsKeyName = "tls.key"
const tlsCaName = "ca.crt"

// clientAuthRequireAndVerify is the dskit http_tls_config client_auth_type
// that requires and verifies a client certificate on every
// incoming connection (mutual TLS).
const clientAuthRequireAndVerify = "RequireAndVerifyClientCert"

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
	// CustomConfig records whether ConfigFile came from the caller. When
	// true, TLS blocks are never spliced in — the caller owns the config,
	// including any server.http_tls_config referencing the mount paths.
	// +private
	CustomConfig bool
	// ServerCert / ServerKey hold the PEM server certificate and its key
	// once WithTls has been called; ClientCa holds the client CA once
	// WithMtls has been called. All nil under plaintext.
	// +private
	ServerCert *dagger.File
	// +private
	ServerKey *dagger.Secret
	// +private
	ClientCa *dagger.File
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
		Image:        fmt.Sprintf("%s/grafana/loki:%s", registry, tag),
		ConfigFile:   cfg,
		Storage:      storage,
		CustomConfig: configFile != nil,
	}, nil
}

// WithTls enables TLS on Loki's HTTP listener (:3100) — which serves both the
// native LogQL/ingest API and the OTLP/HTTP logs receiver — by rendering
// server.http_tls_config into the config. serverCert is the PEM server
// certificate (its SAN must cover the hostname clients dial) and serverKey its
// PEM private key. After this call Endpoint / OtlpHttpEndpoint return https://
// URLs. Ignored when a custom config file was supplied to Loki(); that config
// owns its own TLS.
func (l *Loki) WithTls(serverCert *dagger.File, serverKey *dagger.Secret) *Loki {
	out := *l
	out.ServerCert = serverCert
	out.ServerKey = serverKey
	return &out
}

// WithMtls additionally requires every client to present a certificate signed
// by clientCa (PEM). Must be combined with WithTls; Service returns an error
// otherwise.
func (l *Loki) WithMtls(clientCa *dagger.File) *Loki {
	out := *l
	out.ClientCa = clientCa
	return &out
}

// Service returns the Loki Dagger service. Bind it via WithServiceBinding
// or call .Start(ctx) to launch ahead-of-time.
//
// The container is run as root so it can write to the mounted data dir
// without us having to second-guess the upstream image's USER. This is
// safe for ephemeral dev/test services and avoids per-image UID drift
// across Loki / Tempo / Mimir.
func (l *Loki) Service() (*dagger.Service, error) {
	if err := mtlsWithoutTLS(l.ServerCert, l.ClientCa); err != nil {
		return nil, err
	}
	cfg := l.ConfigFile
	if l.ServerCert != nil && !l.CustomConfig {
		var err error
		cfg, err = renderDskitHTTPTLSConfig(defaultLokiConfig, "loki-tls.yaml", lokiTLSDir, l.ClientCa != nil)
		if err != nil {
			return nil, err
		}
	}
	ctr := dag.Container().From(l.Image).
		WithUser("0:0").
		WithMountedFile(lokiConfigPath, cfg).
		WithExposedPort(lokiHTTPPort)
	ctr = applyServerTLSMaterial(ctr, lokiTLSDir, l.ServerCert, l.ServerKey, l.ClientCa)
	ctr = mountDataDir(ctr, lokiDataDir, l.Storage)
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
		Args:          []string{"-config.file=" + lokiConfigPath},
	}), nil
}

// Endpoint returns the Loki HTTP base URL, e.g. http://<host>:3100, or
// https://<host>:3100 once WithTls has been called.
//
// +cache="never"
func (l *Loki) Endpoint(ctx context.Context) (string, error) {
	svc, err := l.Service()
	if err != nil {
		return "", err
	}
	return svc.Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   lokiHTTPPort,
		Scheme: scheme(l.ServerCert),
	})
}

// OtlpHttpEndpoint returns the Loki OTLP/HTTP logs receiver URL, suitable
// as the `endpoint` for an OpenTelemetry exporter posting log data. The
// scheme is https once WithTls has been called.
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
	// CustomConfig records whether ConfigFile came from the caller; when
	// true, TLS blocks are never spliced in.
	// +private
	CustomConfig bool
	// ServerCert / ServerKey / ClientCa hold the TLS material once WithTls
	// / WithMtls have been called. All nil under plaintext.
	// +private
	ServerCert *dagger.File
	// +private
	ServerKey *dagger.Secret
	// +private
	ClientCa *dagger.File
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
		Image:        fmt.Sprintf("%s/grafana/tempo:%s", registry, tag),
		ConfigFile:   cfg,
		Storage:      storage,
		CustomConfig: configFile != nil,
	}, nil
}

// WithTls enables TLS on every Tempo listener: the native HTTP query API
// (:3200, via server.http_tls_config) and both OTLP receivers (gRPC :4317 and
// HTTP :4318, via distributor.receivers.otlp.protocols.{grpc,http}.tls).
// serverCert is the PEM server certificate and serverKey its PEM private key.
// After this call HttpEndpoint / OtlpHttpEndpoint return https:// URLs;
// OtlpGrpcEndpoint stays scheme-less (gRPC callers configure TLS themselves).
// Ignored when a custom config file was supplied to Tempo().
func (t *Tempo) WithTls(serverCert *dagger.File, serverKey *dagger.Secret) *Tempo {
	out := *t
	out.ServerCert = serverCert
	out.ServerKey = serverKey
	return &out
}

// WithMtls additionally requires every client to present a certificate signed
// by clientCa (PEM) on every listener. Must be combined with WithTls; Service
// returns an error otherwise.
func (t *Tempo) WithMtls(clientCa *dagger.File) *Tempo {
	out := *t
	out.ClientCa = clientCa
	return &out
}

// Service returns the Tempo Dagger service. See Loki.Service for notes
// on the WithUser("0:0") choice.
func (t *Tempo) Service() (*dagger.Service, error) {
	if err := mtlsWithoutTLS(t.ServerCert, t.ClientCa); err != nil {
		return nil, err
	}
	cfg := t.ConfigFile
	if t.ServerCert != nil && !t.CustomConfig {
		var err error
		cfg, err = renderTempoTLSConfig(defaultTempoConfig, "tempo-tls.yaml", tempoTLSDir, t.ClientCa != nil)
		if err != nil {
			return nil, err
		}
	}
	ctr := dag.Container().From(t.Image).
		WithUser("0:0").
		WithMountedFile(tempoConfigPath, cfg).
		WithExposedPort(tempoHTTPPort).
		WithExposedPort(4317).
		WithExposedPort(4318)
	ctr = applyServerTLSMaterial(ctr, tempoTLSDir, t.ServerCert, t.ServerKey, t.ClientCa)
	ctr = mountDataDir(ctr, tempoDataDir, t.Storage)
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
		Args:          []string{"-config.file=" + tempoConfigPath},
	}), nil
}

// HttpEndpoint returns the Tempo HTTP query/push base URL,
// e.g. http://<host>:3200, or https://<host>:3200 once WithTls has been
// called.
//
// +cache="never"
func (t *Tempo) HttpEndpoint(ctx context.Context) (string, error) {
	svc, err := t.Service()
	if err != nil {
		return "", err
	}
	return svc.Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   tempoHTTPPort,
		Scheme: scheme(t.ServerCert),
	})
}

// OtlpGrpcEndpoint returns the Tempo OTLP/gRPC receiver address,
// e.g. <host>:4317. No URL scheme — gRPC clients want host:port and must
// configure TLS themselves (the receiver enforces it once WithTls is set).
//
// +cache="never"
func (t *Tempo) OtlpGrpcEndpoint(ctx context.Context) (string, error) {
	svc, err := t.Service()
	if err != nil {
		return "", err
	}
	return svc.Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port: 4317,
	})
}

// OtlpHttpEndpoint returns the Tempo OTLP/HTTP receiver base URL,
// e.g. http://<host>:4318, or https://<host>:4318 once WithTls has been
// called. The OpenTelemetry HTTP exporter appends the per-signal path
// itself (e.g. /v1/traces).
//
// +cache="never"
func (t *Tempo) OtlpHttpEndpoint(ctx context.Context) (string, error) {
	svc, err := t.Service()
	if err != nil {
		return "", err
	}
	return svc.Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   4318,
		Scheme: scheme(t.ServerCert),
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
	// CustomConfig records whether ConfigFile came from the caller; when
	// true, TLS blocks are never spliced in.
	// +private
	CustomConfig bool
	// ServerCert / ServerKey / ClientCa hold the TLS material once WithTls
	// / WithMtls have been called. All nil under plaintext.
	// +private
	ServerCert *dagger.File
	// +private
	ServerKey *dagger.Secret
	// +private
	ClientCa *dagger.File
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
		Image:        fmt.Sprintf("%s/grafana/mimir:%s", registry, tag),
		ConfigFile:   cfg,
		Storage:      storage,
		CustomConfig: configFile != nil,
	}, nil
}

// WithTls enables TLS on Mimir's HTTP listener (:9009) — which serves both the
// Prometheus-compatible API and the OTLP/HTTP metrics receiver — by rendering
// server.http_tls_config into the config. serverCert is the PEM server
// certificate and serverKey its PEM private key. After this call Endpoint /
// OtlpHttpEndpoint return https:// URLs. Ignored when a custom config file was
// supplied to Mimir().
func (m *Mimir) WithTls(serverCert *dagger.File, serverKey *dagger.Secret) *Mimir {
	out := *m
	out.ServerCert = serverCert
	out.ServerKey = serverKey
	return &out
}

// WithMtls additionally requires every client to present a certificate signed
// by clientCa (PEM). Must be combined with WithTls; Service returns an error
// otherwise.
func (m *Mimir) WithMtls(clientCa *dagger.File) *Mimir {
	out := *m
	out.ClientCa = clientCa
	return &out
}

// Service returns the Mimir Dagger service. The args explicitly include
// `-target=all` so the binary runs in monolithic mode regardless of the
// upstream image's default CMD. See Loki.Service for notes on the
// WithUser("0:0") choice.
func (m *Mimir) Service() (*dagger.Service, error) {
	if err := mtlsWithoutTLS(m.ServerCert, m.ClientCa); err != nil {
		return nil, err
	}
	cfg := m.ConfigFile
	if m.ServerCert != nil && !m.CustomConfig {
		var err error
		cfg, err = renderDskitHTTPTLSConfig(defaultMimirConfig, "mimir-tls.yaml", mimirTLSDir, m.ClientCa != nil)
		if err != nil {
			return nil, err
		}
	}
	ctr := dag.Container().From(m.Image).
		WithUser("0:0").
		WithMountedFile(mimirConfigPath, cfg).
		WithExposedPort(mimirHTTPPort)
	ctr = applyServerTLSMaterial(ctr, mimirTLSDir, m.ServerCert, m.ServerKey, m.ClientCa)
	ctr = mountDataDir(ctr, mimirDataDir, m.Storage)
	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
		Args:          []string{"-config.file=" + mimirConfigPath, "-target=all"},
	}), nil
}

// Endpoint returns the Mimir HTTP base URL, e.g. http://<host>:9009, or
// https://<host>:9009 once WithTls has been called. This endpoint serves
// both the Prometheus-compatible query API and the OTLP HTTP metrics
// ingester.
//
// +cache="never"
func (m *Mimir) Endpoint(ctx context.Context) (string, error) {
	svc, err := m.Service()
	if err != nil {
		return "", err
	}
	return svc.Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   mimirHTTPPort,
		Scheme: scheme(m.ServerCert),
	})
}

// OtlpHttpEndpoint returns the Mimir OTLP/HTTP metrics receiver URL,
// suitable as the `endpoint` for an OpenTelemetry exporter posting
// metric data. The scheme is https once WithTls has been called.
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

	// LokiDatasources / TempoDatasources / MimirDatasources are the
	// accumulated datasource entries, each carrying the backend builder
	// and any client TLS material Grafana uses to reach it.
	// +private
	LokiDatasources []*LokiDatasource
	// +private
	TempoDatasources []*TempoDatasource
	// +private
	MimirDatasources []*MimirDatasource

	// CustomConfig records whether ConfigFile came from the caller; when
	// true, the TLS [server] block is never appended.
	// +private
	CustomConfig bool
	// ServerCert / ServerKey hold the Grafana UI's TLS material once
	// WithTls has been called; nil under plaintext. (Grafana core cannot
	// require client certificates, so there is no WithMtls — see the
	// module README.)
	// +private
	ServerCert *dagger.File
	// +private
	ServerKey *dagger.Secret

	// Dashboards is the accumulated set of dashboard JSON files, mounted
	// at /var/lib/grafana/dashboards on the Grafana container at
	// Service() time. Starts empty.
	Dashboards *dagger.Directory
}

// LokiDatasource is one provisioned Loki datasource: the in-network name
// (also the Grafana datasource uid), the backend builder it points at, and
// the client TLS material Grafana uses to reach the backend when it has
// TLS/mTLS. CaCert / ClientCert / ClientKey are nil for a plaintext backend.
type LokiDatasource struct {
	// +private
	Name string
	// +private
	Backend *Loki
	// +private
	CaCert *dagger.File
	// +private
	ClientCert *dagger.File
	// +private
	ClientKey *dagger.Secret
}

// TempoDatasource mirrors LokiDatasource for a Tempo backend.
type TempoDatasource struct {
	// +private
	Name string
	// +private
	Backend *Tempo
	// +private
	CaCert *dagger.File
	// +private
	ClientCert *dagger.File
	// +private
	ClientKey *dagger.Secret
}

// MimirDatasource mirrors LokiDatasource for a Mimir backend.
type MimirDatasource struct {
	// +private
	Name string
	// +private
	Backend *Mimir
	// +private
	CaCert *dagger.File
	// +private
	ClientCert *dagger.File
	// +private
	ClientKey *dagger.Secret
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
		CustomConfig:  configFile != nil,
		Dashboards:    dag.Directory(),
	}, nil
}

// WithTls switches the Grafana UI's :3000 listener from HTTP to HTTPS by
// setting [server] protocol = https plus cert_file / cert_key in grafana.ini.
// serverCert is the PEM server certificate and serverKey its PEM private key.
// After this call Endpoint returns an https:// URL.
//
// Note: Grafana core does not support requiring client certificates on its
// own listener, so there is no Grafana.WithMtls. To reach an mTLS-required
// backend, supply the client certificate to the datasource instead (see
// WithLokiDatasource). Ignored when a custom config file was supplied to
// Grafana().
func (g *Grafana) WithTls(serverCert *dagger.File, serverKey *dagger.Secret) *Grafana {
	out := *g
	out.ServerCert = serverCert
	out.ServerKey = serverKey
	return &out
}

// WithLokiDatasource binds loki into Grafana's network at hostname `name`
// and accumulates a Loki datasource entry under the same name (which is
// also used as the datasource uid, so callers can hit
// /api/datasources/proxy/uid/<name>/...).
//
// `name` must be a valid DNS label: it is used as the in-network
// hostname (enforced by Dagger's WithServiceBinding), as the Grafana
// datasource uid, and is interpolated into provisioning YAML.
//
// TLS is derived from the loki builder: when loki has WithTls, the datasource
// URL becomes https:// and the entry pins the backend's CA. Supply caCert (the
// PEM CA that signed loki's server certificate) so Grafana can verify the
// backend; when omitted, loki's own server certificate is pinned instead
// (correct only for a self-signed server cert). When loki has WithMtls, also
// supply clientCert / clientKey (the PEM certificate + key Grafana presents to
// the backend); Service returns an error if they are missing.
func (g *Grafana) WithLokiDatasource(
	name string,
	loki *Loki,
	// PEM CA that signed the backend's server certificate.
	// +optional
	caCert *dagger.File,
	// PEM client certificate Grafana presents to an mTLS backend.
	// +optional
	clientCert *dagger.File,
	// PEM client private key paired with clientCert.
	// +optional
	clientKey *dagger.Secret,
) *Grafana {
	out := *g
	out.LokiDatasources = append(append([]*LokiDatasource{}, g.LokiDatasources...), &LokiDatasource{
		Name:       name,
		Backend:    loki,
		CaCert:     caCert,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	})
	return &out
}

// WithTempoDatasource binds tempo into Grafana's network at hostname
// `name` and accumulates a Tempo datasource entry under the same name.
// See WithLokiDatasource for the constraints on `name` and the TLS args.
func (g *Grafana) WithTempoDatasource(
	name string,
	tempo *Tempo,
	// +optional
	caCert *dagger.File,
	// +optional
	clientCert *dagger.File,
	// +optional
	clientKey *dagger.Secret,
) *Grafana {
	out := *g
	out.TempoDatasources = append(append([]*TempoDatasource{}, g.TempoDatasources...), &TempoDatasource{
		Name:       name,
		Backend:    tempo,
		CaCert:     caCert,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	})
	return &out
}

// WithMimirDatasource binds mimir into Grafana's network at hostname `name`
// and accumulates a Prometheus-type datasource entry pointing at
// Mimir's Prometheus-compatible API endpoint. See WithLokiDatasource
// for the constraints on `name` and the TLS args.
func (g *Grafana) WithMimirDatasource(
	name string,
	mimir *Mimir,
	// +optional
	caCert *dagger.File,
	// +optional
	clientCert *dagger.File,
	// +optional
	clientKey *dagger.Secret,
) *Grafana {
	out := *g
	out.MimirDatasources = append(append([]*MimirDatasource{}, g.MimirDatasources...), &MimirDatasource{
		Name:       name,
		Backend:    mimir,
		CaCert:     caCert,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	})
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
func (g *Grafana) Service() (*dagger.Service, error) {
	cfg := g.ConfigFile
	if g.ServerCert != nil && !g.CustomConfig {
		var err error
		cfg, err = renderGrafanaTLSConfig()
		if err != nil {
			return nil, err
		}
	}
	ctr := dag.Container().From(g.Image).
		WithUser("0:0").
		WithExposedPort(grafanaHTTPPort).
		WithMountedFile(grafanaConfigPath, cfg).
		WithMountedSecret(grafanaAdminPwdPath, g.AdminPassword).
		WithEnvVariable("GF_SECURITY_ADMIN_PASSWORD__FILE", grafanaAdminPwdPath)
	ctr = applyServerTLSMaterial(ctr, grafanaTLSDir, g.ServerCert, g.ServerKey, nil)
	ctr = mountDataDir(ctr, grafanaDataDir, g.Storage)

	var entries []map[string]any
	for _, ds := range g.LokiDatasources {
		svc, err := ds.Backend.Service()
		if err != nil {
			return nil, err
		}
		ctr = ctr.WithServiceBinding(ds.Name, svc)
		var entry map[string]any
		ctr, entry, err = datasourceEntry(ctr, ds.Name, "loki", "", lokiHTTPPort,
			ds.Backend.ServerCert, ds.Backend.ClientCa, ds.CaCert, ds.ClientCert, ds.ClientKey)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	for _, ds := range g.TempoDatasources {
		svc, err := ds.Backend.Service()
		if err != nil {
			return nil, err
		}
		ctr = ctr.WithServiceBinding(ds.Name, svc)
		var entry map[string]any
		ctr, entry, err = datasourceEntry(ctr, ds.Name, "tempo", "", tempoHTTPPort,
			ds.Backend.ServerCert, ds.Backend.ClientCa, ds.CaCert, ds.ClientCert, ds.ClientKey)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	for _, ds := range g.MimirDatasources {
		svc, err := ds.Backend.Service()
		if err != nil {
			return nil, err
		}
		ctr = ctr.WithServiceBinding(ds.Name, svc)
		var entry map[string]any
		ctr, entry, err = datasourceEntry(ctr, ds.Name, "prometheus", "/prometheus", mimirHTTPPort,
			ds.Backend.ServerCert, ds.Backend.ClientCa, ds.CaCert, ds.ClientCert, ds.ClientKey)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	dsYAML, err := renderDatasourcesYAML(entries)
	if err != nil {
		return nil, err
	}
	dsFile := dag.Directory().WithNewFile("datasources.yaml", dsYAML).File("datasources.yaml")
	ctr = ctr.WithMountedFile(grafanaDsProvPath, dsFile)

	dbFile := dag.Directory().WithNewFile("dashboards.yaml", dashboardsProviderYAML).File("dashboards.yaml")
	ctr = ctr.
		WithMountedFile(grafanaDbProvPath, dbFile).
		WithMountedDirectory(grafanaDashboardsDir, g.Dashboards)

	return ctr.AsService(dagger.ContainerAsServiceOpts{
		UseEntrypoint: true,
	}), nil
}

// Endpoint returns the Grafana HTTP base URL, e.g. http://<host>:3000, or
// https://<host>:3000 once WithTls has been called.
//
// +cache="never"
func (g *Grafana) Endpoint(ctx context.Context) (string, error) {
	svc, err := g.Service()
	if err != nil {
		return "", err
	}
	return svc.Endpoint(ctx, dagger.ServiceEndpointOpts{
		Port:   grafanaHTTPPort,
		Scheme: scheme(g.ServerCert),
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

// renderDatasourcesYAML marshals the accumulated provisioning entries into a
// datasources.yaml document. Each entry sets uid == name so callers can
// address datasources via the proxy API at /api/datasources/proxy/uid/<name>/
// without an extra lookup. yaml.v3 handles quoting of the $__file{} TLS
// references, so no manual escaping is needed.
func renderDatasourcesYAML(entries []map[string]any) (string, error) {
	if entries == nil {
		entries = []map[string]any{}
	}
	doc := map[string]any{
		"apiVersion":  1,
		"datasources": entries,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("render datasources: %w", err)
	}
	return string(out), nil
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

// tlsPaths returns the tls.crt / tls.key / ca.crt paths inside a TLS
// material directory.
func tlsPaths(dir string) (cert, key, ca string) {
	return dir + "/" + tlsCertName, dir + "/" + tlsKeyName, dir + "/" + tlsCaName
}

// scheme returns "https" once a server certificate has been configured
// (WithTls was called), "http" otherwise.
func scheme(serverCert *dagger.File) string {
	if serverCert != nil {
		return "https"
	}
	return "http"
}

// applyServerTLSMaterial mounts the server certificate (file), server key
// (secret, so it never lands in the layer cache) and — under mTLS — the
// client CA (file) at the fixed paths inside dir that the rendered config
// already references. A nil serverCert leaves the container untouched.
func applyServerTLSMaterial(ctr *dagger.Container, dir string, serverCert *dagger.File, serverKey *dagger.Secret, clientCa *dagger.File) *dagger.Container {
	if serverCert == nil {
		return ctr
	}
	certPath, keyPath, caPath := tlsPaths(dir)
	ctr = ctr.
		WithMountedFile(certPath, serverCert).
		WithMountedSecret(keyPath, serverKey)
	if clientCa != nil {
		ctr = ctr.WithMountedFile(caPath, clientCa)
	}
	return ctr
}

// documentMapping returns the top-level mapping node of a parsed YAML
// document, creating an empty one for an empty document. Splicing operates on
// yaml.Node (not map[string]any) so untouched scalars keep their exact source
// representation — notably Loki's `from: 2020-05-15`, which a map round-trip
// would rewrite to a full RFC3339 timestamp that Loki's config parser rejects.
func documentMapping(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			doc.Content = []*yaml.Node{m}
			return m
		}
		return doc.Content[0]
	}
	return doc
}

// ensureMapNode returns the mapping node stored under key in a mapping node,
// creating (and appending) an empty mapping when the key is absent.
func ensureMapNode(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		child)
	return child
}

// setMapKey sets (or replaces) key -> value in a mapping node.
func setMapKey(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value)
}

// blockNode converts a freshly-built map (no user-supplied scalars, so a
// map round-trip is safe here) into a yaml.Node ready to splice into a config
// tree.
func blockNode(m map[string]any) (*yaml.Node, error) {
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return doc.Content[0], nil
	}
	return &doc, nil
}

// serverTLSBlock renders a dskit http_tls_config / server tls_config block
// pointing at the fixed server cert/key paths inside dir. Under mTLS it also
// pins the client CA and sets client_auth_type so the listener requires and
// verifies a client certificate. Used by Loki, Mimir and Tempo's native HTTP
// API.
func serverTLSBlock(dir string, mtls bool) map[string]any {
	certPath, keyPath, caPath := tlsPaths(dir)
	blk := map[string]any{
		"cert_file": certPath,
		"key_file":  keyPath,
	}
	if mtls {
		blk["client_ca_file"] = caPath
		blk["client_auth_type"] = clientAuthRequireAndVerify
	}
	return blk
}

// receiverTLSBlock renders the OpenTelemetry Collector receiver `tls:` block
// used by Tempo's OTLP receivers. Unlike the dskit server block there is no
// client_auth_type — the presence of client_ca_file is what makes the
// receiver require and verify a client certificate.
func receiverTLSBlock(dir string, mtls bool) map[string]any {
	certPath, keyPath, caPath := tlsPaths(dir)
	blk := map[string]any{
		"cert_file": certPath,
		"key_file":  keyPath,
	}
	if mtls {
		blk["client_ca_file"] = caPath
	}
	return blk
}

// renderDskitHTTPTLSConfig parses a dskit-based backend's default YAML config
// (Loki or Mimir), splices server.http_tls_config into it, and stages the
// result as a workdir file. Because the OTLP receiver is served on the same
// HTTP listener, one block secures both the native API and the OTLP ingester.
func renderDskitHTTPTLSConfig(base []byte, name, dir string, mtls bool) (*dagger.File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("parse %s config: %w", name, err)
	}
	root := documentMapping(&doc)
	blk, err := blockNode(serverTLSBlock(dir, mtls))
	if err != nil {
		return nil, fmt.Errorf("build %s tls block: %w", name, err)
	}
	setMapKey(ensureMapNode(root, "server"), "http_tls_config", blk)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("render %s config: %w", name, err)
	}
	return writeWorkdirFile(name, out)
}

// renderTempoTLSConfig parses Tempo's default YAML config and enables TLS on
// every listener: server.http_tls_config for the native HTTP query API, and
// distributor.receivers.otlp.protocols.{grpc,http}.tls for the two OTLP
// receivers. The result is staged as a workdir file.
func renderTempoTLSConfig(base []byte, name, dir string, mtls bool) (*dagger.File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("parse %s config: %w", name, err)
	}
	root := documentMapping(&doc)

	// Tempo's `server` block is a dskit server.Config, so the native HTTP
	// API's TLS lives under http_tls_config (same key as Loki/Mimir), not
	// a `tls_config` key.
	serverBlk, err := blockNode(serverTLSBlock(dir, mtls))
	if err != nil {
		return nil, fmt.Errorf("build %s server tls block: %w", name, err)
	}
	setMapKey(ensureMapNode(root, "server"), "http_tls_config", serverBlk)

	protocols := ensureMapNode(ensureMapNode(ensureMapNode(ensureMapNode(root, "distributor"), "receivers"), "otlp"), "protocols")
	for _, proto := range []string{"grpc", "http"} {
		blk, err := blockNode(receiverTLSBlock(dir, mtls))
		if err != nil {
			return nil, fmt.Errorf("build %s %s tls block: %w", name, proto, err)
		}
		setMapKey(ensureMapNode(protocols, proto), "tls", blk)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("render %s config: %w", name, err)
	}
	return writeWorkdirFile(name, out)
}

// renderGrafanaTLSConfig appends a [server] section to the embedded default
// grafana.ini switching the UI listener to HTTPS with the mounted server
// cert/key. Used only for the default config; a caller-supplied grafana.ini
// owns its own [server] block.
func renderGrafanaTLSConfig() (*dagger.File, error) {
	certPath, keyPath, _ := tlsPaths(grafanaTLSDir)
	var b bytes.Buffer
	b.Write(defaultGrafanaConfig)
	if n := len(defaultGrafanaConfig); n == 0 || defaultGrafanaConfig[n-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n[server]\nprotocol = https\ncert_file = %s\ncert_key = %s\n", certPath, keyPath)
	return writeWorkdirFile("grafana-tls.ini", b.Bytes())
}

// fileExpansion wraps a container path in Grafana's provisioning file
// provider so secureJsonData reads the PEM material from a mounted file
// (keeping the client key a secret mount) rather than inlining it.
func fileExpansion(path string) string { return "$__file{" + path + "}" }

// datasourceEntry builds one provisioning entry and, when the backend has
// TLS, mounts the client TLS material into ctr and points the entry at it via
// Grafana's $__file{} expansion. dsType is the Grafana datasource type
// ("loki"/"tempo"/"prometheus"); urlPath is appended after host:port. TLS
// state is derived from the backend's own cert material: backendServerCert
// non-nil means the backend has TLS (https URL + pinned CA); backendClientCa
// non-nil means the backend requires mTLS (Grafana must present a client
// cert). caCert overrides the pinned CA; when nil the backend's server
// certificate is pinned instead.
func datasourceEntry(
	ctr *dagger.Container,
	name, dsType, urlPath string,
	port int,
	backendServerCert, backendClientCa, caCert, clientCert *dagger.File,
	clientKey *dagger.Secret,
) (*dagger.Container, map[string]any, error) {
	tls := backendServerCert != nil
	sch := "http"
	if tls {
		sch = "https"
	}
	entry := map[string]any{
		"name":      name,
		"uid":       name,
		"type":      dsType,
		"access":    "proxy",
		"url":       fmt.Sprintf("%s://%s:%d%s", sch, name, port, urlPath),
		"isDefault": false,
	}
	if !tls {
		return ctr, entry, nil
	}

	dsDir := grafanaDsTLSDir + "/" + name
	ca := caCert
	if ca == nil {
		ca = backendServerCert
	}
	caPath := dsDir + "/ca.crt"
	ctr = ctr.WithMountedFile(caPath, ca)

	jsonData := map[string]any{"tlsAuthWithCACert": true}
	secure := map[string]any{"tlsCACert": fileExpansion(caPath)}

	if backendClientCa != nil {
		if clientCert == nil || clientKey == nil {
			return nil, nil, fmt.Errorf(
				"datasource %q points at an mTLS backend but no client certificate/key was supplied to WithXDatasource",
				name)
		}
		certPath := dsDir + "/client.crt"
		keyPath := dsDir + "/client.key"
		ctr = ctr.
			WithMountedFile(certPath, clientCert).
			WithMountedSecret(keyPath, clientKey)
		jsonData["tlsAuth"] = true
		secure["tlsClientCert"] = fileExpansion(certPath)
		secure["tlsClientKey"] = fileExpansion(keyPath)
	}

	entry["jsonData"] = jsonData
	entry["secureJsonData"] = secure
	return ctr, entry, nil
}

// mtlsWithoutTLS reports the misconfiguration where a client CA was supplied
// (WithMtls) without a server certificate (WithTls). Every backend Service
// surfaces this as an error rather than silently ignoring the CA.
func mtlsWithoutTLS(serverCert *dagger.File, clientCa *dagger.File) error {
	if clientCa != nil && serverCert == nil {
		return fmt.Errorf("WithMtls requires WithTls: a client CA was set without a server certificate")
	}
	return nil
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
