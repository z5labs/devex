// Package main implements the test module for the zig Dagger module. Each test
// is exposed as a standalone dagger function so it can be invoked individually
// during TDD; All wires them up for parallel execution under `dagger call all`.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	jobs = jobs.WithJob("CcCompilesHelloC", t.CcCompilesHelloC)
	jobs = jobs.WithJob("CcCrossWindowsProducesExe", t.CcCrossWindowsProducesExe)
	jobs = jobs.WithJob("CcRejectsEmptyFiles", t.CcRejectsEmptyFiles)
	jobs = jobs.WithJob("CcRejectsPathOutputName", t.CcRejectsPathOutputName)
	jobs = jobs.WithJob("CxxCompilesHelloCpp", t.CxxCompilesHelloCpp)
	jobs = jobs.WithJob("ObjCopyProducesBinary", t.ObjCopyProducesBinary)
	jobs = jobs.WithJob("ObjCopyProducesIntelHex", t.ObjCopyProducesIntelHex)
	jobs = jobs.WithJob("ObjCopyRejectsUnknownFormat", t.ObjCopyRejectsUnknownFormat)
	jobs = jobs.WithJob("SizeReportsSections", t.SizeReportsSections)
	jobs = jobs.WithJob("SizeRejectsNonElf", t.SizeRejectsNonElf)

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

// cDir returns the C fixture (hello.c) for the Cc tests.
func cDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/c")
}

// cppDir returns the C++ fixture (hello.cpp) for the Cxx tests.
func cppDir() *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/cpp")
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

// CcCompilesHelloC compiles the C fixture for the host with `zig cc` and
// asserts the produced artifact is non-empty.
func (t *Tests) CcCompilesHelloC(ctx context.Context) error {
	size, err := dag.Zig().Cc(cDir(), []string{"hello.c"}).Size(ctx)
	if err != nil {
		return fmt.Errorf("Cc hello.c: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty artifact, got size 0")
	}
	return nil
}

// singleExe builds the single-file fixture into a host ELF executable, reused as
// the input for the ObjCopy and Size tests.
func singleExe() *dagger.File {
	return dag.Zig().BuildExe(singleDir(), "main.zig")
}

// ObjCopyProducesBinary converts the BuildExe ELF to a raw .bin and asserts the
// result is non-empty and no longer carries the ELF magic.
func (t *Tests) ObjCopyProducesBinary(ctx context.Context) error {
	out := dag.Zig().ObjCopy(singleExe(), dagger.ZigObjCopyOpts{Format: "binary"})
	size, err := out.Size(ctx)
	if err != nil {
		return fmt.Errorf("ObjCopy binary: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty .bin, got size 0")
	}
	// A raw .bin holds arbitrary, non-UTF-8 bytes, so File.Contents() (which
	// resolves as a string) is the wrong tool — it can error or mangle the
	// data. Export the file and inspect its magic bytes directly.
	dir, err := os.MkdirTemp(".", "objcopy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "out.bin")
	if _, err := out.Export(ctx, path); err != nil {
		return fmt.Errorf("export .bin: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read .bin: %w", err)
	}
	if bytes.HasPrefix(data, []byte("\x7fELF")) {
		return fmt.Errorf("expected raw binary without ELF magic, but found it")
	}
	return nil
}

// ObjCopyProducesIntelHex converts the BuildExe ELF to Intel HEX and asserts the
// first record begins with ':' (the Intel HEX record start code).
func (t *Tests) ObjCopyProducesIntelHex(ctx context.Context) error {
	contents, err := dag.Zig().ObjCopy(singleExe(), dagger.ZigObjCopyOpts{Format: "hex"}).Contents(ctx)
	if err != nil {
		return fmt.Errorf("ObjCopy hex: %w", err)
	}
	if !strings.HasPrefix(strings.TrimLeft(contents, "\r\n"), ":") {
		return fmt.Errorf("expected Intel HEX output starting with ':', got %.20q", contents)
	}
	return nil
}

// ObjCopyRejectsUnknownFormat asserts ObjCopy rejects an unsupported format
// (e.g. "uf2"). ObjCopy returns a lazy file, so the error surfaces on resolve.
func (t *Tests) ObjCopyRejectsUnknownFormat(ctx context.Context) error {
	_, err := dag.Zig().ObjCopy(singleExe(), dagger.ZigObjCopyOpts{Format: "uf2"}).Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected error for unknown format, got nil")
	}
	if !strings.Contains(err.Error(), "invalid objcopy format") {
		return fmt.Errorf("expected format-validation error, got: %v", err)
	}
	return nil
}

// SizeReportsSections asserts Size on the BuildExe host ELF returns Text > 0 and
// internally consistent Flash/Ram rollups.
func (t *Tests) SizeReportsSections(ctx context.Context) error {
	s := dag.Zig().Size(singleExe())
	text, err := s.Text(ctx)
	if err != nil {
		return fmt.Errorf("Size.Text: %w", err)
	}
	data, err := s.Data(ctx)
	if err != nil {
		return fmt.Errorf("Size.Data: %w", err)
	}
	bss, err := s.Bss(ctx)
	if err != nil {
		return fmt.Errorf("Size.Bss: %w", err)
	}
	flash, err := s.Flash(ctx)
	if err != nil {
		return fmt.Errorf("Size.Flash: %w", err)
	}
	ram, err := s.RAM(ctx)
	if err != nil {
		return fmt.Errorf("Size.Ram: %w", err)
	}
	if text <= 0 {
		return fmt.Errorf("expected Text > 0, got %d", text)
	}
	if flash != text+data {
		return fmt.Errorf("expected Flash == Text+Data (%d), got %d", text+data, flash)
	}
	if ram != data+bss {
		return fmt.Errorf("expected Ram == Data+Bss (%d), got %d", data+bss, ram)
	}
	return nil
}

// SizeRejectsNonElf feeds a raw .bin (produced by ObjCopy) into Size and asserts
// a clear non-ELF error.
func (t *Tests) SizeRejectsNonElf(ctx context.Context) error {
	bin := dag.Zig().ObjCopy(singleExe(), dagger.ZigObjCopyOpts{Format: "binary"})
	_, err := dag.Zig().Size(bin).Text(ctx)
	if err == nil {
		return fmt.Errorf("expected error for non-ELF input, got nil")
	}
	if !strings.Contains(err.Error(), "not a valid ELF file") {
		return fmt.Errorf("expected non-ELF error, got: %v", err)
	}
	return nil
}

// CcCrossWindowsProducesExe cross-compiles the C fixture for
// x86_64-windows-gnu and asserts the artifact carries the requested output
// name. Resolving .Name runs the cross-compile, so a successful Name read also
// proves the cross build succeeded.
func (t *Tests) CcCrossWindowsProducesExe(ctx context.Context) error {
	f := dag.Zig().Cc(cDir(), []string{"hello.c"}, dagger.ZigCcOpts{
		Target:     "x86_64-windows-gnu",
		OutputName: "hello.exe",
	})
	name, err := f.Name(ctx)
	if err != nil {
		return fmt.Errorf("cross exe Name: %w", err)
	}
	if name != "hello.exe" {
		return fmt.Errorf("expected exe name %q, got %q", "hello.exe", name)
	}
	return nil
}

// CcRejectsEmptyFiles asserts Cc rejects an empty files slice. Cc returns a
// lazy file, so the validation error surfaces on resolve.
func (t *Tests) CcRejectsEmptyFiles(ctx context.Context) error {
	_, err := dag.Zig().Cc(cDir(), []string{}).Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected error for empty files, got nil")
	}
	// Assert it's the validation error, not an incidental exec failure, so a
	// regression that drops the up-front check (and instead fails inside zig)
	// doesn't slip through.
	if !strings.Contains(err.Error(), "files is required") {
		return fmt.Errorf("expected files-required validation error, got: %v", err)
	}
	return nil
}

// CcRejectsPathOutputName asserts Cc rejects a path-like outputName (the
// parameter is a bare filename, not a path). The validation error surfaces on
// resolve, before any zig exec runs.
func (t *Tests) CcRejectsPathOutputName(ctx context.Context) error {
	_, err := dag.Zig().Cc(cDir(), []string{"hello.c"}, dagger.ZigCcOpts{
		OutputName: "/tmp/a.out",
	}).Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected error for path-like outputName, got nil")
	}
	if !strings.Contains(err.Error(), "must be a bare filename") {
		return fmt.Errorf("expected bare-filename validation error, got: %v", err)
	}
	return nil
}

// CxxCompilesHelloCpp compiles the C++ fixture for the host with `zig c++` and
// asserts the produced artifact is non-empty.
func (t *Tests) CxxCompilesHelloCpp(ctx context.Context) error {
	size, err := dag.Zig().Cxx(cppDir(), []string{"hello.cpp"}).Size(ctx)
	if err != nil {
		return fmt.Errorf("Cxx hello.cpp: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("expected non-empty artifact, got size 0")
	}
	return nil
}
