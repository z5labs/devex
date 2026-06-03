package main

import (
	"context"

	"dagger/java/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

// MavenCi is a chained builder for a standardized Maven CI pipeline. Construct
// via Maven.Ci(); enable check stages via the With* methods; call Run to
// execute checks-then-package, or Check to run only the parallel checks.
//
// Stage 1 runs the enabled checks (Test, Verify) in parallel via
// github.com/dagger/dagger/util/parallel; errors are aggregated. Stage 2 runs
// `mvn package -DskipTests` (the checks already covered testing) and Run
// returns the produced target/ directory for downstream pipelines to compose.
//
// The builder reuses the parent Maven lifecycle helpers, so wrapper handling,
// JDK inference, and cache mounts are inherited.
type MavenCi struct {
	// +private
	Maven *Maven
	// +private
	TestEnabled bool
	// +private
	VerifyEnabled bool
}

// Ci returns a new pipeline builder bound to this Maven tool object.
func (m *Maven) Ci() *MavenCi {
	return &MavenCi{Maven: m}
}

// WithTest enables the `mvn test` check stage.
func (c *MavenCi) WithTest() *MavenCi {
	c.TestEnabled = true
	return c
}

// WithVerify enables the `mvn verify` check stage.
func (c *MavenCi) WithVerify() *MavenCi {
	c.VerifyEnabled = true
	return c
}

// Check runs the enabled check stages (Test, Verify) in parallel via
// github.com/dagger/dagger/util/parallel and returns the aggregated error. Use
// when callers want to run the checks independently of packaging.
//
// +check
// +cache="session"
func (c *MavenCi) Check(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if c.TestEnabled {
		jobs = jobs.WithJob("test", c.runTest)
	}
	if c.VerifyEnabled {
		jobs = jobs.WithJob("verify", c.runVerify)
	}
	return jobs.Run(ctx)
}

// Run executes the pipeline: stage 1 (Check) → stage 2 (`mvn package
// -DskipTests`). Returns the produced target/ directory. On stage-1 failure,
// returns the aggregated error from Check and a nil directory (packaging is
// skipped).
//
// +check
// +cache="session"
func (c *MavenCi) Run(ctx context.Context) (*dagger.Directory, error) {
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	// Packaging is artifact-only: the check stage already ran the tests, so
	// skip them here (mirrors Gradle's assemble, which never runs tests).
	return c.Maven.Package(ctx, true)
}

func (c *MavenCi) runTest(ctx context.Context) error {
	_, err := c.Maven.Test(ctx)
	return err
}

func (c *MavenCi) runVerify(ctx context.Context) error {
	_, err := c.Maven.Verify(ctx)
	return err
}

// GradleCi is a chained builder for a standardized Gradle CI pipeline.
// Construct via Gradle.Ci(); enable check stages via the With* methods; call
// Run to execute checks-then-assemble, or Check to run only the parallel
// checks.
//
// Stage 1 runs the enabled checks (Test, Check) in parallel via
// github.com/dagger/dagger/util/parallel; errors are aggregated. Stage 2 runs
// `gradle assemble` (which never runs tests) and Run returns the produced
// build/libs directory for downstream pipelines to compose.
//
// The builder reuses the parent Gradle lifecycle helpers, so wrapper handling,
// JDK inference, and cache mounts are inherited.
type GradleCi struct {
	// +private
	Gradle *Gradle
	// +private
	TestEnabled bool
	// +private
	CheckEnabled bool
}

// Ci returns a new pipeline builder bound to this Gradle tool object.
func (g *Gradle) Ci() *GradleCi {
	return &GradleCi{Gradle: g}
}

// WithTest enables the `gradle test` check stage.
func (c *GradleCi) WithTest() *GradleCi {
	c.TestEnabled = true
	return c
}

// WithCheck enables the `gradle check` check stage.
func (c *GradleCi) WithCheck() *GradleCi {
	c.CheckEnabled = true
	return c
}

// Check runs the enabled check stages (Test, Check) in parallel via
// github.com/dagger/dagger/util/parallel and returns the aggregated error. Use
// when callers want to run the checks independently of the build.
//
// +check
// +cache="session"
func (c *GradleCi) Check(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if c.TestEnabled {
		jobs = jobs.WithJob("test", c.runTest)
	}
	if c.CheckEnabled {
		jobs = jobs.WithJob("check", c.runCheck)
	}
	return jobs.Run(ctx)
}

// Run executes the pipeline: stage 1 (Check) → stage 2 (`gradle assemble`).
// Returns the produced build/libs directory. On stage-1 failure, returns the
// aggregated error from Check and a nil directory (the build is skipped).
//
// +check
// +cache="session"
func (c *GradleCi) Run(ctx context.Context) (*dagger.Directory, error) {
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	return c.Gradle.Assemble(ctx)
}

func (c *GradleCi) runTest(ctx context.Context) error {
	_, err := c.Gradle.Test(ctx)
	return err
}

func (c *GradleCi) runCheck(ctx context.Context) error {
	_, err := c.Gradle.Tasks(ctx, []string{"check"}, nil)
	return err
}
