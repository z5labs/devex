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
// to 0 (unbounded fan-out) — each `dagger check` job runs on its own GH
// Actions runner, so in-runner parallelism is bounded by the VM's
// CPU/memory, not by the scheduler. Pass any positive integer to opt into
// a specific cap.
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=""
	goImageTag string,
	// +default=0
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
	jobs = jobs.WithJob("BuildRejectsMalformedStamps", func(ctx context.Context) error {
		return t.BuildRejectsMalformedStamps(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildStampsReachTheBinary", func(ctx context.Context) error {
		return t.BuildStampsReachTheBinary(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildTagsSelectTaggedFiles", func(ctx context.Context) error {
		return t.BuildTagsSelectTaggedFiles(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildTrimpathRemovesSourcePaths", func(ctx context.Context) error {
		return t.BuildTrimpathRemovesSourcePaths(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildStripShrinksTheBinary", func(ctx context.Context) error {
		return t.BuildStripShrinksTheBinary(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildPlatformCrossCompiles", func(ctx context.Context) error {
		return t.BuildPlatformCrossCompiles(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildRaceLinksTheDetector", func(ctx context.Context) error {
		return t.BuildRaceLinksTheDetector(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildRejectsRaceWithDisableCgo", func(ctx context.Context) error {
		return t.BuildRejectsRaceWithDisableCgo(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildBuildmodeCArchiveProducesArchiveAndHeader", func(ctx context.Context) error {
		return t.BuildBuildmodeCArchiveProducesArchiveAndHeader(ctx, goImageTag)
	})
	jobs = jobs.WithJob("BuildBuildmodeMembersAllProduceOutput", func(ctx context.Context) error {
		return t.BuildBuildmodeMembersAllProduceOutput(ctx, goImageTag)
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

	jobs = jobs.WithJob("SpdxDocumentIsCompliant", func(ctx context.Context) error {
		return t.SpdxDocumentIsCompliant(ctx, goImageTag)
	})
	jobs = jobs.WithJob("CycloneDxDocumentIsCompliant", func(ctx context.Context) error {
		return t.CycloneDxDocumentIsCompliant(ctx, goImageTag)
	})
	jobs = jobs.WithJob("SbomFormatsAgreeOnComponents", func(ctx context.Context) error {
		return t.SbomFormatsAgreeOnComponents(ctx, goImageTag)
	})
	jobs = jobs.WithJob("SbomResolvesDependencyLicences", func(ctx context.Context) error {
		return t.SbomResolvesDependencyLicences(ctx, goImageTag)
	})
	jobs = jobs.WithJob("SbomDescribesTheBinaryNotTheSourceTree", func(ctx context.Context) error {
		return t.SbomDescribesTheBinaryNotTheSourceTree(ctx, goImageTag)
	})

	jobs = jobs.WithJob("ExamplesCookbook", t.exampleSmoke)

	return jobs.Run(ctx)
}

// exampleSmoke runs every examples/go cookbook recipe end-to-end against its
// built-in sample module, so the suite fails if the examples rot against the
// go API. It is intentionally unexported so it stays out of this module's
// Dagger schema (and the root ci/ bindings); it is driven only as a job in
// All. goImageTag is deliberately not forwarded: the recipes exist to show
// the toolchain being inferred from the sample's go.mod.
func (t *Tests) exampleSmoke(ctx context.Context) error {
	ex := dag.GoExamples()

	binary := ex.BuildBinary()
	name, err := binary.Name(ctx)
	if err != nil {
		return fmt.Errorf("example recipe BuildBinary: %w", err)
	}
	if name != "app" {
		return fmt.Errorf("example recipe BuildBinary: expected binary named %q, got %q", "app", name)
	}
	size, err := binary.Size(ctx)
	if err != nil {
		return fmt.Errorf("example recipe BuildBinary: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("example recipe BuildBinary: expected non-empty binary, got size 0")
	}

	out, err := ex.TestPackage(ctx)
	if err != nil {
		return fmt.Errorf("example recipe TestPackage: %w", err)
	}
	if !strings.Contains(out, "ok") {
		return fmt.Errorf("example recipe TestPackage: expected 'ok' marker in output, got: %q", out)
	}

	if err := exampleModuleHygiene(ctx, ex); err != nil {
		return err
	}

	tool := ex.InstallTool()
	name, err = tool.Name(ctx)
	if err != nil {
		return fmt.Errorf("example recipe InstallTool: %w", err)
	}
	if name != "yamlfmt" {
		return fmt.Errorf("example recipe InstallTool: expected binary named %q, got %q", "yamlfmt", name)
	}
	size, err = tool.Size(ctx)
	if err != nil {
		return fmt.Errorf("example recipe InstallTool: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("example recipe InstallTool: expected non-empty binary, got size 0")
	}

	return nil
}

// exampleModuleHygiene asserts ModuleHygiene returns the checked source tree
// rather than an empty directory: the sample is gofmt-clean, vet-clean and
// depends only on the standard library, so tidy must hand back a tree whose
// go.mod still declares the sample module.
func exampleModuleHygiene(ctx context.Context, ex *dagger.GoExamples) error {
	tidied := ex.ModuleHygiene()

	entries, err := tidied.Entries(ctx)
	if err != nil {
		return fmt.Errorf("example recipe ModuleHygiene: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("example recipe ModuleHygiene: returned an empty directory")
	}

	gomod, err := tidied.File("go.mod").Contents(ctx)
	if err != nil {
		return fmt.Errorf("example recipe ModuleHygiene: read tidied go.mod: %w", err)
	}
	if !strings.Contains(gomod, "module example.com/greeter") {
		return fmt.Errorf("example recipe ModuleHygiene: tidied go.mod lost the sample module directive, got: %q", gomod)
	}
	return nil
}

// CiCheckRunsEnabledChecksAndSkipsBuild configures every With* stage
// against the clean hello fixture and calls Check (not Run), asserting
// no error. To actively prove Check does not invoke the build stage
// internally, WithBuild is configured with a non-existent package path:
// if Check were to call runBuild, `go build ./does-not-exist` would
// fail and surface here as an error. A nil return therefore proves
// both (a) the checks passed and (b) the build was skipped.
func (t *Tests) CiCheckRunsEnabledChecksAndSkipsBuild(ctx context.Context, goImageTag string) error {
	err := dag.Go(dagger.GoOpts{Version: goImageTag}).Ci(helloDir()).
		WithFmt().
		WithVet().
		WithLint().
		WithTest(dagger.GoCiWithTestOpts{Race: true}).
		WithBuild(dagger.GoCiWithBuildOpts{Pkg: "./does-not-exist"}).
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

// stampedDir returns the stamped fixture: a main package whose version and
// commit are package-level vars for `-X` to assign, and whose flavor
// constant is selected by the `fancy` build tag.
func stampedDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/stamped")
}

// carchiveDir returns the carchive fixture: a main package exporting a cgo
// //export function, which is what -buildmode=c-archive and c-shared compile.
func carchiveDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/carchive")
}

// racyDir returns the racy fixture: a main package whose raceDetector
// constant is selected by the `race` build tag, which `go build -race` sets.
func racyDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/racy")
}

// alpineImage is the minimal container built binaries are executed in.
// ":latest" is a moving target, so the tag is pinned.
const alpineImage = "alpine:3.22"

// runBuiltBinary executes a freshly built linux binary in a minimal
// container and returns its stdout.
func runBuiltBinary(ctx context.Context, bin *dagger.File) (string, error) {
	return dag.Container().From(alpineImage).
		WithFile("/bin/app", bin, dagger.ContainerWithFileOpts{Permissions: 0o755}).
		WithExec([]string{"/bin/app"}).
		Stdout(ctx)
}

// runBuiltBinaryOnGlibc executes a freshly built binary in the same
// glibc-based toolchain container it was compiled in, and returns its stdout.
// A -race build links the race runtime through cgo, so its output is
// dynamically linked against glibc and cannot run in the musl-based alpine
// image runBuiltBinary uses. Reusing Container(source) rather than pinning a
// second image keeps the runtime ABI matched to the build's by construction.
func runBuiltBinaryOnGlibc(ctx context.Context, goImageTag string, source *dagger.Directory, bin *dagger.File) (string, error) {
	return dag.Go(dagger.GoOpts{Version: goImageTag}).
		Container(source).
		WithFile("/bin/app", bin, dagger.ContainerWithFileOpts{Permissions: 0o755}).
		WithExec([]string{"/bin/app"}).
		Stdout(ctx)
}

// BuildRejectsMalformedStamps asserts Build rejects a stamp with no "=" and
// a stamp whose importpath.Name is empty, and that each message names the
// offending element rather than reporting a generic parse failure.
func (t *Tests) BuildRejectsMalformedStamps(ctx context.Context, goImageTag string) error {
	for _, stamp := range []string{"main.version", "=v1.0.0"} {
		_, err := dag.Go(dagger.GoOpts{Version: goImageTag}).
			Build(stampedDir(), dagger.GoBuildOpts{
				Pkg:          ".",
				ArtifactName: "stamped",
				Stamps:       []string{stamp},
			}).
			Entries(ctx)
		if err == nil {
			return fmt.Errorf("expected Build to reject stamp %q, got nil error", stamp)
		}
		if !strings.Contains(err.Error(), stamp) {
			return fmt.Errorf("expected error for stamp %q to name it, got: %s", stamp, err.Error())
		}
	}
	return nil
}

// BuildStampsReachTheBinary asserts -X stamps are applied and that a stamp
// value containing "=" arrives unmangled: only the first "=" separates the
// variable name from its value.
func (t *Tests) BuildStampsReachTheBinary(ctx context.Context, goImageTag string) error {
	out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(stampedDir(), dagger.GoBuildOpts{
		Pkg:          ".",
		ArtifactName: "stamped",
		Trimpath:     true,
		Strip:        true,
		DisableCgo:   true,
		Stamps:       []string{"main.version=v1.2.3", "main.commit=sha=deadbeef"},
	})
	got, err := runBuiltBinary(ctx, out.File("stamped"))
	if err != nil {
		return fmt.Errorf("run stamped binary: %w", err)
	}
	const want = "version=v1.2.3 commit=sha=deadbeef flavor=plain\n"
	if got != want {
		return fmt.Errorf("expected %q, got %q", want, got)
	}
	return nil
}

// BuildTagsSelectTaggedFiles asserts tags reaches `go build -tags`: the
// stamped fixture reports flavor=fancy only when the `fancy` tag is set.
func (t *Tests) BuildTagsSelectTaggedFiles(ctx context.Context, goImageTag string) error {
	out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(stampedDir(), dagger.GoBuildOpts{
		Pkg:          ".",
		ArtifactName: "stamped",
		DisableCgo:   true,
		Tags:         []string{"fancy"},
	})
	got, err := runBuiltBinary(ctx, out.File("stamped"))
	if err != nil {
		return fmt.Errorf("run tagged binary: %w", err)
	}
	if !strings.Contains(got, "flavor=fancy") {
		return fmt.Errorf("expected flavor=fancy with the fancy tag, got %q", got)
	}
	return nil
}

// BuildTrimpathRemovesSourcePaths asserts trimpath reaches `go build
// -trimpath`: the build's own /src mount point is recorded in an untrimmed
// binary and absent from a trimmed one.
func (t *Tests) BuildTrimpathRemovesSourcePaths(ctx context.Context, goImageTag string) error {
	for _, tc := range []struct {
		trimpath bool
		want     bool
	}{
		{trimpath: false, want: true},
		{trimpath: true, want: false},
	} {
		out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(stampedDir(), dagger.GoBuildOpts{
			Pkg:          ".",
			ArtifactName: "stamped",
			DisableCgo:   true,
			Trimpath:     tc.trimpath,
		})
		found, err := binaryContains(ctx, out.File("stamped"), "/src/main.go")
		if err != nil {
			return fmt.Errorf("scan binary (trimpath=%v): %w", tc.trimpath, err)
		}
		if found != tc.want {
			return fmt.Errorf("trimpath=%v: expected /src/main.go present=%v, got %v", tc.trimpath, tc.want, found)
		}
	}
	return nil
}

// BuildStripShrinksTheBinary asserts strip reaches `go build -ldflags "-s
// -w"`: dropping the symbol table and DWARF info makes the output smaller.
func (t *Tests) BuildStripShrinksTheBinary(ctx context.Context, goImageTag string) error {
	sizes := make(map[bool]int, 2)
	for _, strip := range []bool{false, true} {
		out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(stampedDir(), dagger.GoBuildOpts{
			Pkg:          ".",
			ArtifactName: "stamped",
			DisableCgo:   true,
			Strip:        strip,
		})
		size, err := out.File("stamped").Size(ctx)
		if err != nil {
			return fmt.Errorf("size (strip=%v): %w", strip, err)
		}
		sizes[strip] = size
	}
	if sizes[true] >= sizes[false] {
		return fmt.Errorf("expected stripped binary to be smaller, got %d stripped vs %d unstripped", sizes[true], sizes[false])
	}
	return nil
}

// BuildBuildmodeCArchiveProducesArchiveAndHeader asserts C_ARCHIVE reaches
// `go build -buildmode=c-archive`: the output is the ar archive plus the
// generated C header, and specifically not an executable — which is what the
// same fixture and the same -o would produce with the buildmode left off.
func (t *Tests) BuildBuildmodeCArchiveProducesArchiveAndHeader(ctx context.Context, goImageTag string) error {
	out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(carchiveDir(), dagger.GoBuildOpts{
		Pkg:          ".",
		ArtifactName: "libgreet.a",
		Buildmode:    dagger.GoBuildModeCArchive,
	})
	entries, err := out.Entries(ctx)
	if err != nil {
		return fmt.Errorf("list /out: %w", err)
	}
	want := []string{"libgreet.a", "libgreet.h"}
	if strings.Join(entries, ",") != strings.Join(want, ",") {
		return fmt.Errorf("expected /out to hold exactly %v, got %v", want, entries)
	}
	// An ar archive opens with "!<arch>\n"; an ELF executable opens with
	// "\x7fELF". Reading the first bytes is what tells the two apart.
	magic, err := dag.Container().From(alpineImage).
		WithFile("/x/libgreet.a", out.File("libgreet.a")).
		WithExec([]string{"head", "-c", "8", "/x/libgreet.a"}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("read archive magic: %w", err)
	}
	if magic != "!<arch>\n" {
		return fmt.Errorf("expected ar magic %q, got %q", "!<arch>\n", magic)
	}
	header, err := out.File("libgreet.h").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read generated header: %w", err)
	}
	if !strings.Contains(header, "Greet") {
		return fmt.Errorf("expected libgreet.h to declare the exported Greet, got:\n%s", header)
	}
	return nil
}

// BuildBuildmodeMembersAllProduceOutput calls Build once per BuildMode member
// with a fixture and an output name that mode can actually satisfy, asserting
// each produces a non-empty artifact. That is what makes every member of the
// enum reachable rather than merely declared: a member the module failed to
// map onto a `-buildmode=` value would fail here.
func (t *Tests) BuildBuildmodeMembersAllProduceOutput(ctx context.Context, goImageTag string) error {
	for _, tc := range []struct {
		mode     dagger.GoBuildMode
		source   *dagger.Directory
		pkg      string
		artifact string
	}{
		// archive ignores main packages, so it gets the library package out
		// of the multipkg fixture.
		{dagger.GoBuildModeArchive, multipkgDir(), "./pkg/foo", "foo.a"},
		{dagger.GoBuildModeCArchive, carchiveDir(), ".", "libgreet.a"},
		{dagger.GoBuildModeCShared, carchiveDir(), ".", "libgreet.so"},
		{dagger.GoBuildModeExe, stampedDir(), ".", "stamped"},
		{dagger.GoBuildModePie, stampedDir(), ".", "stamped"},
		{dagger.GoBuildModePlugin, stampedDir(), ".", "stamped.so"},
	} {
		size, err := dag.Go(dagger.GoOpts{Version: goImageTag}).
			Build(tc.source, dagger.GoBuildOpts{
				Pkg:          tc.pkg,
				ArtifactName: tc.artifact,
				Buildmode:    tc.mode,
			}).
			File(tc.artifact).
			Size(ctx)
		if err != nil {
			return fmt.Errorf("buildmode %s: %w", tc.mode, err)
		}
		if size == 0 {
			return fmt.Errorf("buildmode %s: expected non-empty %s, got size 0", tc.mode, tc.artifact)
		}
	}
	return nil
}

// BuildRaceLinksTheDetector asserts race reaches `go build -race`: the racy
// fixture reports race=on only when the detector is linked in, because
// -race implies the `race` build tag. The binary is run to prove the
// detector is present in the artifact and not merely on the command line.
func (t *Tests) BuildRaceLinksTheDetector(ctx context.Context, goImageTag string) error {
	for _, tc := range []struct {
		race bool
		want string
	}{
		{race: false, want: "race=off\n"},
		{race: true, want: "race=on\n"},
	} {
		out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(racyDir(), dagger.GoBuildOpts{
			Pkg:          ".",
			ArtifactName: "racy",
			Race:         tc.race,
		})
		got, err := runBuiltBinaryOnGlibc(ctx, goImageTag, racyDir(), out.File("racy"))
		if err != nil {
			return fmt.Errorf("run racy binary (race=%v): %w", tc.race, err)
		}
		if got != tc.want {
			return fmt.Errorf("race=%v: expected %q, got %q", tc.race, tc.want, got)
		}
	}
	return nil
}

// BuildRejectsRaceWithDisableCgo asserts Build refuses the race+disableCgo
// pairing up front rather than letting `go build` fail on it, and that the
// message names both inputs so the caller knows which two are in conflict.
func (t *Tests) BuildRejectsRaceWithDisableCgo(ctx context.Context, goImageTag string) error {
	_, err := dag.Go(dagger.GoOpts{Version: goImageTag}).
		Build(racyDir(), dagger.GoBuildOpts{
			Pkg:          ".",
			ArtifactName: "racy",
			Race:         true,
			DisableCgo:   true,
		}).
		Entries(ctx)
	if err == nil {
		return fmt.Errorf("expected Build to reject race with disableCgo, got nil error")
	}
	for _, want := range []string{"race", "disableCgo"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("expected rejection to name %q, got: %s", want, err.Error())
		}
	}
	return nil
}

// BuildPlatformCrossCompiles asserts platform reaches GOOS/GOARCH: the
// binary built for linux/arm64 carries the aarch64 machine type in its ELF
// header (e_machine == 0xb7 at offset 18), which an amd64 build does not.
func (t *Tests) BuildPlatformCrossCompiles(ctx context.Context, goImageTag string) error {
	for _, tc := range []struct{ platform, wantMachine string }{
		{"linux/arm64", "b7"},
		{"linux/amd64", "3e"},
	} {
		out := dag.Go(dagger.GoOpts{Version: goImageTag}).Build(stampedDir(), dagger.GoBuildOpts{
			Pkg:          ".",
			ArtifactName: "stamped",
			DisableCgo:   true,
			Platform:     tc.platform,
		})
		got, err := dag.Container().From(alpineImage).
			WithFile("/bin/app", out.File("stamped")).
			WithExec([]string{"od", "-An", "-t", "x1", "-j", "18", "-N", "1", "/bin/app"}).
			Stdout(ctx)
		if err != nil {
			return fmt.Errorf("read ELF header (%s): %w", tc.platform, err)
		}
		if strings.TrimSpace(got) != tc.wantMachine {
			return fmt.Errorf("%s: expected e_machine %q, got %q", tc.platform, tc.wantMachine, strings.TrimSpace(got))
		}
	}
	return nil
}

// binaryContains reports whether the raw bytes of bin contain needle.
// grep -a treats the binary as text so a match is reported rather than
// swallowed by the "binary file matches" shortcut.
func binaryContains(ctx context.Context, bin *dagger.File, needle string) (bool, error) {
	out, err := dag.Container().From(alpineImage).
		WithFile("/bin/app", bin).
		WithExec([]string{"sh", "-c", `grep -a -q -- "$1" /bin/app && echo yes || echo no`, "sh", needle}).
		Stdout(ctx)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
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
		Pkg:          ".",
		ArtifactName: "hello",
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
