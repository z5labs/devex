package main

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"

	"dagger/go/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

const (
	// defaultGolangciLintVersion is the golangci-lint release the lint
	// stage installs when the caller pins nothing. This is the single
	// place the pin is stated: bumping it is one edit, and nothing —
	// not a bundled config, not an adopter's `.golangci.yml` — repeats
	// the number.
	//
	// Its major version is load-bearing beyond the pin, because it is
	// also the configuration dialect. golangci-lint v1 does not ignore
	// a v2 config file, it refuses it outright ("you are using a
	// configuration file for golangci-lint v2 with golangci-lint v1"),
	// and v2 refuses a v1 file the same way. A v2 pin therefore means
	// every `.golangci.yml` reaching this stage must open with
	// `version: "2"`.
	defaultGolangciLintVersion = "v2.12.2"

	golangciLintConfigMountPath = "/tmp/.golangci.yml"

	golangciLintModulePath = "github.com/golangci/golangci-lint"
)

// Ci is a chained builder for a standardized Go CI pipeline. Construct via
// Go.Ci(source); enable stages via the With* methods; call Run to execute
// checks-then-build, or Check to run only the parallel checks.
//
// Stage 1 runs the enabled static checks in parallel (Fmt, Vet, Lint, Test);
// errors are aggregated. Stage 2 builds the source and Run returns the
// produced binary as a *dagger.File. Downstream consumers compose that file
// into their own pipelines (package, sign, publish, ...).
type Ci struct {
	// +private
	Go *Go
	// +private
	Source *dagger.Directory

	// +private
	FmtEnabled bool
	// +private
	VetEnabled bool
	// +private
	LintEnabled bool
	// +private
	LintVersion string
	// +private
	LintConfig *dagger.File
	// +private
	TestEnabled bool
	// +private
	TestRace bool

	// +private
	BuildPkg string
	// +private
	BuildBinaryName string
}

// Ci returns a new pipeline builder bound to the supplied source.
func (g *Go) Ci(source *dagger.Directory) *Ci {
	return &Ci{Go: g, Source: source}
}

// WithFmt enables the gofmt check stage.
func (c *Ci) WithFmt() *Ci {
	c.FmtEnabled = true
	return c
}

// WithVet enables the `go vet ./...` check stage.
func (c *Ci) WithVet() *Ci {
	c.VetEnabled = true
	return c
}

// WithLint enables the golangci-lint check stage. version pins the
// installed golangci-lint version (defaults to defaultGolangciLintVersion
// when empty). config, if non-nil, is mounted at golangciLintConfigMountPath
// and passed to golangci-lint via --config.
//
// The default is a golangci-lint **v2** release, so a config passed here
// must be written in the v2 dialect — a file opening with `version: "2"`.
// A v1 file is not tolerated by a v2 binary; it is rejected before any
// linter runs. Pass a `v1.x` version to roll the whole stage back, config
// dialect included; the module path installed follows the version's major,
// so both majors are reachable without forking this pipeline.
func (c *Ci) WithLint(
	// +optional
	version string,
	// +optional
	config *dagger.File,
) *Ci {
	c.LintEnabled = true
	c.LintVersion = version
	c.LintConfig = config
	return c
}

// WithTest enables the `go test ./...` check stage. Pass race=true to
// enable the data-race detector.
func (c *Ci) WithTest(
	// +optional
	race bool,
) *Ci {
	c.TestEnabled = true
	c.TestRace = race
	return c
}

// WithBuild configures the build stage parameters. pkg defaults to "."
// when empty; binaryName defaults to the basename of the `module` directive
// in go.mod when empty. Build is always executed by Run regardless of
// whether this method is called.
//
// Note: the binary-name flag is called binaryName (CLI: --binary-name) to
// avoid colliding with Dagger CLI's top-level --output/-o flag.
func (c *Ci) WithBuild(
	// +optional
	pkg string,
	// +optional
	binaryName string,
) *Ci {
	c.BuildPkg = pkg
	c.BuildBinaryName = binaryName
	return c
}

// Check runs the enabled check stages (Fmt, Vet, Lint, Test) in
// parallel via github.com/dagger/dagger/util/parallel and returns the
// aggregated error. Use when callers want to run the checks
// independently of the build (for example multi-platform pipelines
// that share one check run across N platform builds).
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
	if c.VetEnabled {
		jobs = jobs.WithJob("vet", c.runVet)
	}
	if c.TestEnabled {
		jobs = jobs.WithJob("test", c.runTest)
	}
	if c.LintEnabled {
		jobs = jobs.WithJob("lint", c.runLint)
	}
	return jobs.Run(ctx)
}

// Run executes the pipeline: stage 1 (Check) → stage 2 (build). Returns
// the built binary as a *dagger.File. On stage-1 failure, returns the
// aggregated error from Check and a nil file (stage 2 is skipped).
//
// +check
// +cache="session"
func (c *Ci) Run(ctx context.Context) (*dagger.File, error) {
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	return c.runBuild(ctx)
}

func (c *Ci) runFmt(ctx context.Context) error {
	_, err := c.Go.Fmt(ctx, c.Source)
	return err
}

func (c *Ci) runVet(ctx context.Context) error {
	return c.Go.Vet(ctx, c.Source, "./...")
}

func (c *Ci) runTest(ctx context.Context) error {
	_, err := c.Go.Test(ctx, c.Source, "./...", c.TestRace, nil)
	return err
}

func (c *Ci) runLint(ctx context.Context) error {
	version := c.LintVersion
	if version == "" {
		version = defaultGolangciLintVersion
	}
	pkg, err := golangciLintPkg(version)
	if err != nil {
		return err
	}
	// Build golangci-lint in a source-less container so the install
	// dedupes across fixtures. Mounting it into the lint container as
	// a file keeps the source mount (and per-fixture cache key) off
	// the install span — see plan for the trace that motivated this.
	//
	// The toolchain used is deliberately not c.Go's. golangci-lint's own
	// go.mod tracks the newest Go release (v2 needs 1.25), the official
	// golang images set GOTOOLCHAIN=local so there is no automatic
	// upgrade, and a project pinned to an older Go therefore could not
	// build the linter at all. The linter is a tool, not part of the
	// project's build: analysing a `go 1.23` module with a binary built
	// by a newer toolchain is exactly what upstream's prebuilt releases
	// do. Keeping the install off c.Go's pin also means every caller
	// shares one install regardless of what they pinned.
	lintBin, err := new(Go).Install(pkg)
	if err != nil {
		return err
	}
	ctr, err := c.Go.Container(ctx, c.Source)
	if err != nil {
		return err
	}
	ctr = ctr.WithFile("/usr/local/bin/golangci-lint", lintBin)
	args := []string{"golangci-lint", "run"}
	if c.LintConfig != nil {
		ctr = ctr.WithMountedFile(golangciLintConfigMountPath, c.LintConfig)
		args = append(args, "--config", golangciLintConfigMountPath)
	}
	args = append(args, "./...")
	_, err = ctr.WithExec(args).Sync(ctx)
	return err
}

// golangciLintPkg returns the `go install` argument that builds the
// requested golangci-lint release.
//
// The import path is a function of the version, not a constant: Go's
// semantic import versioning means golangci-lint's module path gained a
// `/v2` element at v2.0.0, so `.../cmd/golangci-lint@v2.12.2` on the v1
// path resolves to nothing. Deriving the element from the version is what
// lets a caller pin either major — roll forward, or roll back to a v1
// release along with a v1 config — without forking this pipeline.
func golangciLintPkg(version string) (string, error) {
	major, err := majorFromVersion(version)
	if err != nil {
		return "", fmt.Errorf("golangci-lint version %q: %w", version, err)
	}
	path := golangciLintModulePath
	if major >= 2 {
		path += "/v" + strconv.Itoa(major)
	}
	return path + "/cmd/golangci-lint@" + version, nil
}

// majorFromVersion returns the major version number of a `vN...` string,
// tolerating anything after the major (`v2`, `v2.12.2`, `v2.0.0-rc1`,
// `v2.0.1-0.20240101120000-abcdef123456`). It errors rather than guessing
// on a version it cannot read a major out of — a bare commit hash, say —
// because guessing means silently choosing the wrong module path and
// surfacing as an unresolvable package rather than as a bad pin.
func majorFromVersion(version string) (int, error) {
	if !strings.HasPrefix(version, "v") {
		return 0, fmt.Errorf(`must be a semver tag beginning with "v"`)
	}
	digits := version[1:]
	if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = digits[:i]
	}
	if digits == "" {
		return 0, fmt.Errorf(`must be a semver tag beginning with "v" followed by a major version number`)
	}
	major, err := strconv.Atoi(digits)
	if err != nil {
		return 0, fmt.Errorf("unreadable major version: %w", err)
	}
	return major, nil
}

// runBuild compiles c.Source. pkg defaults to "."; binaryName defaults to
// the basename of the `module` directive in go.mod.
func (c *Ci) runBuild(ctx context.Context) (*dagger.File, error) {
	pkg := c.BuildPkg
	if pkg == "" {
		pkg = "."
	}
	binaryName := c.BuildBinaryName
	if binaryName == "" {
		modContents, err := c.Source.File("go.mod").Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("read go.mod to derive binary name: %w", err)
		}
		modulePath, err := parseModuleDirective(modContents)
		if err != nil {
			return nil, fmt.Errorf("parse go.mod to derive binary name: %w", err)
		}
		binaryName = basenameAfterSlash(modulePath)
		if binaryName == "" {
			return nil, fmt.Errorf("could not derive default binary name from go.mod module directive")
		}
	}
	ctr, err := c.Go.Container(ctx, c.Source)
	if err != nil {
		return nil, err
	}
	target := "/out/" + binaryName
	return ctr.WithExec([]string{"go", "build", "-o", target, pkg}).File(target), nil
}

// parseModuleDirective scans go.mod for the top-level `module <path>`
// directive and returns the path. Returns "" if absent. Tolerates
// arbitrary whitespace between `module` and the path (go.mod permits
// tabs as well as spaces).
func parseModuleDirective(content string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}

// basenameAfterSlash returns everything after the final "/" in s (or s
// itself if no "/" is present). Used to derive the default binary name
// from a module path like example.com/hello → "hello".
func basenameAfterSlash(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}
