// Package main is the grafana-stack-examples Dagger module: a runnable
// cookbook of grafana-stack recipes. Each recipe wires up one shape of the
// stack — a single backend, or Loki + Tempo + Mimir behind a provisioned
// Grafana — and returns the endpoint a client would point at, so a whole
// shape can be exercised with a single `dagger call`.
//
// Read them in order: the three single-backend recipes show what one signal
// costs, FullStackWithGrafana shows how the datasource builders stitch the
// backends into one Grafana, and GrafanaWithDashboard layers provisioned
// dashboard JSON on top.
package main

import (
	"context"
	_ "embed"
	"fmt"

	"dagger/grafana-stack-examples/internal/dagger"
)

// GrafanaStackExamples is the module's main object: a namespace for the
// grafana-stack usage recipes.
type GrafanaStackExamples struct{}

// sampleDashboard is the dashboard JSON provisioned by GrafanaWithDashboard.
// Embedded at build time so the recipe never reaches out to the network, and
// so its panels' datasource uids stay in lockstep with the datasource names
// below.
//
//go:embed dashboards/otel-overview.json
var sampleDashboard string

// Datasource names used by the full-stack recipes. Each doubles as the
// in-network hostname Grafana reaches the backend on and as the Grafana
// datasource uid, so these are the uids the embedded dashboard's panels
// reference.
const (
	lokiDatasource  = "loki"
	tempoDatasource = "tempo"
	mimirDatasource = "mimir"
)

// examplesAdminPassword is the Grafana admin password the full-stack recipes
// provision. A recipe is meant to be opened in a browser, so it is a
// documented throwaway constant rather than random bytes nobody can read
// back: log in with admin / admin. Never do this outside a dev sandbox.
const examplesAdminPassword = "admin"

// LokiOnly spins up a lone Loki on its defaults and returns the URL of its
// OTLP/HTTP logs receiver. This is the smallest useful stack: point an
// OpenTelemetry log exporter at the returned endpoint and records land in
// Loki, no Grafana required.
//
// +cache="never"
func (m *GrafanaStackExamples) LokiOnly(ctx context.Context) (string, error) {
	endpoint, err := dag.GrafanaStack().Loki().OtlpHTTPEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve loki otlp/http endpoint: %w", err)
	}
	return endpoint, nil
}

// TempoOnly spins up a lone Tempo and returns the address of its OTLP/gRPC
// trace receiver. Note the shape difference from the Loki and Mimir recipes:
// gRPC callers want a bare host:port with no URL scheme, which is exactly
// what OtlpGrpcEndpoint hands back.
//
// +cache="never"
func (m *GrafanaStackExamples) TempoOnly(ctx context.Context) (string, error) {
	endpoint, err := dag.GrafanaStack().Tempo().OtlpGrpcEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve tempo otlp/grpc endpoint: %w", err)
	}
	return endpoint, nil
}

// MimirOnly spins up a lone Mimir in monolithic mode and returns the URL of
// its OTLP/HTTP metrics receiver. Multitenancy is off in the default config,
// so an exporter can push to the returned endpoint without an X-Scope-OrgID
// header.
//
// +cache="never"
func (m *GrafanaStackExamples) MimirOnly(ctx context.Context) (string, error) {
	endpoint, err := dag.GrafanaStack().Mimir().OtlpHTTPEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve mimir otlp/http endpoint: %w", err)
	}
	return endpoint, nil
}

// FullStackWithGrafana wires all three backends into one Grafana with a
// provisioned datasource each and returns the Grafana UI endpoint (log in
// with admin / admin). This is the recipe to copy for a local observability
// sandbox: the WithXDatasource builders bind each backend into Grafana's
// network and register it, so no datasource has to be clicked together in the
// UI after startup.
//
// +cache="never"
func (m *GrafanaStackExamples) FullStackWithGrafana(ctx context.Context) (string, error) {
	endpoint, err := fullStack().Endpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve grafana endpoint: %w", err)
	}
	return endpoint, nil
}

// GrafanaWithDashboard is FullStackWithGrafana plus a sample dashboard
// registered via WithDashboard, and returns the Grafana UI endpoint (log in
// with admin / admin, then find "OTel Overview" in the dashboard list). It
// shows the contract dashboard provisioning relies on: every panel addresses
// its datasource by uid, and that uid is the name the datasource was
// registered under — so a dashboard authored against "loki" / "tempo" /
// "mimir" resolves the moment Grafana starts.
//
// +cache="never"
func (m *GrafanaStackExamples) GrafanaWithDashboard(ctx context.Context) (string, error) {
	endpoint, err := fullStack().
		WithDashboard("otel-overview", dashboardFile()).
		Endpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve grafana endpoint: %w", err)
	}
	return endpoint, nil
}

// fullStack builds the Loki + Tempo + Mimir + Grafana wiring shared by the
// two Grafana recipes: one backend per signal, each registered as a
// datasource under its own name.
func fullStack() *dagger.GrafanaStackGrafana {
	stack := dag.GrafanaStack()
	adminPassword := dag.SetSecret("grafana-examples-admin-password", examplesAdminPassword)

	return stack.
		Grafana(adminPassword).
		WithLokiDatasource(lokiDatasource, stack.Loki()).
		WithTempoDatasource(tempoDatasource, stack.Tempo()).
		WithMimirDatasource(mimirDatasource, stack.Mimir())
}

// dashboardFile stages the embedded dashboard JSON as the *dagger.File that
// WithDashboard takes. Staging through a Directory keeps the bytes in-engine;
// nothing is read from the host at call time.
func dashboardFile() *dagger.File {
	const name = "otel-overview.json"
	return dag.Directory().WithNewFile(name, sampleDashboard).File(name)
}
