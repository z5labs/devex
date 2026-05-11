package main

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"dagger/go/internal/dagger"

	"github.com/dagger/dagger/util/parallel"
)

const (
	defaultGolangciLintVersion = "v1.64.8"
	golangciLintConfigMountPath = "/tmp/.golangci.yml"
)

// Ci is a chained builder for a standardized Go CI pipeline. Construct via
// Go.Ci(source); enable stages via the With* methods; call Run to execute.
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

// Run executes the pipeline: stage 1 (parallel checks) → stage 2 (build).
// Returns the built binary as a *dagger.File. Stage-1 errors are aggregated
// via github.com/dagger/dagger/util/parallel; on stage-1 failure Run returns
// the aggregated error and a nil file (stage 2 is skipped).
//
// +check
// +cache="session"
func (c *Ci) Run(ctx context.Context) (*dagger.File, error) {
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
	if err := jobs.Run(ctx); err != nil {
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
	ctr, err := c.Go.Container(ctx, c.Source)
	if err != nil {
		return err
	}
	ctr = ctr.
		WithEnvVariable("GOBIN", "/usr/local/bin").
		WithExec([]string{"go", "install",
			"github.com/golangci/golangci-lint/cmd/golangci-lint@" + version})
	args := []string{"golangci-lint", "run"}
	if c.LintConfig != nil {
		ctr = ctr.WithMountedFile(golangciLintConfigMountPath, c.LintConfig)
		args = append(args, "--config", golangciLintConfigMountPath)
	}
	args = append(args, "./...")
	_, err = ctr.WithExec(args).Sync(ctx)
	return err
}

// runBuild compiles c.Source. pkg defaults to "."; output defaults to the
// basename of the `module` directive in go.mod.
func (c *Ci) runBuild(ctx context.Context) (*dagger.File, error) {
	pkg := c.BuildPkg
	if pkg == "" {
		pkg = "."
	}
	output := c.BuildBinaryName
	if output == "" {
		modContents, err := c.Source.File("go.mod").Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("read go.mod to derive output name: %w", err)
		}
		modulePath, err := parseModuleDirective(modContents)
		if err != nil {
			return nil, fmt.Errorf("parse go.mod to derive output name: %w", err)
		}
		output = basenameAfterSlash(modulePath)
		if output == "" {
			return nil, fmt.Errorf("could not derive default output name from go.mod module directive")
		}
	}
	ctr, err := c.Go.Container(ctx, c.Source)
	if err != nil {
		return nil, err
	}
	target := "/out/" + output
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
