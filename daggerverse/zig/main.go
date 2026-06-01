// Package main implements the zig Dagger module: a wrapper around the Zig
// toolchain (build, build-exe, run, test, fmt, version, env, targets) so
// downstream pipelines can build, test, format, and cross-compile Zig without
// re-inventing toolchain pinning and container plumbing.
//
// Toolchain version is pinned via New(version) or inferred from the source's
// build.zig.zon `minimum_zig_version`; falls back to a module-pinned default.
//
// There is no canonical official Zig image, so the base container downloads
// the official release tarball from ziglang.org (via dag.HTTP, SHA256-verified
// against download/index.json) and unpacks it onto a minimal alpine base with
// `zig` on PATH and a shared zig-cache cache volume mounted.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dagger/zig/internal/dagger"
)

const (
	// defaultZigVersion is used when New("") is called against a source with
	// no build.zig.zon minimum_zig_version, and for source-less funcs. Must
	// be a version present in https://ziglang.org/download/index.json.
	defaultZigVersion = "0.14.1"
	zigIndexURL       = "https://ziglang.org/download/index.json"
	zigInstallDir     = "/opt/zig"
	zigCacheDir       = "/zig-global-cache"
	alpineRef         = "alpine:3.21"
)

// Zig wraps the Zig toolchain as Dagger functions. Construct via New(); call
// Container() for the prepared base container, or use the per-CLI helpers
// (Build, BuildExe, Run, Test, Fmt, ...) which reuse the same backing
// container.
type Zig struct {
	// Version is the pinned Zig toolchain version (e.g. "0.14.1"). Empty
	// means infer from the supplied source's build.zig.zon
	// `minimum_zig_version`; falls back to a module-pinned default.
	Version string
}

// New returns a Zig module configured for the given toolchain version.
// version is optional: empty means the version is inferred from the source's
// build.zig.zon for source-bearing funcs, and the module-pinned default is
// used for source-less funcs (ToolVersion, Env, Targets).
func New(
	// +optional
	version string,
) *Zig {
	return &Zig{Version: version}
}

// Container returns the prepared base container with the zig toolchain on
// PATH, the shared zig-cache cache volume mounted, source mounted at /src, and
// the working directory set to /src. Use this as an escape hatch when a Zig
// command isn't covered by the typed helpers.
//
// The toolchain is downloaded from ziglang.org at the version from New() or,
// when New("") was used, from source/build.zig.zon's `minimum_zig_version`
// (falling back to the module-pinned default). The signature takes ctx +
// returns error because version inference and the tarball download require
// async I/O.
//
// +cache="session"
func (z *Zig) Container(
	ctx context.Context,
	source *dagger.Directory,
) (*dagger.Container, error) {
	if source == nil {
		return nil, fmt.Errorf("source is required; use ToolVersion/Env/Targets for source-less workflows")
	}
	base, err := z.baseContainer(ctx, source)
	if err != nil {
		return nil, err
	}
	return base.
		WithMountedDirectory("/src", source).
		WithWorkdir("/src"), nil
}

// Build runs `zig build [-Doptimize=<optimize>] [-Dtarget=<target>] [steps...]
// [args...]` against the supplied source and returns the `zig-out` install
// directory.
//
// optimize, when non-empty, must be one of Debug, ReleaseSafe, ReleaseFast,
// ReleaseSmall and is rejected otherwise. Empty optimize and empty target
// build for the host.
//
// +cache="session"
func (z *Zig) Build(
	ctx context.Context,
	source *dagger.Directory,
	// +optional
	optimize string,
	// +optional
	target string,
	// +optional
	steps []string,
	// +optional
	args []string,
) (*dagger.Directory, error) {
	if err := validateOptimize(optimize); err != nil {
		return nil, err
	}
	ctr, err := z.Container(ctx, source)
	if err != nil {
		return nil, err
	}
	cmd := []string{"zig", "build"}
	if optimize != "" {
		cmd = append(cmd, "-Doptimize="+optimize)
	}
	if target != "" {
		cmd = append(cmd, "-Dtarget="+target)
	}
	cmd = append(cmd, steps...)
	cmd = append(cmd, args...)
	return ctr.WithExec(cmd).Directory("/src/zig-out"), nil
}

// BuildExe runs `zig build-exe <root> [-O <optimize>] [-target <target>]
// --name <name> [args...]` for a single entry file and returns the produced
// executable.
//
// root is required (the single entry .zig file). optimize, when non-empty,
// must be one of Debug, ReleaseSafe, ReleaseFast, ReleaseSmall. name defaults
// to the basename of root with any trailing ".zig" stripped.
//
// Note the flag spelling differs from Build: build-exe uses -O / -target /
// --name (compiler flags), not the -Doptimize= / -Dtarget= build-system
// options.
//
// +cache="session"
func (z *Zig) BuildExe(
	ctx context.Context,
	source *dagger.Directory,
	root string,
	// +optional
	optimize string,
	// +optional
	target string,
	// +optional
	name string,
	// +optional
	args []string,
) (*dagger.File, error) {
	if root == "" {
		return nil, fmt.Errorf("BuildExe: root is required (the single entry .zig file)")
	}
	if err := validateOptimize(optimize); err != nil {
		return nil, err
	}
	ctr, err := z.Container(ctx, source)
	if err != nil {
		return nil, err
	}
	out := name
	if out == "" {
		out = exeName(root)
	}
	cmd := []string{"zig", "build-exe", root}
	if optimize != "" {
		cmd = append(cmd, "-O", optimize)
	}
	if target != "" {
		cmd = append(cmd, "-target", target)
	}
	cmd = append(cmd, "--name", out)
	cmd = append(cmd, args...)
	return ctr.WithExec(cmd).File("/src/" + out), nil
}

// Run runs `zig build run [-- args...]` against the supplied source and
// returns the program's stdout.
//
// +cache="session"
func (z *Zig) Run(
	ctx context.Context,
	source *dagger.Directory,
	// +optional
	args []string,
) (string, error) {
	ctr, err := z.Container(ctx, source)
	if err != nil {
		return "", err
	}
	cmd := []string{"zig", "build", "run"}
	if len(args) > 0 {
		cmd = append(cmd, "--")
		cmd = append(cmd, args...)
	}
	return ctr.WithExec(cmd).Stdout(ctx)
}

// Test runs `zig build test` when root is empty, else `zig test <root>`,
// against the supplied source and returns the combined stdout.
//
// +cache="session"
func (z *Zig) Test(
	ctx context.Context,
	source *dagger.Directory,
	// +optional
	root string,
	// +optional
	args []string,
) (string, error) {
	ctr, err := z.Container(ctx, source)
	if err != nil {
		return "", err
	}
	var cmd []string
	if root == "" {
		cmd = []string{"zig", "build", "test"}
	} else {
		cmd = []string{"zig", "test", root}
	}
	cmd = append(cmd, args...)
	return ctr.WithExec(cmd).Stdout(ctx)
}

// Fmt runs `zig fmt --check .` against the supplied source and returns the
// list of unformatted files. `zig fmt --check` exits non-zero and prints the
// offending paths to stdout, so the exec is run allowing any exit code; a
// non-zero exit (or any reported file) is also returned as an error so CI
// fails fast on formatting violations.
//
// +cache="session"
func (z *Zig) Fmt(ctx context.Context, source *dagger.Directory) (string, error) {
	ctr, err := z.Container(ctx, source)
	if err != nil {
		return "", err
	}
	exec := ctr.WithExec(
		[]string{"zig", "fmt", "--check", "."},
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny},
	)
	out, err := exec.Stdout(ctx)
	if err != nil {
		return out, err
	}
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return out, err
	}
	if code != 0 || strings.TrimSpace(out) != "" {
		return out, fmt.Errorf("zig fmt found unformatted files:\n%s", out)
	}
	return out, nil
}

// ToolVersion runs `zig version` in a source-less base container and returns
// the trimmed output (e.g. "0.14.1").
//
// +cache="session"
func (z *Zig) ToolVersion(ctx context.Context) (string, error) {
	ctr, err := z.baseContainer(ctx, nil)
	if err != nil {
		return "", err
	}
	out, err := ctr.WithExec([]string{"zig", "version"}).Stdout(ctx)
	if err != nil {
		return out, err
	}
	return strings.TrimSpace(out), nil
}

// Env runs `zig env` in a source-less base container and returns its stdout
// (JSON).
//
// +cache="session"
func (z *Zig) Env(ctx context.Context) (string, error) {
	ctr, err := z.baseContainer(ctx, nil)
	if err != nil {
		return "", err
	}
	return ctr.WithExec([]string{"zig", "env"}).Stdout(ctx)
}

// Targets runs `zig targets` in a source-less base container and returns its
// stdout (the supported architecture/OS/ABI matrix).
//
// +cache="session"
func (z *Zig) Targets(ctx context.Context) (string, error) {
	ctr, err := z.baseContainer(ctx, nil)
	if err != nil {
		return "", err
	}
	return ctr.WithExec([]string{"zig", "targets"}).Stdout(ctx)
}

// baseContainer downloads + verifies + unpacks the zig toolchain at the
// resolved version onto an alpine base with `zig` on PATH and the shared
// zig-cache cache volume mounted at ZIG_GLOBAL_CACHE_DIR. source is used only
// for version inference and may be nil for source-less callers.
func (z *Zig) baseContainer(ctx context.Context, source *dagger.Directory) (*dagger.Container, error) {
	version, err := z.resolveVersion(ctx, source)
	if err != nil {
		return nil, err
	}
	tc, err := z.toolchainDir(ctx, version)
	if err != nil {
		return nil, err
	}
	return dag.Container().
		From(alpineRef).
		WithDirectory(zigInstallDir, tc).
		WithEnvVariable("PATH", zigInstallDir+":${PATH}", dagger.ContainerWithEnvVariableOpts{Expand: true}).
		WithMountedCache(zigCacheDir, dag.CacheVolume("zig-cache")).
		WithEnvVariable("ZIG_GLOBAL_CACHE_DIR", zigCacheDir), nil
}

// toolchainDir downloads the official zig release tarball for the host
// platform at the given version, verifies its SHA256 against the value in
// download/index.json, extracts it, and returns the unpacked toolchain
// directory (the `zig` binary sits at its top level).
func (z *Zig) toolchainDir(ctx context.Context, version string) (*dagger.Directory, error) {
	targetKey, err := zigTargetKey(ctx)
	if err != nil {
		return nil, err
	}
	art, err := zigDownload(ctx, version, targetKey)
	if err != nil {
		return nil, err
	}
	tarball := dag.HTTP(art.Tarball)
	// `sha256sum -c` exits non-zero on mismatch, which fails this build step
	// and surfaces as an error — fail-fast checksum verification with no
	// dedicated HTTP checksum API needed.
	verify := fmt.Sprintf("echo '%s  /zig.tar.xz' | sha256sum -c -", art.Shasum)
	return dag.Container().
		From(alpineRef).
		WithExec([]string{"apk", "add", "--no-cache", "tar", "xz"}).
		WithMountedFile("/zig.tar.xz", tarball).
		WithExec([]string{"sh", "-c", verify}).
		WithExec([]string{"mkdir", "-p", zigInstallDir}).
		WithExec([]string{"tar", "-xJf", "/zig.tar.xz", "-C", zigInstallDir, "--strip-components=1"}).
		Directory(zigInstallDir), nil
}

// resolveVersion returns z.Version when set; otherwise reads
// source/build.zig.zon's `minimum_zig_version` and returns it. Returns the
// module-pinned default when source is nil, build.zig.zon is missing, or no
// minimum_zig_version is declared.
func (z *Zig) resolveVersion(ctx context.Context, source *dagger.Directory) (string, error) {
	if z.Version != "" {
		return z.Version, nil
	}
	if source == nil {
		return defaultZigVersion, nil
	}
	contents, err := source.File("build.zig.zon").Contents(ctx)
	if err != nil {
		// build.zig.zon missing or unreadable — fall back per spec.
		return defaultZigVersion, nil
	}
	if v := parseMinimumZigVersion(contents); v != "" {
		return v, nil
	}
	return defaultZigVersion, nil
}

// parseMinimumZigVersion scans a build.zig.zon file (ZON, not JSON) for the
// `.minimum_zig_version = "X.Y.Z"` field and returns the quoted value. Returns
// "" when the field is absent. ZON permits arbitrary whitespace around `=`, so
// the scan locates the key, then the next `=`, then the next quoted string.
func parseMinimumZigVersion(content string) string {
	const key = ".minimum_zig_version"
	i := strings.Index(content, key)
	if i < 0 {
		return ""
	}
	rest := content[i+len(key):]
	if eq := strings.IndexByte(rest, '='); eq >= 0 {
		rest = rest[eq+1:]
	} else {
		return ""
	}
	open := strings.IndexByte(rest, '"')
	if open < 0 {
		return ""
	}
	rest = rest[open+1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// zigTargetKey maps the engine's default platform to the per-target key used
// in download/index.json (e.g. "x86_64-linux", "aarch64-linux").
func zigTargetKey(ctx context.Context) (string, error) {
	plat, err := dag.DefaultPlatform(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve default platform: %w", err)
	}
	s := string(plat) // e.g. "linux/amd64"
	switch {
	case strings.Contains(s, "amd64"), strings.Contains(s, "x86_64"):
		return "x86_64-linux", nil
	case strings.Contains(s, "arm64"), strings.Contains(s, "aarch64"):
		return "aarch64-linux", nil
	default:
		return "", fmt.Errorf("unsupported host platform %q for zig toolchain download", s)
	}
}

// zigArtifact is one per-target entry in download/index.json. size is encoded
// as a JSON string in the index, so it is typed as a string here.
type zigArtifact struct {
	Tarball string `json:"tarball"`
	Shasum  string `json:"shasum"`
	Size    string `json:"size"`
}

// zigDownload fetches download/index.json and returns the artifact for
// (version, targetKey). The inner per-version map mixes JSON strings (date,
// docs, notes) with objects (the per-target builds), so it is decoded as
// json.RawMessage and only the requested target key is unmarshaled.
func zigDownload(ctx context.Context, version, targetKey string) (zigArtifact, error) {
	raw, err := dag.HTTP(zigIndexURL).Contents(ctx)
	if err != nil {
		return zigArtifact{}, fmt.Errorf("fetch zig index.json: %w", err)
	}
	var index map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &index); err != nil {
		return zigArtifact{}, fmt.Errorf("parse zig index.json: %w", err)
	}
	targets, ok := index[version]
	if !ok {
		return zigArtifact{}, fmt.Errorf("zig version %q not found in index.json", version)
	}
	rawArt, ok := targets[targetKey]
	if !ok {
		return zigArtifact{}, fmt.Errorf("zig version %q has no %q build in index.json", version, targetKey)
	}
	var art zigArtifact
	if err := json.Unmarshal(rawArt, &art); err != nil {
		return zigArtifact{}, fmt.Errorf("parse zig artifact for %s/%s: %w", version, targetKey, err)
	}
	if art.Tarball == "" || art.Shasum == "" {
		return zigArtifact{}, fmt.Errorf("zig artifact for %s/%s missing tarball/shasum", version, targetKey)
	}
	return art, nil
}

// validateOptimize accepts an empty optimize (host default) or one of the four
// Zig optimization modes, rejecting anything else with a clear error.
func validateOptimize(optimize string) error {
	switch optimize {
	case "", "Debug", "ReleaseSafe", "ReleaseFast", "ReleaseSmall":
		return nil
	default:
		return fmt.Errorf("invalid optimize %q: must be one of Debug, ReleaseSafe, ReleaseFast, ReleaseSmall", optimize)
	}
}

// exeName returns basename(root) with a trailing ".zig" stripped — the default
// executable name `zig build-exe` produces for the given root file.
func exeName(root string) string {
	b := root
	if i := strings.LastIndexByte(b, '/'); i >= 0 {
		b = b[i+1:]
	}
	return strings.TrimSuffix(b, ".zig")
}
