// Package main implements the test module for the zig Dagger module. Each test
// is exposed as a standalone dagger function so it can be invoked individually
// during TDD; All wires them up for parallel execution under `dagger call all`.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// zonVersion is the minimum_zig_version declared by the zon-version fixture's
// build.zig.zon. ContainerInfersVersionFromZon asserts the toolchain selected
// by the inference path reports this version, and it is deliberately a
// different patch than the module's pinned default so the test proves
// inference rather than the fallback.
const zonVersion = "0.14.0"

// All runs every zig-module test in parallel.
//
// parallel caps how many tests run concurrently inside this suite. Defaults to
// 0 (unbounded fan-out) — each `dagger check` job runs on its own GH Actions
// runner, so in-runner parallelism is bounded by the VM's CPU/memory, not by
// the scheduler. Pass any positive integer to opt into a specific cap.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("ContainerHasZigToolchain", t.ContainerHasZigToolchain)
	jobs = jobs.WithJob("ContainerInfersVersionFromZon", t.ContainerInfersVersionFromZon)
	jobs = jobs.WithJob("ToolVersionReturnsVersion", t.ToolVersionReturnsVersion)
	jobs = jobs.WithJob("EnvContainsVersionKey", t.EnvContainsVersionKey)
	jobs = jobs.WithJob("TargetsListsKnownArch", t.TargetsListsKnownArch)
	jobs = jobs.WithJob("BuildHelloProducesBinary", t.BuildHelloProducesBinary)
	jobs = jobs.WithJob("BuildOptimizeReleaseSmall", t.BuildOptimizeReleaseSmall)
	jobs = jobs.WithJob("BuildCrossTargetProducesBinary", t.BuildCrossTargetProducesBinary)
	jobs = jobs.WithJob("BuildRejectsInvalidOptimize", t.BuildRejectsInvalidOptimize)
	jobs = jobs.WithJob("BuildExeProducesExecutable", t.BuildExeProducesExecutable)
	jobs = jobs.WithJob("BuildExeRejectsEmptyRoot", t.BuildExeRejectsEmptyRoot)
	jobs = jobs.WithJob("TestHelloBuildStepPasses", t.TestHelloBuildStepPasses)
	jobs = jobs.WithJob("TestDirectFilePasses", t.TestDirectFilePasses)
	jobs = jobs.WithJob("RunHelloPrintsHello", t.RunHelloPrintsHello)
	jobs = jobs.WithJob("FmtHelloIsClean", t.FmtHelloIsClean)
	jobs = jobs.WithJob("FmtUnformattedReportsFile", t.FmtUnformattedReportsFile)

	return jobs.Run(ctx)
}

// helloDir returns the on-disk hello fixture (full project: build.zig,
// build.zig.zon, src/main.zig) as a *dagger.Directory.
func helloDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/hello")
}

// zonVersionDir returns the version-inference fixture: a build.zig.zon
// declaring minimum_zig_version = zonVersion and nothing else.
func zonVersionDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/zon-version")
}

// singleDir returns the single-file fixture (main.zig with a test block) for
// BuildExe and direct-file Test.
func singleDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/single")
}

// unformattedDir returns the fixture containing an intentionally unformatted
// bad.zig that `zig fmt --check` must report.
func unformattedDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/unformatted")
}

// ContainerHasZigToolchain proves the base container is reachable, the
// downloaded toolchain is on PATH, the source is mounted at /src, and `zig`
// runs. This is the canary for every other test — if it fails, the rest can't
// possibly pass.
func (t *Tests) ContainerHasZigToolchain(ctx context.Context) error {
	out, err := dag.Zig().Container(helloDir()).
		WithExec([]string{"zig", "version"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("zig version exec: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("expected non-empty zig version output")
	}
	return nil
}

// ContainerInfersVersionFromZon asserts that constructing the module with
// New("") and a fixture whose build.zig.zon declares
// minimum_zig_version = zonVersion actually downloads the matching toolchain —
// i.e. resolveVersion + ZON parsing wire through to toolchain selection.
// zonVersion is a different patch than the pinned default, so a match proves
// inference rather than the fallback.
func (t *Tests) ContainerInfersVersionFromZon(ctx context.Context) error {
	out, err := dag.Zig().Container(zonVersionDir()).
		WithExec([]string{"zig", "version"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("zig version exec: %w", err)
	}
	if !strings.Contains(out, zonVersion) {
		return fmt.Errorf("expected %q (from build.zig.zon) in output, got %q", zonVersion, out)
	}
	return nil
}

// ToolVersionReturnsVersion calls the source-less ToolVersion and asserts it
// returns a dotted version string.
func (t *Tests) ToolVersionReturnsVersion(ctx context.Context) error {
	out, err := dag.Zig().ToolVersion(ctx)
	if err != nil {
		return fmt.Errorf("ToolVersion: %w", err)
	}
	if !strings.Contains(out, ".") {
		return fmt.Errorf("expected a dotted version string, got %q", out)
	}
	return nil
}

// EnvContainsVersionKey calls the source-less Env and asserts the `zig env`
// JSON contains the version key.
func (t *Tests) EnvContainsVersionKey(ctx context.Context) error {
	out, err := dag.Zig().Env(ctx)
	if err != nil {
		return fmt.Errorf("Env: %w", err)
	}
	if !strings.Contains(out, "version") {
		return fmt.Errorf("expected 'version' key in zig env output, got %q", out)
	}
	return nil
}

// TargetsListsKnownArch calls the source-less Targets and asserts a known
// architecture appears in the output.
func (t *Tests) TargetsListsKnownArch(ctx context.Context) error {
	out, err := dag.Zig().Targets(ctx)
	if err != nil {
		return fmt.Errorf("Targets: %w", err)
	}
	if !strings.Contains(out, "x86_64") && !strings.Contains(out, "aarch64") {
		return fmt.Errorf("expected a known arch (x86_64/aarch64) in zig targets output")
	}
	return nil
}

// BuildHelloProducesBinary builds the hello fixture for the host and asserts
// the installed executable (zig-out/bin/hello) is non-empty.
func (t *Tests) BuildHelloProducesBinary(ctx context.Context) error {
	size, err := dag.Zig().Build(helloDir()).File("bin/hello").Size(ctx)
	if err != nil {
		return fmt.Errorf("read zig-out/bin/hello: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty hello binary, got size 0")
	}
	return nil
}

// BuildOptimizeReleaseSmall builds the hello fixture with
// -Doptimize=ReleaseSmall and asserts an executable is produced.
func (t *Tests) BuildOptimizeReleaseSmall(ctx context.Context) error {
	size, err := dag.Zig().Build(helloDir(), dagger.ZigBuildOpts{Optimize: "ReleaseSmall"}).
		File("bin/hello").Size(ctx)
	if err != nil {
		return fmt.Errorf("read ReleaseSmall binary: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	return nil
}

// BuildCrossTargetProducesBinary cross-compiles the hello fixture for
// aarch64-linux and asserts an artifact is produced. The binary is not
// host-runnable, so only its size is checked.
func (t *Tests) BuildCrossTargetProducesBinary(ctx context.Context) error {
	size, err := dag.Zig().Build(helloDir(), dagger.ZigBuildOpts{Target: "aarch64-linux"}).
		File("bin/hello").Size(ctx)
	if err != nil {
		return fmt.Errorf("read cross-compiled binary: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty cross-compiled binary, got size 0")
	}
	return nil
}

// BuildRejectsInvalidOptimize asserts Build rejects an invalid optimize value.
// Build returns a lazy directory, so the validation error surfaces on resolve.
func (t *Tests) BuildRejectsInvalidOptimize(ctx context.Context) error {
	_, err := dag.Zig().Build(helloDir(), dagger.ZigBuildOpts{Optimize: "Turbo"}).Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected error for invalid optimize, got nil")
	}
	return nil
}

// BuildExeProducesExecutable builds the single-file fixture via build-exe and
// asserts the produced file is named "main" and non-empty.
func (t *Tests) BuildExeProducesExecutable(ctx context.Context) error {
	f := dag.Zig().BuildExe(singleDir(), "main.zig")
	name, err := f.Name(ctx)
	if err != nil {
		return fmt.Errorf("exe Name: %w", err)
	}
	if name != "main" {
		return fmt.Errorf("expected exe name %q, got %q", "main", name)
	}
	size, err := f.Size(ctx)
	if err != nil {
		return fmt.Errorf("exe Size: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty executable, got size 0")
	}
	return nil
}

// BuildExeRejectsEmptyRoot asserts BuildExe rejects an empty root. BuildExe
// returns a lazy file, so the error surfaces on resolve.
func (t *Tests) BuildExeRejectsEmptyRoot(ctx context.Context) error {
	_, err := dag.Zig().BuildExe(singleDir(), "").Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected error for empty root, got nil")
	}
	return nil
}

// TestHelloBuildStepPasses runs `zig build test` against the hello fixture and
// asserts it succeeds.
func (t *Tests) TestHelloBuildStepPasses(ctx context.Context) error {
	if _, err := dag.Zig().Test(ctx, helloDir()); err != nil {
		return fmt.Errorf("Test hello build step: %w", err)
	}
	return nil
}

// TestDirectFilePasses runs `zig test main.zig` against the single-file
// fixture and asserts it succeeds.
func (t *Tests) TestDirectFilePasses(ctx context.Context) error {
	if _, err := dag.Zig().Test(ctx, singleDir(), dagger.ZigTestOpts{Root: "main.zig"}); err != nil {
		return fmt.Errorf("Test direct file: %w", err)
	}
	return nil
}

// RunHelloPrintsHello runs the hello fixture and asserts its stdout contains
// "hello".
func (t *Tests) RunHelloPrintsHello(ctx context.Context) error {
	out, err := dag.Zig().Run(ctx, helloDir())
	if err != nil {
		return fmt.Errorf("Run hello: %w", err)
	}
	if !strings.Contains(out, "hello") {
		return fmt.Errorf("expected 'hello' in output, got %q", out)
	}
	return nil
}

// FmtHelloIsClean runs Fmt against the fmt-clean hello fixture and asserts no
// error is returned.
func (t *Tests) FmtHelloIsClean(ctx context.Context) error {
	if err := dag.Zig().Fmt(ctx, helloDir()); err != nil {
		return fmt.Errorf("Fmt hello: %w", err)
	}
	return nil
}

// FmtUnformattedReportsFile runs Fmt against the unformatted fixture and
// asserts it returns an error naming the offending file. Fmt surfaces the
// offending paths only via the error (it returns error alone, since a Dagger
// function's non-error return value is dropped at the GraphQL boundary when it
// also returns a non-nil error).
func (t *Tests) FmtUnformattedReportsFile(ctx context.Context) error {
	err := dag.Zig().Fmt(ctx, unformattedDir())
	if err == nil {
		return fmt.Errorf("expected Fmt error for unformatted fixture, got nil")
	}
	if !strings.Contains(err.Error(), "bad.zig") {
		return fmt.Errorf("expected 'bad.zig' in fmt error, got %q", err.Error())
	}
	return nil
}
