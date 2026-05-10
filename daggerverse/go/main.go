// Package main implements the go Dagger module: a thin wrapper around the
// Go CLI surface (build, test, vet, fmt, run, generate, install, mod*, work,
// env, version) so downstream pipelines can compose Go workflows without
// re-inventing toolchain pinning, cache mounts, and container plumbing.
//
// Toolchain version is pinned via New(version) or inferred from the source's
// go.mod `go` directive; falls back to "latest" when no go directive is
// found. Every container mounts the shared `go-mod-cache` and
// `go-build-cache` Dagger cache volumes.
package main

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"dagger/go/internal/dagger"
)

// Go wraps the Go CLI as Dagger functions. Construct via New(); call
// Container() for the prepared base container, or use the per-CLI helpers
// (Build, Test, Vet, ...) which reuse the same backing container.
type Go struct {
	// Version is the pinned Go toolchain version (e.g. "1.23"). Empty
	// means infer from the supplied source's go.mod `go` directive;
	// falls back to "latest" when no go directive is found.
	Version string
}

// New returns a Go module configured for the given toolchain version.
// version is optional: empty means the version is inferred from the source's
// go.mod for source-bearing CLI funcs, and "latest" is used for source-less
// funcs (Env, ToolVersion, Install).
func New(
	// +optional
	version string,
) *Go {
	return &Go{Version: version}
}

// Container returns the prepared base container with go-mod-cache mounted at
// /go/pkg/mod, go-build-cache mounted at /root/.cache/go-build, source
// mounted at /src, and the working directory set to /src. Use this as an
// escape hatch when a Go command isn't covered by the typed helpers.
//
// The toolchain image is golang:<version> where version comes from New() or,
// when New("") was used, from source/go.mod's `go` directive (fallback
// "latest"). The signature takes ctx + returns error because go.mod
// inspection requires async I/O.
func (g *Go) Container(
	ctx context.Context,
	source *dagger.Directory,
) (*dagger.Container, error) {
	version, err := g.resolveVersion(ctx, source)
	if err != nil {
		return nil, err
	}
	return dag.Container().
		From("golang:"+version).
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("go-mod-cache")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("go-build-cache")).
		WithMountedDirectory("/src", source).
		WithWorkdir("/src"), nil
}

// Install runs `go install pkg` in a source-less base container with
// GOBIN=/out and returns the resulting binary as a *dagger.File. The
// returned filename is the basename of pkg (with any @version suffix
// stripped), matching `go install`'s naming rules.
//
// Callers should pin pkg to a specific version (e.g. `pkg@v1.2.3`) for
// reproducible builds; `@latest` or unpinned paths will resolve against
// the proxy at call time. Result caching is disabled so unpinned callers
// don't get silently stale binaries.
//
// +cache="never"
func (g *Go) Install(pkg string) *dagger.File {
	return g.bareContainer().
		WithEnvVariable("GOBIN", "/out").
		WithExec([]string{"go", "install", pkg}).
		File("/out/" + pkgBinName(pkg))
}

// pkgBinName returns the binary name `go install` produces for pkg: the last
// path segment with any "@version" suffix stripped.
func pkgBinName(pkg string) string {
	p := pkg
	if i := strings.IndexByte(p, '@'); i >= 0 {
		p = p[:i]
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Work runs `go work <subcommand> [args...]` against the supplied source
// and returns stdout. subcommand is required (e.g. "init", "use", "sync",
// "version").
func (g *Go) Work(
	ctx context.Context,
	source *dagger.Directory,
	subcommand string,
	// +optional
	args []string,
) (string, error) {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return "", err
	}
	cmd := []string{"go", "work", subcommand}
	cmd = append(cmd, args...)
	return ctr.WithExec(cmd).Stdout(ctx)
}

// ModTidy runs `go mod tidy` against the supplied source and returns the
// updated /src directory.
func (g *Go) ModTidy(
	ctx context.Context,
	source *dagger.Directory,
) (*dagger.Directory, error) {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return nil, err
	}
	return ctr.WithExec([]string{"go", "mod", "tidy"}).Directory("/src"), nil
}

// ModDownload runs `go mod download` against the supplied source.
func (g *Go) ModDownload(ctx context.Context, source *dagger.Directory) error {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return err
	}
	_, err = ctr.WithExec([]string{"go", "mod", "download"}).Sync(ctx)
	return err
}

// ModVerify runs `go mod verify` against the supplied source.
func (g *Go) ModVerify(ctx context.Context, source *dagger.Directory) error {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return err
	}
	_, err = ctr.WithExec([]string{"go", "mod", "verify"}).Sync(ctx)
	return err
}

// Generate runs `go generate pkg` against the supplied source and returns
// /src after generation. pkg defaults to `./...`.
func (g *Go) Generate(
	ctx context.Context,
	source *dagger.Directory,
	// +default="./..."
	pkg string,
) (*dagger.Directory, error) {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return nil, err
	}
	return ctr.WithExec([]string{"go", "generate", pkg}).Directory("/src"), nil
}

// Run runs `go run pkg [args...]` against the supplied source and returns
// the program's stdout. pkg is required (a single runnable main package).
func (g *Go) Run(
	ctx context.Context,
	source *dagger.Directory,
	pkg string,
	// +optional
	args []string,
) (string, error) {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return "", err
	}
	cmd := []string{"go", "run", pkg}
	cmd = append(cmd, args...)
	return ctr.WithExec(cmd).Stdout(ctx)
}

// Build runs `go build -o /out/[output] [flags] pkg` against the supplied
// source and returns /out as a *dagger.Directory. pkg defaults to `./...`;
// when output is empty, `-o /out/` is used so go build picks names per its
// own rules (one binary per main package).
func (g *Go) Build(
	ctx context.Context,
	source *dagger.Directory,
	// +default="./..."
	pkg string,
	// +optional
	output string,
	// +optional
	flags []string,
) (*dagger.Directory, error) {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return nil, err
	}
	target := "/out/"
	if output != "" {
		target = "/out/" + output
	}
	args := []string{"go", "build", "-o", target}
	args = append(args, flags...)
	args = append(args, pkg)
	return ctr.WithExec(args).Directory("/out"), nil
}

// Test runs `go test -count=1 [-race] [flags] pkg` against the supplied
// source and returns the combined stdout. -count=1 is always passed to
// bypass Go's internal test cache.
func (g *Go) Test(
	ctx context.Context,
	source *dagger.Directory,
	// +default="./..."
	pkg string,
	// +default=false
	race bool,
	// +optional
	flags []string,
) (string, error) {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return "", err
	}
	args := []string{"go", "test", "-count=1"}
	if race {
		args = append(args, "-race")
	}
	args = append(args, flags...)
	args = append(args, pkg)
	return ctr.WithExec(args).Stdout(ctx)
}

// Fmt runs `gofmt -l -d .` against the supplied source. Returns the diff
// of any unformatted files; non-empty output is also returned as an error so
// CI fails fast on formatting violations.
func (g *Go) Fmt(ctx context.Context, source *dagger.Directory) (string, error) {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return "", err
	}
	out, err := ctr.WithExec([]string{"gofmt", "-l", "-d", "."}).Stdout(ctx)
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(out) != "" {
		return out, fmt.Errorf("gofmt found unformatted files:\n%s", out)
	}
	return out, nil
}

// Vet runs `go vet pkg` against the supplied source. pkg defaults to
// `./...`. Returns a non-nil error when vet reports any issue.
func (g *Go) Vet(
	ctx context.Context,
	source *dagger.Directory,
	// +default="./..."
	pkg string,
) error {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return err
	}
	_, err = ctr.WithExec([]string{"go", "vet", pkg}).Sync(ctx)
	return err
}

// Env runs `go env` in a source-less base container and returns its stdout.
func (g *Go) Env(ctx context.Context) (string, error) {
	return g.bareContainer().WithExec([]string{"go", "env"}).Stdout(ctx)
}

// ToolVersion runs `go version` in a source-less base container and returns
// the trimmed output (e.g. "go version go1.23.0 linux/amd64").
func (g *Go) ToolVersion(ctx context.Context) (string, error) {
	out, err := g.bareContainer().WithExec([]string{"go", "version"}).Stdout(ctx)
	if err != nil {
		return out, err
	}
	return strings.TrimSpace(out), nil
}

// bareContainer is the source-less variant of Container: golang image at
// g.Version (or "latest"), shared cache mounts, no /src. Used by funcs that
// don't operate on a user-supplied source (Env, ToolVersion, Install).
func (g *Go) bareContainer() *dagger.Container {
	version := g.Version
	if version == "" {
		version = "latest"
	}
	return dag.Container().
		From("golang:"+version).
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("go-mod-cache")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("go-build-cache"))
}

// resolveVersion returns g.Version when set; otherwise reads source/go.mod's
// `go` directive and returns it. Returns "latest" when go.mod is missing or
// has no go directive.
func (g *Go) resolveVersion(ctx context.Context, source *dagger.Directory) (string, error) {
	if g.Version != "" {
		return g.Version, nil
	}
	if source == nil {
		return "latest", nil
	}
	contents, err := source.File("go.mod").Contents(ctx)
	if err != nil {
		// go.mod missing or unreadable — fall back per spec.
		return "latest", nil
	}
	if v := parseGoDirective(contents); v != "" {
		return v, nil
	}
	return "latest", nil
}

// parseGoDirective scans a go.mod file's contents for the top-level
// `go X.Y[.Z]` directive and returns the version string. Returns "" when
// no directive is present.
func parseGoDirective(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if rest, ok := strings.CutPrefix(line, "go "); ok {
			// First whitespace-separated field only — drops any inline
			// `// comment` and trailing tokens.
			if fields := strings.Fields(rest); len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}
