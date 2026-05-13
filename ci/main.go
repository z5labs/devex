package main

import (
	"context"
	"runtime"

	par "github.com/dagger/dagger/util/parallel"
)

type Ci struct{}

// Test runs every daggerverse module's tests/All() check across the suite.
//
// Concurrency mirrors `go test`: at most `parallel` module suites run
// concurrently (analog of `-p`), and each suite runs its own tests
// sequentially by default. To opt into more inner parallelism, call a
// single suite directly, e.g.
//
//	dagger call kafka-tests all --parallel=8
//
// parallel defaults to 0, which is interpreted as runtime.NumCPU() — the
// number of CPUs visible to the module runtime container. Pass an explicit
// positive integer to override the auto-detected value.
//
// +check
// +cache="session"
func (c *Ci) Test(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	if parallel <= 0 {
		parallel = runtime.NumCPU()
	}

	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true).
		WithLimit(parallel)

	jobs = jobs.WithJob("random", func(ctx context.Context) error {
		return dag.RandomTests().All(ctx)
	})
	jobs = jobs.WithJob("crypto", func(ctx context.Context) error {
		return dag.CryptoTests().All(ctx)
	})
	jobs = jobs.WithJob("certificate-management", func(ctx context.Context) error {
		return dag.CertificateManagementTests().All(ctx)
	})
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
