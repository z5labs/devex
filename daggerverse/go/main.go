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
//
// +cache="session"
func (g *Go) Container(
	ctx context.Context,
	source *dagger.Directory,
) (*dagger.Container, error) {
	if source == nil {
		return nil, fmt.Errorf("source is required; use Env/ToolVersion/Install for source-less workflows")
	}
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
// pkg MUST be pinned to an explicit version (e.g. `pkg@v1.2.3` or a
// commit-hash pseudo-version); `@latest` and bare paths are rejected.
// The pin is what makes the result safe to cache across calls within a
// session — without it, the proxy could resolve different versions on
// successive invocations.
//
// +cache="session"
func (g *Go) Install(pkg string) (*dagger.File, error) {
	if err := validatePinnedPkg(pkg); err != nil {
		return nil, err
	}
	return g.bareContainer().
		WithEnvVariable("GOBIN", "/out").
		WithExec([]string{"go", "install", pkg}).
		File("/out/" + pkgBinName(pkg)), nil
}

// validatePinnedPkg returns an error unless pkg has an `@<version>` suffix
// where the version is non-empty and not `latest`. This forces callers to
// pin so Install's session cache cannot return a stale binary when the
// upstream module's `@latest` advances.
func validatePinnedPkg(pkg string) error {
	i := strings.IndexByte(pkg, '@')
	if i < 0 {
		return fmt.Errorf("Install: pkg %q must be pinned (use pkg@v1.2.3)", pkg)
	}
	v := pkg[i+1:]
	if v == "" || v == "latest" {
		return fmt.Errorf("Install: pkg %q must pin an explicit version (got %q)", pkg, v)
	}
	return nil
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
//
// +cache="session"
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
//
// +cache="session"
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
//
// +cache="session"
func (g *Go) ModDownload(ctx context.Context, source *dagger.Directory) error {
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return err
	}
	_, err = ctr.WithExec([]string{"go", "mod", "download"}).Sync(ctx)
	return err
}

// ModVerify runs `go mod verify` against the supplied source.
//
// +cache="session"
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
//
// +cache="session"
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
//
// +cache="session"
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

// Build runs `go build` against the supplied source and returns /out as a
// *dagger.Directory. pkg defaults to `./...`; when output is empty, `-o
// /out/` is used so go build picks names per its own rules (one binary per
// main package).
//
// Every flag this function can pass is a named input with its own doc
// comment, so `dagger functions` describes what each one does to the
// output. There is deliberately no raw `flags []string` escape hatch: a bag
// of strings cannot be validated, cannot be documented per flag, and makes
// every caller re-learn the same spellings. Container() is the escape hatch
// for anything not named here — it hands back the prepared container so a
// caller can run whatever `go build` invocation it likes.
//
// +cache="session"
func (g *Go) Build(
	ctx context.Context,
	source *dagger.Directory,
	// Package(s) to build, in `go build` package-list syntax.
	//
	// +default="./..."
	pkg string,
	// Name of the artifact written under /out. Empty means `-o /out/`, which
	// lets go build name each binary after its main package.
	//
	// Named artifactName rather than output because the Dagger CLI reserves
	// `--output/-o` for exporting a call's result: a function parameter
	// called output collides with it, and `dagger call build` then fails to
	// parse its own flags before it runs anything. Ci.WithBuild's
	// binaryName dodges the same collision; this one is not always a binary,
	// because buildmode can make it an archive or a shared library.
	//
	// +optional
	artifactName string,
	// Pass -trimpath: strip the build's local file system paths out of the
	// binary, so the output does not depend on where it was compiled.
	//
	// +optional
	trimpath bool,
	// Pass -ldflags "-s -w": drop the symbol table and the DWARF debug
	// info. Smaller binary, no usable stack symbolization or debugger.
	//
	// +optional
	strip bool,
	// Link-time variable assignments, each `importpath.Name=value`,
	// rendered as `-ldflags "-X importpath.Name=value"`. This is how a
	// binary learns its own version or commit. Only the first `=` splits
	// name from value, so a value may itself contain `=`. An element with
	// no `=`, or with an empty name, is rejected. The linker silently
	// ignores a stamp naming a variable that does not exist, or one that
	// is not a package-level string.
	//
	// +optional
	stamps []string,
	// Build tags, passed as `-tags a,b,c`. Selects which `//go:build`
	// files are compiled in.
	//
	// +optional
	tags []string,
	// Target platform as `GOOS/GOARCH[/variant]`, e.g. "linux/arm64".
	// Sets GOOS and GOARCH for a cross-compile; empty builds for the
	// toolchain container's own platform. Any variant segment is ignored —
	// GOARM/GOAMD64 are left unset.
	//
	// +optional
	platform string,
	// Set CGO_ENABLED=0. Produces a statically linked binary with no libc
	// dependency, which is what a scratch image needs, at the cost of the
	// cgo-backed net and os/user implementations.
	//
	// +optional
	disableCgo bool,
	// Pass -race: link Go's data-race detector into the output. The binary
	// then reports racing accesses to stderr as it runs, at roughly 2-20x
	// the CPU and 5-10x the memory of an ordinary build — so this is a
	// binary for an integration test, not one to ship.
	//
	// -race requires cgo, so it cannot be combined with disableCgo (Build
	// rejects that pairing) and it needs a C toolchain for the target: the
	// golang image has one for its own platform, but a cross-compile via
	// platform does not unless the toolchain image provides it.
	//
	// +optional
	race bool,
	// Pass -buildmode=<mode>: what the linker emits, which for most modes is
	// not an executable. Absent leaves the flag off entirely, so `go build`
	// picks its own default for the target — an executable for a main
	// package, an archive for the rest. See BuildMode for what each member
	// produces.
	//
	// +optional
	buildmode BuildMode,
) (*dagger.Directory, error) {
	if race && disableCgo {
		return nil, fmt.Errorf("Build: race and disableCgo cannot be combined: -race links the race runtime through cgo, so CGO_ENABLED=0 makes `go build -race` fail — pass one or the other")
	}
	ldflags, err := renderLdflags(strip, stamps)
	if err != nil {
		return nil, err
	}
	ctr, err := g.Container(ctx, source)
	if err != nil {
		return nil, err
	}
	if platform != "" {
		goos, goarch, err := parseBuildPlatform(platform)
		if err != nil {
			return nil, err
		}
		ctr = ctr.
			WithEnvVariable("GOOS", goos).
			WithEnvVariable("GOARCH", goarch)
	}
	if disableCgo {
		ctr = ctr.WithEnvVariable("CGO_ENABLED", "0")
	}
	target := "/out/"
	if artifactName != "" {
		target = "/out/" + artifactName
	}
	args := []string{"go", "build"}
	if trimpath {
		args = append(args, "-trimpath")
	}
	if race {
		args = append(args, "-race")
	}
	if buildmode != "" {
		mode, err := buildmode.flag()
		if err != nil {
			return nil, err
		}
		args = append(args, "-buildmode="+mode)
	}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "-o", target, pkg)
	return ctr.WithExec(args).Directory("/out"), nil
}

// renderLdflags builds the single `-ldflags` argument from the strip switch
// and the -X stamps. Returns "" when neither is requested, so the caller can
// leave -ldflags off the command line entirely.
func renderLdflags(strip bool, stamps []string) (string, error) {
	var parts []string
	if strip {
		parts = append(parts, "-s", "-w")
	}
	for _, stamp := range stamps {
		// IndexByte, not a split: everything after the first `=` is the
		// value, so a value containing `=` reaches the linker intact.
		i := strings.IndexByte(stamp, '=')
		if i < 0 {
			return "", fmt.Errorf("Build: stamp %q is not in importpath.Name=value form: no %q", stamp, "=")
		}
		if i == 0 {
			return "", fmt.Errorf("Build: stamp %q has an empty importpath.Name before %q", stamp, "=")
		}
		quoted, err := quoteLdflag(stamp)
		if err != nil {
			return "", err
		}
		parts = append(parts, "-X", quoted)
	}
	return strings.Join(parts, " "), nil
}

// quoteLdflag makes tok survive cmd/go's -ldflags splitting. cmd/go splits
// the -ldflags value on whitespace, honouring single and double quotes but
// no escape sequences — so a stamp value containing a space has to be
// wrapped, and one containing both quote characters cannot be represented
// at all and is rejected rather than silently truncated.
func quoteLdflag(tok string) (string, error) {
	if !strings.ContainsAny(tok, " \t\n'\"") {
		return tok, nil
	}
	if !strings.Contains(tok, "'") {
		return "'" + tok + "'", nil
	}
	if !strings.Contains(tok, `"`) {
		return `"` + tok + `"`, nil
	}
	return "", fmt.Errorf("Build: stamp %q contains both quote characters, which -ldflags cannot express", tok)
}

// parseBuildPlatform splits a platform string ("goos/goarch" or
// "goos/goarch/variant") into GOOS and GOARCH. Segments past the first two
// are accepted and ignored.
func parseBuildPlatform(p string) (goos, goarch string, err error) {
	parts := strings.Split(p, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("Build: invalid platform %q (expected GOOS/GOARCH[/variant])", p)
	}
	return parts[0], parts[1], nil
}

// Test runs `go test -count=1 [-race] [flags] pkg` against the supplied
// source and returns the combined stdout. -count=1 is always passed to
// bypass Go's internal test cache.
//
// +cache="session"
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
//
// +cache="session"
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
//
// +cache="session"
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
//
// +cache="session"
func (g *Go) Env(ctx context.Context) (string, error) {
	return g.bareContainer().WithExec([]string{"go", "env"}).Stdout(ctx)
}

// ToolVersion runs `go version` in a source-less base container and returns
// the trimmed output (e.g. "go version go1.23.0 linux/amd64").
//
// +cache="session"
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
// no directive is present or when scanning fails.
func parseGoDirective(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	// Raise the per-line limit well above the default 64KiB — generated
	// or vendored go.mod files can have very long require blocks. 1MiB
	// is comfortably larger than any real-world go.mod line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
	// Scanner errors (e.g. line longer than the buffer) fall through to
	// "" so the caller fallbacks to "latest" rather than panicking.
	_ = scanner.Err()
	return ""
}
