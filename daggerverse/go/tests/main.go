// Package main implements the test module for the go Dagger module. Each test
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

// All runs every go-module test in parallel. goImageTag is forwarded to
// each per-test as the Go toolchain image tag passed to dag.Go(); an empty
// string preserves the module's default behavior (infer from each fixture's
// go.mod for source-bearing tests, fall back to "latest" otherwise).
//
// Note: ContainerInfersVersionFromGoMod intentionally ignores goImageTag —
// it asserts the empty-version inference path against a 1.23 fixture, so a
// caller-supplied override would defeat what the test is verifying.
//
// parallel caps how many tests run concurrently inside this suite. Defaults
// to 1 (sequential) to mirror `go test` package-level semantics; pass 0 to
// fan out every test with no limit, or any positive integer to opt into a
// specific level of concurrency.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=""
	goImageTag string,
	// +default=1
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}

	jobs = jobs.WithJob("ContainerHasGoToolchain", func(ctx context.Context) error {
		return t.ContainerHasGoToolchain(ctx, goImageTag)
	})
	jobs = jobs.WithJob("ContainerInfersVersionFromGoMod", func(ctx context.Context) error {
		return t.ContainerInfersVersionFromGoMod(ctx, goImageTag)
	})
	jobs = jobs.WithJob("ToolVersionContainsGoVersion", func(ctx context.Context) error {
		return t.ToolVersionContainsGoVersion(ctx, goImageTag)
	})
	jobs = jobs.WithJob("EnvContainsGoroot", func(ctx context.Context) error {
		return t.EnvContainsGoroot(ctx, goImageTag)
	})
	jobs = jobs.WithJob("VetHelloPasses", func(ctx context.Context) error {
		return t.VetHelloPasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("FmtHelloIsClean", func(ctx context.Context) error {
		return t.FmtHelloIsClean(ctx, goImageTag)
	})
	jobs = jobs.WithJob("TestHelloPasses", func(ctx context.Context) error {
		return t.TestHelloPasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildHelloWritesBinary", func(ctx context.Context) error {
		return t.BuildHelloWritesBinary(ctx, goImageTag)
	})
	jobs = jobs.WithJob("RunHelloPrintsHello", func(ctx context.Context) error {
		return t.RunHelloPrintsHello(ctx, goImageTag)
	})
	jobs = jobs.WithJob("GenerateHelloProducesFile", func(ctx context.Context) error {
		return t.GenerateHelloProducesFile(ctx, goImageTag)
	})
	jobs = jobs.WithJob("ModTidyHelloIsIdempotent", func(ctx context.Context) error {
		return t.ModTidyHelloIsIdempotent(ctx, goImageTag)
	})
	jobs = jobs.WithJob("ModDownloadHelloPasses", func(ctx context.Context) error {
		return t.ModDownloadHelloPasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("ModVerifyHelloPasses", func(ctx context.Context) error {
		return t.ModVerifyHelloPasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("WorkInitSucceeds", func(ctx context.Context) error {
		return t.WorkInitSucceeds(ctx, goImageTag)
	})
	jobs = jobs.WithJob("InstallSmallToolReturnsBinary", func(ctx context.Context) error {
		return t.InstallSmallToolReturnsBinary(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildMultipkgDotSlashEllipsis", func(ctx context.Context) error {
		return t.BuildMultipkgDotSlashEllipsis(ctx, goImageTag)
	})
	jobs = jobs.WithJob("TestMultipkgPkgArgVariants", func(ctx context.Context) error {
		return t.TestMultipkgPkgArgVariants(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiRunHelloDefaultsProduceModuleNameBinary", func(ctx context.Context) error {
		return t.CiRunHelloDefaultsProduceModuleNameBinary(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiWithFmtPasses", func(ctx context.Context) error {
		return t.CiWithFmtPasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiWithVetPasses", func(ctx context.Context) error {
		return t.CiWithVetPasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiWithTestPasses", func(ctx context.Context) error {
		return t.CiWithTestPasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiWithTestRacePasses", func(ctx context.Context) error {
		return t.CiWithTestRacePasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiWithBuildCustomBinaryName", func(ctx context.Context) error {
		return t.CiWithBuildCustomBinaryName(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiWithLintPasses", func(ctx context.Context) error {
		return t.CiWithLintPasses(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiRunHelloAllStages", func(ctx context.Context) error {
		return t.CiRunHelloAllStages(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiRunVetBadAggregates", func(ctx context.Context) error {
		return t.CiRunVetBadAggregates(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CiCheckRunsEnabledChecksAndSkipsBuild", func(ctx context.Context) error {
		return t.CiCheckRunsEnabledChecksAndSkipsBuild(ctx, goImageTag)
	})

	return jobs.Run(ctx)
}

// CiCheckRunsEnabledChecksAndSkipsBuild configures every With* stage
// including WithBuild, then calls Check (not Run) and asserts no error.
// Check returns no *dagger.File by signature, so the "build did not run"
// property is enforced at the type level; this test verifies the happy
// path of the check-only terminal against a clean source.
func (t *Tests) CiCheckRunsEnabledChecksAndSkipsBuild(ctx context.Context, goImageTag string) error {
	err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).
		WithFmt().
		WithVet().
		WithLint().
		WithTest(dagger.GoCiWithTestOpts{Race: true}).
		WithBuild().
		Check(ctx)
	if err != nil {
		return fmt.Errorf("Ci.Check on clean hello: %w", err)
	}
	return nil
}

// CiRunVetBadAggregates runs Ci against the vet-bad fixture with both Vet
// and Lint enabled and asserts that stage-1 aggregated BOTH job failures
// rather than short-circuiting on the first. parallel.New concatenates each
// job's raw error (job names appear in trace spans, not the Go-level
// string), so each underlying `withExec` failure surfaces as a separate
// "exit code: 1" line. Counting those occurrences confirms both vet and
// lint ran and both errors were propagated through Run.
func (t *Tests) CiRunVetBadAggregates(ctx context.Context, goImageTag string) error {
	_, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(vetBadDir()).
		WithVet().
		WithLint().
		Run().Size(ctx)
	if err == nil {
		return fmt.Errorf("expected non-nil error from Ci.Run on vet-bad fixture, got nil")
	}
	msg := err.Error()
	if got := strings.Count(msg, "exit code: 1"); got < 2 {
		return fmt.Errorf("expected aggregated error to contain at least 2 \"exit code: 1\" lines (one per failing stage-1 job), got %d: %s", got, msg)
	}
	return nil
}

// CiRunHelloAllStages runs Ci with every stage enabled against the hello
// fixture and asserts a non-empty binary is produced.
func (t *Tests) CiRunHelloAllStages(ctx context.Context, goImageTag string) error {
	size, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).
		WithFmt().
		WithVet().
		WithLint().
		WithTest(dagger.GoCiWithTestOpts{Race: true}).
		WithBuild().
		Run().Size(ctx)
	if err != nil {
		return fmt.Errorf("Ci all-stages Run: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	return nil
}

// CiWithLintPasses runs Ci with the Lint stage enabled against the
// clean hello fixture and asserts a non-empty binary is produced.
// Uses the pinned default golangci-lint version.
func (t *Tests) CiWithLintPasses(ctx context.Context, goImageTag string) error {
	size, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).WithLint().Run().Size(ctx)
	if err != nil {
		return fmt.Errorf("Ci.WithLint.Run: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	return nil
}

// CiWithBuildCustomBinaryName configures a custom binary name via WithBuild
// and asserts the produced File carries that name.
func (t *Tests) CiWithBuildCustomBinaryName(ctx context.Context, goImageTag string) error {
	f := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).
		WithBuild(dagger.GoCiWithBuildOpts{BinaryName: "custom"}).
		Run()
	name, err := f.Name(ctx)
	if err != nil {
		return fmt.Errorf("binary.Name: %w", err)
	}
	if name != "custom" {
		return fmt.Errorf("expected binary name %q, got %q", "custom", name)
	}
	size, err := f.Size(ctx)
	if err != nil {
		return fmt.Errorf("binary.Size: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	return nil
}

// CiWithTestPasses runs Ci with the Test stage enabled (no race) against
// hello and asserts a non-empty binary is produced.
func (t *Tests) CiWithTestPasses(ctx context.Context, goImageTag string) error {
	size, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).WithTest().Run().Size(ctx)
	if err != nil {
		return fmt.Errorf("Ci.WithTest.Run: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	return nil
}

// CiWithTestRacePasses runs Ci with the Test stage enabled with -race and
// asserts a non-empty binary is produced.
func (t *Tests) CiWithTestRacePasses(ctx context.Context, goImageTag string) error {
	size, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).
		WithTest(dagger.GoCiWithTestOpts{Race: true}).
		Run().Size(ctx)
	if err != nil {
		return fmt.Errorf("Ci.WithTest(race).Run: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	return nil
}

// CiWithVetPasses runs Ci with the Vet stage enabled against the vet-clean
// hello fixture and asserts a non-empty binary is produced.
func (t *Tests) CiWithVetPasses(ctx context.Context, goImageTag string) error {
	size, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).WithVet().Run().Size(ctx)
	if err != nil {
		return fmt.Errorf("Ci.WithVet.Run: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	return nil
}

// CiWithFmtPasses runs Ci with the Fmt stage enabled against the
// gofmt-clean hello fixture and asserts a non-empty binary is produced.
func (t *Tests) CiWithFmtPasses(ctx context.Context, goImageTag string) error {
	size, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).WithFmt().Run().Size(ctx)
	if err != nil {
		return fmt.Errorf("Ci.WithFmt.Run: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty binary, got size 0")
	}
	return nil
}

// CiRunHelloDefaultsProduceModuleNameBinary asserts that Ci.Run with no
// builders configured still produces a binary named after the go.mod
// module path (example.com/hello → "hello").
func (t *Tests) CiRunHelloDefaultsProduceModuleNameBinary(ctx context.Context, goImageTag string) error {
	f := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).Run()
	name, err := f.Name(ctx)
	if err != nil {
		return fmt.Errorf("binary.Name: %w", err)
	}
	if name != "hello" {
		return fmt.Errorf("expected binary name %q, got %q", "hello", name)
	}
	size, err := f.Size(ctx)
	if err != nil {
		return fmt.Errorf("binary.Size: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty hello binary, got size 0")
	}
	return nil
}

// helloDir returns the on-disk hello fixture as a *dagger.Directory.
func helloDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/hello")
}

// multipkgDir returns the multi-package fixture (main + pkg/foo subpackage).
func multipkgDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/multipkg")
}

// vetBadDir returns the vet-bad fixture (intentional Printf verb mismatch
// for stage-1 failure aggregation tests).
func vetBadDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/vet-bad")
}

// BuildMultipkgDotSlashEllipsis builds the multipkg fixture with the default
// pkg=./... and asserts the produced multipkg binary is non-empty. Only the
// root main package contributes a binary; pkg/foo is a library.
func (t *Tests) BuildMultipkgDotSlashEllipsis(ctx context.Context, goImageTag string) error {
	out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(multipkgDir())
	size, err := out.File("multipkg").Size(ctx)
	if err != nil {
		return fmt.Errorf("read multipkg binary: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty multipkg binary, got size 0")
	}
	return nil
}

// TestMultipkgPkgArgVariants runs `go test` against the multipkg fixture
// twice — once with pkg=./... (covers the whole module) and once with
// pkg=./pkg/foo (sub-package only) — to confirm the pkg arg shape.
func (t *Tests) TestMultipkgPkgArgVariants(ctx context.Context, goImageTag string) error {
	if _, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Test(ctx, multipkgDir()); err != nil {
		return fmt.Errorf("Test multipkg ./...: %w", err)
	}
	if _, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Test(ctx, multipkgDir(), dagger.GoTestOpts{
		Pkg: "./pkg/foo",
	}); err != nil {
		return fmt.Errorf("Test multipkg ./pkg/foo: %w", err)
	}
	return nil
}

// InstallSmallToolReturnsBinary installs a small public tool (stringer) and
// asserts the returned binary is non-empty. The version is pinned so CI
// doesn't drift with upstream releases. Requires network egress for the
// initial fetch; subsequent runs hit the go-mod-cache.
func (t *Tests) InstallSmallToolReturnsBinary(ctx context.Context, goImageTag string) error {
	size, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Install("golang.org/x/tools/cmd/stringer@v0.45.0").Size(ctx)
	if err != nil {
		return fmt.Errorf("Install stringer: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty stringer binary, got size 0")
	}
	return nil
}

// WorkInitSucceeds runs `go work init .` against the hello fixture and
// asserts no error. `go work init` is a side-effecting subcommand that
// returns empty stdout on success — the assertion is the absence of error.
func (t *Tests) WorkInitSucceeds(ctx context.Context, goImageTag string) error {
	if _, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Work(ctx, helloDir(), "init", dagger.GoWorkOpts{
		Args: []string{"."},
	}); err != nil {
		return fmt.Errorf("Work init: %w", err)
	}
	return nil
}

// ModTidyHelloIsIdempotent runs `go mod tidy` against the stdlib-only hello
// fixture and asserts the resulting go.mod is unchanged.
func (t *Tests) ModTidyHelloIsIdempotent(ctx context.Context, goImageTag string) error {
	original, err := helloDir().File("go.mod").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read original go.mod: %w", err)
	}
	tidied, err := dag.Go(dagger.GoOpts{Version: goImageTag}).ModTidy(helloDir()).File("go.mod").Contents(ctx)
	if err != nil {
		return fmt.Errorf("ModTidy: %w", err)
	}
	if tidied != original {
		return fmt.Errorf("expected go.mod unchanged after tidy.\n--- before:\n%s--- after:\n%s", original, tidied)
	}
	return nil
}

// ModDownloadHelloPasses runs ModDownload against the hello fixture and
// asserts no error.
func (t *Tests) ModDownloadHelloPasses(ctx context.Context, goImageTag string) error {
	if err := dag.Go(dagger.GoOpts{Version: goImageTag}).ModDownload(ctx, helloDir()); err != nil {
		return fmt.Errorf("ModDownload: %w", err)
	}
	return nil
}

// ModVerifyHelloPasses runs ModVerify against the hello fixture and asserts
// no error.
func (t *Tests) ModVerifyHelloPasses(ctx context.Context, goImageTag string) error {
	if err := dag.Go(dagger.GoOpts{Version: goImageTag}).ModVerify(ctx, helloDir()); err != nil {
		return fmt.Errorf("ModVerify: %w", err)
	}
	return nil
}

// GenerateHelloProducesFile runs go generate against the hello fixture and
// asserts the //go:generate directive produced out.txt with the expected
// content.
func (t *Tests) GenerateHelloProducesFile(ctx context.Context, goImageTag string) error {
	dir := dag.Go(dagger.GoOpts{Version: goImageTag}).Generate(helloDir())
	got, err := dir.File("out.txt").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read out.txt: %w", err)
	}
	if got != "generated\n" {
		return fmt.Errorf("expected %q, got %q", "generated\n", got)
	}
	return nil
}

// RunHelloPrintsHello runs the hello fixture's main and asserts stdout is
// "hello\n".
func (t *Tests) RunHelloPrintsHello(ctx context.Context, goImageTag string) error {
	out, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Run(ctx, helloDir(), ".")
	if err != nil {
		return fmt.Errorf("Run hello: %w", err)
	}
	if out != "hello\n" {
		return fmt.Errorf("expected %q, got %q", "hello\n", out)
	}
	return nil
}

// BuildHelloWritesBinary builds the hello fixture into /out and asserts the
// produced "hello" binary is non-empty.
func (t *Tests) BuildHelloWritesBinary(ctx context.Context, goImageTag string) error {
	out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(helloDir(), dagger.GoBuildOpts{
		Pkg:    ".",
		Output: "hello",
	})
	size, err := out.File("hello").Size(ctx)
	if err != nil {
		return fmt.Errorf("read hello binary: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty hello binary, got size 0")
	}
	return nil
}

// TestHelloPasses runs `go test ./...` against the hello fixture and asserts
// the canonical "PASS" marker appears in stdout.
func (t *Tests) TestHelloPasses(ctx context.Context, goImageTag string) error {
	out, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Test(ctx, helloDir())
	if err != nil {
		return fmt.Errorf("Test hello: %w (output: %q)", err, out)
	}
	if !strings.Contains(out, "ok") {
		return fmt.Errorf("expected 'ok' marker in test output, got: %q", out)
	}
	return nil
}

// FmtHelloIsClean runs Fmt against the gofmt-clean hello fixture and asserts
// the diff is empty.
func (t *Tests) FmtHelloIsClean(ctx context.Context, goImageTag string) error {
	out, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Fmt(ctx, helloDir())
	if err != nil {
		return fmt.Errorf("Fmt hello: %w (output: %q)", err, out)
	}
	if out != "" {
		return fmt.Errorf("expected empty gofmt diff, got: %q", out)
	}
	return nil
}

// VetHelloPasses runs Vet against the hello fixture, which is vet-clean,
// so the call must succeed.
func (t *Tests) VetHelloPasses(ctx context.Context, goImageTag string) error {
	if err := dag.Go(dagger.GoOpts{Version: goImageTag}).Vet(ctx, helloDir()); err != nil {
		return fmt.Errorf("Vet hello: %w", err)
	}
	return nil
}

// EnvContainsGoroot calls dag.Go(dagger.GoOpts{Version: goImageTag}).Env and asserts the output mentions GOROOT
// — the canonical signal that `go env` ran inside the prepared container.
func (t *Tests) EnvContainsGoroot(ctx context.Context, goImageTag string) error {
	out, err := dag.Go(dagger.GoOpts{Version: goImageTag}).Env(ctx)
	if err != nil {
		return fmt.Errorf("Env: %w", err)
	}
	if !strings.Contains(out, "GOROOT") {
		return fmt.Errorf("expected 'GOROOT' in output, got: %q", out)
	}
	return nil
}

// ToolVersionContainsGoVersion calls dag.Go(dagger.GoOpts{Version: goImageTag}).ToolVersion and asserts the
// output starts with the canonical "go version" prefix.
func (t *Tests) ToolVersionContainsGoVersion(ctx context.Context, goImageTag string) error {
	out, err := dag.Go(dagger.GoOpts{Version: goImageTag}).ToolVersion(ctx)
	if err != nil {
		return fmt.Errorf("ToolVersion: %w", err)
	}
	if !strings.Contains(out, "go version") {
		return fmt.Errorf("expected 'go version' in output, got: %q", out)
	}
	return nil
}

// ContainerInfersVersionFromGoMod asserts that constructing the module with
// New("") and a fixture whose go.mod declares `go 1.23` actually pulls the
// matching golang:1.23 image — i.e. resolveVersion + go.mod parsing wire
// through to the toolchain selection. Catches regressions in go.mod parsing
// or in the fallback path silently using `latest`.
//
// goImageTag is accepted for signature uniformity (All forwards it to
// every test) but deliberately ignored: this test exercises the
// empty-version inference path, so a caller-supplied override would
// defeat what's being verified.
func (t *Tests) ContainerInfersVersionFromGoMod(ctx context.Context, goImageTag string) error {
	_ = goImageTag
	out, err := dag.Go().
		Container(helloDir()).
		WithExec([]string{"go", "version"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("go version exec: %w", err)
	}
	if !strings.Contains(out, "go1.23") {
		return fmt.Errorf("expected 'go1.23' (from fixture go.mod) in output, got: %q", out)
	}
	return nil
}

// ContainerHasGoToolchain proves the base container is reachable, the source
// is mounted at /src, and the golang image's `go` binary runs. This is the
// canary for every other test — if it fails, the rest can't possibly pass.
func (t *Tests) ContainerHasGoToolchain(ctx context.Context, goImageTag string) error {
	out, err := dag.Go(dagger.GoOpts{Version: goImageTag}).
		Container(helloDir()).
		WithExec([]string{"go", "version"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("go version exec: %w", err)
	}
	if !strings.Contains(out, "go version") {
		return fmt.Errorf("expected 'go version' in stdout, got: %q", out)
	}
	return nil
}
