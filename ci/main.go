package main

import (
	"context"

	"github.com/dagger/dagger/util/parallel"
)

type Ci struct{}

// All runs every daggerverse module's tests/All() check in parallel.
//
// +check
// +cache="session"
func (c *Ci) All(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)

	jobs = jobs.WithJob("random", func(ctx context.Context) error {
		return dag.RandomTests().All(ctx)
	})

	return jobs.Run(ctx)
}
