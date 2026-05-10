// Package main implements the test module for the go Dagger module. Each test
// is exposed as a standalone dagger function so it can be invoked individually
// during TDD; All wires them up for parallel execution under `dagger call all`.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every go-module test in parallel.
//
// +check
// +cache="session"
func (t *Tests) All(ctx context.Context) error {
	jobs := parallel.New().
		WithRollupLogs(true).
		WithRollupSpans(true)

	jobs = jobs.WithJob("ContainerHasGoToolchain", t.ContainerHasGoToolchain)
	jobs = jobs.WithJob("ToolVersionContainsGoVersion", t.ToolVersionContainsGoVersion)
	jobs = jobs.WithJob("EnvContainsGoroot", t.EnvContainsGoroot)
	jobs = jobs.WithJob("VetHelloPasses", t.VetHelloPasses)
	jobs = jobs.WithJob("FmtHelloIsClean", t.FmtHelloIsClean)
	jobs = jobs.WithJob("TestHelloPasses", t.TestHelloPasses)
	jobs = jobs.WithJob("BuildHelloWritesBinary", t.BuildHelloWritesBinary)
	jobs = jobs.WithJob("RunHelloPrintsHello", t.RunHelloPrintsHello)
	jobs = jobs.WithJob("GenerateHelloProducesFile", t.GenerateHelloProducesFile)
	jobs = jobs.WithJob("ModTidyHelloIsIdempotent", t.ModTidyHelloIsIdempotent)
	jobs = jobs.WithJob("ModDownloadHelloPasses", t.ModDownloadHelloPasses)
	jobs = jobs.WithJob("ModVerifyHelloPasses", t.ModVerifyHelloPasses)
	jobs = jobs.WithJob("WorkInitSucceeds", t.WorkInitSucceeds)
	jobs = jobs.WithJob("InstallSmallToolReturnsBinary", t.InstallSmallToolReturnsBinary)
	jobs = jobs.WithJob("BuildMultipkgDotSlashEllipsis", t.BuildMultipkgDotSlashEllipsis)
	jobs = jobs.WithJob("TestMultipkgPkgArgVariants", t.TestMultipkgPkgArgVariants)

	return jobs.Run(ctx)
}

// helloDir returns the on-disk hello fixture as a *dagger.Directory.
func helloDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/hello")
}

// multipkgDir returns the multi-package fixture (main + pkg/foo subpackage).
func multipkgDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/multipkg")
}

// BuildMultipkgDotSlashEllipsis builds the multipkg fixture with the default
// pkg=./... and asserts the produced multipkg binary is non-empty. Only the
// root main package contributes a binary; pkg/foo is a library.
func (t *Tests) BuildMultipkgDotSlashEllipsis(ctx context.Context) error {
	out := dag.Go().Build(multipkgDir())
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
func (t *Tests) TestMultipkgPkgArgVariants(ctx context.Context) error {
	if _, err := dag.Go().Test(ctx, multipkgDir()); err != nil {
		return fmt.Errorf("Test multipkg ./...: %w", err)
	}
	if _, err := dag.Go().Test(ctx, multipkgDir(), dagger.GoTestOpts{
		Pkg: "./pkg/foo",
	}); err != nil {
		return fmt.Errorf("Test multipkg ./pkg/foo: %w", err)
	}
	return nil
}

// InstallSmallToolReturnsBinary installs a small public tool (stringer) and
// asserts the returned binary is non-empty. Requires network egress for the
// initial fetch; subsequent runs hit the go-mod-cache.
func (t *Tests) InstallSmallToolReturnsBinary(ctx context.Context) error {
	size, err := dag.Go().Install("golang.org/x/tools/cmd/stringer@latest").Size(ctx)
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
func (t *Tests) WorkInitSucceeds(ctx context.Context) error {
	if _, err := dag.Go().Work(ctx, helloDir(), "init", dagger.GoWorkOpts{
		Args: []string{"."},
	}); err != nil {
		return fmt.Errorf("Work init: %w", err)
	}
	return nil
}

// ModTidyHelloIsIdempotent runs `go mod tidy` against the stdlib-only hello
// fixture and asserts the resulting go.mod is unchanged.
func (t *Tests) ModTidyHelloIsIdempotent(ctx context.Context) error {
	original, err := helloDir().File("go.mod").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read original go.mod: %w", err)
	}
	tidied, err := dag.Go().ModTidy(helloDir()).File("go.mod").Contents(ctx)
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
func (t *Tests) ModDownloadHelloPasses(ctx context.Context) error {
	if err := dag.Go().ModDownload(ctx, helloDir()); err != nil {
		return fmt.Errorf("ModDownload: %w", err)
	}
	return nil
}

// ModVerifyHelloPasses runs ModVerify against the hello fixture and asserts
// no error.
func (t *Tests) ModVerifyHelloPasses(ctx context.Context) error {
	if err := dag.Go().ModVerify(ctx, helloDir()); err != nil {
		return fmt.Errorf("ModVerify: %w", err)
	}
	return nil
}

// GenerateHelloProducesFile runs go generate against the hello fixture and
// asserts the //go:generate directive produced out.txt with the expected
// content.
func (t *Tests) GenerateHelloProducesFile(ctx context.Context) error {
	dir := dag.Go().Generate(helloDir())
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
func (t *Tests) RunHelloPrintsHello(ctx context.Context) error {
	out, err := dag.Go().Run(ctx, helloDir(), ".")
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
func (t *Tests) BuildHelloWritesBinary(ctx context.Context) error {
	out := dag.Go().Build(helloDir(), dagger.GoBuildOpts{
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
func (t *Tests) TestHelloPasses(ctx context.Context) error {
	out, err := dag.Go().Test(ctx, helloDir())
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
func (t *Tests) FmtHelloIsClean(ctx context.Context) error {
	out, err := dag.Go().Fmt(ctx, helloDir())
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
func (t *Tests) VetHelloPasses(ctx context.Context) error {
	if err := dag.Go().Vet(ctx, helloDir()); err != nil {
		return fmt.Errorf("Vet hello: %w", err)
	}
	return nil
}

// EnvContainsGoroot calls dag.Go().Env and asserts the output mentions GOROOT
// — the canonical signal that `go env` ran inside the prepared container.
func (t *Tests) EnvContainsGoroot(ctx context.Context) error {
	out, err := dag.Go().Env(ctx)
	if err != nil {
		return fmt.Errorf("Env: %w", err)
	}
	if !strings.Contains(out, "GOROOT") {
		return fmt.Errorf("expected 'GOROOT' in output, got: %q", out)
	}
	return nil
}

// ToolVersionContainsGoVersion calls dag.Go().ToolVersion and asserts the
// output starts with the canonical "go version" prefix.
func (t *Tests) ToolVersionContainsGoVersion(ctx context.Context) error {
	out, err := dag.Go().ToolVersion(ctx)
	if err != nil {
		return fmt.Errorf("ToolVersion: %w", err)
	}
	if !strings.Contains(out, "go version") {
		return fmt.Errorf("expected 'go version' in output, got: %q", out)
	}
	return nil
}

// ContainerHasGoToolchain proves the base container is reachable, the source
// is mounted at /src, and the golang image's `go` binary runs. This is the
// canary for every other test — if it fails, the rest can't possibly pass.
func (t *Tests) ContainerHasGoToolchain(ctx context.Context) error {
	out, err := dag.Go().
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
