package main

import (
	"context"

	"dagger/zig/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

// Ci is a chained builder for a standardized Zig CI pipeline. Construct via
// Zig.Ci(source); enable check stages via the With* methods; call Run to
// execute checks-then-build, or Check to run only the parallel checks.
//
// Stage 1 runs the enabled static checks in parallel (Fmt, Test); errors are
// aggregated. Stage 2 builds the source and Run returns the produced zig-out
// directory. Downstream consumers compose that directory into their own
// pipelines (package, sign, publish, ...).
type Ci struct {
	// +private
	Zig *Zig
	// +private
	Source *dagger.Directory

	// +private
	FmtEnabled bool
	// +private
	TestEnabled bool
	// +private
	TestRoot string

	// +private
	BuildOptimize string
	// +private
	BuildTarget string
	// +private
	BuildSteps []string
}

// Ci returns a new pipeline builder bound to the supplied source.
func (z *Zig) Ci(source *dagger.Directory) *Ci {
	return &Ci{Zig: z, Source: source}
}

// WithFmt enables the `zig fmt --check` check stage.
func (c *Ci) WithFmt() *Ci {
	c.FmtEnabled = true
	return c
}

// WithTest enables the test check stage. root maps onto Zig.Test's optional
// root: empty runs `zig build test`; non-empty runs `zig test <root>`.
func (c *Ci) WithTest(
	// +optional
	root string,
) *Ci {
	c.TestEnabled = true
	c.TestRoot = root
	return c
}

// WithBuild configures the build stage parameters (forwarded to Zig.Build).
// optimize, when non-empty, must be one of Debug, ReleaseSafe, ReleaseFast,
// ReleaseSmall; target sets -Dtarget; steps names build steps. Build is always
// executed by Run regardless of whether this method is called.
func (c *Ci) WithBuild(
	// +optional
	optimize string,
	// +optional
	target string,
	// +optional
	steps []string,
) *Ci {
	c.BuildOptimize = optimize
	c.BuildTarget = target
	c.BuildSteps = steps
	return c
}

// Check runs the enabled check stages (Fmt, Test) in parallel via
// github.com/dagger/dagger/util/parallel and returns the aggregated error. Use
// when callers want to run the checks independently of the build (for example
// multi-target pipelines that share one check run across N target builds).
//
// +check
// +cache="session"
func (c *Ci) Check(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if c.FmtEnabled {
		jobs = jobs.WithJob("fmt", c.runFmt)
	}
	if c.TestEnabled {
		jobs = jobs.WithJob("test", c.runTest)
	}
	return jobs.Run(ctx)
}

// Run executes the pipeline: stage 1 (Check) → stage 2 (build). Returns the
// produced zig-out directory. On stage-1 failure, returns the aggregated error
// from Check and a nil directory (stage 2 is skipped).
//
// +check
// +cache="session"
func (c *Ci) Run(ctx context.Context) (*dagger.Directory, error) {
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	return c.runBuild(ctx)
}

func (c *Ci) runFmt(ctx context.Context) error {
	return c.Zig.Fmt(ctx, c.Source)
}

func (c *Ci) runTest(ctx context.Context) error {
	_, err := c.Zig.Test(ctx, c.Source, c.TestRoot, nil)
	return err
}

func (c *Ci) runBuild(ctx context.Context) (*dagger.Directory, error) {
	return c.Zig.Build(ctx, c.Source, c.BuildOptimize, c.BuildTarget, c.BuildSteps, nil)
}
