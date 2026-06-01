# zig

A Dagger module wrapping the [Zig](https://ziglang.org/) toolchain (build,
build-exe, run, test, fmt, version, env, targets) so downstream pipelines can
build, test, format, and cross-compile Zig without re-inventing toolchain
pinning and container plumbing.

There is no canonical official Zig image, so the base container downloads the
official release tarball from ziglang.org via `dag.HTTP`, verifies its SHA256
against [`download/index.json`](https://ziglang.org/download/index.json), and
unpacks it onto a minimal `alpine` base with `zig` on `PATH`. A shared
`zig-cache` Dagger cache volume is mounted at `/zig-global-cache`
(`ZIG_GLOBAL_CACHE_DIR`). Source is mounted at `/src` and used as the working
directory.

## Toolchain version

`New(version)` pins the toolchain (e.g. `"0.14.1"`). When called with `""`,
source-bearing helpers parse the supplied source's `build.zig.zon`
`minimum_zig_version` field and use that version; if it is absent (or no
`build.zig.zon` is present) the module falls back to a pinned default.
Source-less helpers (`ToolVersion`, `Env`, `Targets`) use `Version` directly,
falling back to the same default. The selected version must exist in
`download/index.json`.

## Function surface

| Name | Purpose |
|---|---|
| `Container(source)` | Prepared base container — escape hatch when a Zig command isn't covered by the typed helpers. Returns `*Container` lazily; the underlying constructor takes ctx + returns error so version inference and the toolchain download can run. |
| `Build(source, optimize, target, steps, args)` | `zig build [-Doptimize=..] [-Dtarget=..] steps... args...`; returns the `zig-out` install directory. `optimize` ∈ {Debug, ReleaseSafe, ReleaseFast, ReleaseSmall} when set. |
| `BuildExe(source, root, optimize, target, name, args)` | `zig build-exe <root> [-O ..] [-target ..] --name <name> args...`; returns the produced executable as a `*File`. `root` is required. |
| `Run(source, args)` | `zig build run [-- args...]`; returns stdout. |
| `Test(source, root, args)` | `zig build test` (empty `root`) or `zig test <root>`; returns stdout. |
| `Fmt(source)` | `zig fmt --check .`; returns a non-nil error listing the unformatted files (nil when clean) so CI fails fast. |
| `ToolVersion()` | `zig version`. |
| `Env()` | `zig env` (JSON). |
| `Targets()` | `zig targets`. |

`Build` uses Zig's build-system options (`-Doptimize=`, `-Dtarget=`), while
`BuildExe` uses the compiler flags (`-O`, `-target`).

## CLI quick reference

```sh
# List functions
dagger -m daggerverse/zig functions

# Run zig version against the hello fixture
dagger -m daggerverse/zig call container \
    --source=daggerverse/zig/tests/fixtures/hello \
    with-exec --args="zig,version" stdout

# Cross-compile a project for aarch64-linux
dagger -m daggerverse/zig call build --source=. --target=aarch64-linux
```

## Zig SDK quick reference

```go
z := dag.Zig() // or dag.Zig(dagger.ZigOpts{Version: "0.14.1"})

// Build returns the zig-out install directory.
out := z.Build(src, dagger.ZigBuildOpts{Optimize: "ReleaseSmall"})

// BuildExe returns the produced executable.
exe := z.BuildExe(src, "main.zig", dagger.ZigBuildExeOpts{Name: "app"})

// Run returns the program's stdout.
stdout, err := z.Run(ctx, src)
```

See `tests/main.go` for one example per function.
