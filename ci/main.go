package main

import (
	"context"

	"github.com/dagger/dagger/util/parallel"
)

type Ci struct{}

// Test runs every daggerverse module's tests/All() check in parallel.
//
// +check
// +cache="session"
func (c *Ci) Test(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)

	jobs = jobs.WithJob("random", dag.RandomTests().All)
	jobs = jobs.WithJob("crypto", dag.CryptoTests().All)
	jobs = jobs.WithJob("certificate-management", dag.CertificateManagementTests().All)
	jobs = jobs.WithJob("grafana-stack", func(ctx context.Context) error {
		return dag.GrafanaStackTests().All(ctx)
	})
	jobs = jobs.WithJob("kafka", func(ctx context.Context) error {
		return dag.KafkaTests().All(ctx)
	})
	jobs = jobs.WithJob("go", func(ctx context.Context) error {
		return dag.GoTests().All(ctx)
	})
	jobs = jobs.WithJob("envoy", func(ctx context.Context) error {
		return dag.EnvoyTests().All(ctx)
	})

	return jobs.Run(ctx)
}
