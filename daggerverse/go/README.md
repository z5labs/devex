# go

A Dagger module wrapping the Go CLI surface (build, test, vet, fmt, run,
generate, install, mod\*, work, env, version) so downstream pipelines can
compose Go workflows without re-inventing toolchain pinning, cache mounts,
and container plumbing.

Every container mounts shared `go-mod-cache` (at `/go/pkg/mod`) and
`go-build-cache` (at `/root/.cache/go-build`) Dagger cache volumes. Source is
mounted at `/src` and used as the working directory.

## Toolchain version

`New(version)` pins the toolchain (e.g. `"1.23"`). When called with `""`,
source-bearing helpers parse the supplied source's `go.mod` `go` directive
and use that version; if no directive is present the image falls back to
`golang:latest`. Source-less helpers (`Env`, `ToolVersion`, `Install`) use
`g.Version` directly, falling back to `latest`.

## Function surface

| Name | Purpose |
|---|---|
| `Container(source)` | Prepared base container — escape hatch when a Go command isn't covered by the typed helpers. Returns `*Container` lazily; the underlying constructor takes ctx + returns error in source so go.mod inspection can run. |
| `Build(source, pkg, artifactName, trimpath, strip, stamps, tags, platform, disableCgo, race, buildmode)` | `go build -o /out/[artifactName] ...`; returns `/out` as a `*Directory`. `pkg` defaults to `./...`. Every flag is a named input — see [Build flags](#build-flags). |
| `Test(source, pkg, race, flags)` | `go test -count=1 [-race] ...`; returns combined stdout. |
| `Vet(source, pkg)` | `go vet pkg`. |
| `Fmt(source)` | `gofmt -l -d .`; non-empty diff is also returned as an error. |
| `Generate(source, pkg)` | `go generate pkg`; returns `/src` after generation. |
| `Run(source, pkg, args)` | `go run pkg args...`; returns stdout. |
| `Install(pkg)` | `go install pkg` with `GOBIN=/out`; returns the resulting binary as a `*File`. |
| `ModTidy(source)` | `go mod tidy`; returns `/src`. |
| `ModDownload(source)` | `go mod download`. |
| `ModVerify(source)` | `go mod verify`. |
| `Work(source, subcommand, args)` | `go work <subcommand> args...`; returns stdout. |
| `Env()` | `go env`. |
| `ToolVersion()` | `go version`. |
| `Ci(source)` | Returns a `Ci` builder for staged pipelines (parallel checks → build). `Run` returns the built binary as a `*File`. |

## Build flags

`Build` takes no `flags []string`. Every flag it can pass is a named input
with its own doc comment, so `dagger functions` describes what each one does
to the output, the module validates them, and no caller has to re-learn a
spelling:

| Input | Effect |
|---|---|
| `trimpath` | `-trimpath` — the output no longer depends on where it was compiled. |
| `strip` | `-ldflags "-s -w"` — no symbol table, no DWARF. |
| `stamps` | `-ldflags "-X importpath.Name=value"` per element. |
| `tags` | `-tags a,b,c`. |
| `platform` | `GOOS`/`GOARCH` for a cross-compile, as `GOOS/GOARCH[/variant]`. |
| `disableCgo` | `CGO_ENABLED=0`. |
| `race` | `-race` — links Go's data-race detector into the output. |
| `buildmode` | `-buildmode=<mode>` — what the linker emits, as a `BuildMode` enum. |

`stamps` elements are `importpath.Name=value`; only the first `=` splits name
from value, so a value may contain `=`. An element with no `=`, or an empty
name, is rejected with a message naming it. A value containing whitespace is
quoted for you, since `cmd/go` splits the `-ldflags` value on whitespace.

`race` costs roughly 2-20x the CPU and 5-10x the memory of an ordinary build,
so the result is a binary for an integration test rather than one to ship. It
needs cgo, which makes two pairings worth knowing:

- **`race` with `disableCgo` is rejected** with a message naming both, rather
  than left for the linker to fail on. `-race` links the race runtime through
  cgo, so `CGO_ENABLED=0` cannot work; pass one or the other.
- **`race` with `platform`** needs a C cross-compiler for the target in the
  toolchain image. The `golang` image ships one for its own platform only, so
  a race-enabled cross-compile fails there — use `Container(source)` with an
  image that has the cross toolchain.

`buildmode` is a `BuildMode` enum rather than a string because the set is
closed and each member has a different output shape. Members surface as
`ARCHIVE`, `C_ARCHIVE`, `C_SHARED`, `EXE`, `PIE` and `PLUGIN` — the Dagger Go
SDK derives each name from the constant identifier in SCREAMING_SNAKE_CASE, so
`go build`'s hyphenated spellings are mapped internally rather than typed:

| Member | `go build` | Emits |
|---|---|---|
| `ARCHIVE` | `archive` | `.a` per listed non-main package; main packages are ignored. |
| `C_ARCHIVE` | `c-archive` | A C archive plus a generated header; only cgo `//export` functions are callable. |
| `C_SHARED` | `c-shared` | The same exported surface as `C_ARCHIVE`, linked dynamically. |
| `EXE` | `exe` | Executables, position-dependent even where PIE is the toolchain default. |
| `PIE` | `pie` | Position independent executables, which a runtime wanting ASLR requires. |
| `PLUGIN` | `plugin` | A shared library loadable with `plugin.Open`; host and plugin must come from the same toolchain and dependency versions. |

Only `race` is rejected alongside `disableCgo`; no buildmode is. That asymmetry
is measured rather than assumed: `go build -race` refuses outright with `-race
requires cgo; enable cgo by setting CGO_ENABLED=1`, whereas `c-archive` and
`c-shared` build fine with `CGO_ENABLED=0` given a pure-Go main package. What
needs cgo for those two is the `//export` directives in the *source*, which
`Build` cannot inspect — so with cgo off, a package whose exports live in cgo
files fails with `build constraints exclude all Go files`, and a pure-Go one
yields a library exporting nothing and no generated header. Rejecting the
pairing would break the second case, which works today.

Leaving `buildmode` unset leaves the flag off entirely, so `go build` picks its
own default. Two of `go build`'s modes are deliberately absent: `default` is
what omitting the input already means, and `shared` is only half a feature
without a `-linkshared` counterpart on the consuming build, which `Build` does
not have — use `Container(source)` if you are building against a shared std.

The name written under `/out` is `artifactName`, not `output`: the Dagger CLI
reserves `--output/-o` for exporting a call's result, and a function parameter
called `output` collides with it badly enough that `dagger call build` fails to
parse its own flags before running anything. `Ci.WithBuild`'s `binaryName`
dodges the same collision; `Build`'s is not always a binary, because
`buildmode` can make it an archive or a shared library.

There is deliberately no raw-flag escape hatch on `Build`: if a flag is worth
passing it is worth naming, and a bag of strings can be neither validated nor
documented. `Container(source)` remains the escape hatch for anything not
named above — it hands back the prepared container to run any `go build`
invocation you like.

## CLI quick reference

```sh
# List functions
dagger -m daggerverse/go functions

# Run go version against the hello fixture
dagger -m daggerverse/go call container \
    --source=daggerverse/go/tests/fixtures/hello \
    with-exec --args="go,version" stdout

# Test all packages in a Go source tree
dagger -m daggerverse/go call test --source=. --pkg=./...

# Build a C archive: /out holds libgreet.a and the generated libgreet.h
dagger -m daggerverse/go call build --source=path/to/project --pkg=. \
    --artifact-name=libgreet.a --buildmode=C_ARCHIVE entries

# Build a race-enabled binary for an integration test
dagger -m daggerverse/go call build --source=path/to/project --pkg=. \
    --artifact-name=myapp --race=true export --path=./out
```

## Go SDK quick reference

```go
g := dag.Go() // or dag.Go(dagger.GoOpts{Version: "1.23"})

// Build returns the /out directory containing the produced binaries.
out := g.Build(src, dagger.GoBuildOpts{Pkg: "./...", ArtifactName: "myapp"})

// A release build: static, reproducible, and told its own version.
rel := g.Build(src, dagger.GoBuildOpts{
    Pkg: ".", ArtifactName: "myapp",
    Trimpath: true, Strip: true, DisableCgo: true,
    Platform: "linux/arm64",
    Stamps:   []string{"main.version=v1.2.3"},
})

// A binary for an integration test, with the race detector linked in.
racy := g.Build(src, dagger.GoBuildOpts{
    Pkg: ".", ArtifactName: "myapp", Race: true,
})

// A C archive: /out holds libmyapp.a and the generated libmyapp.h.
lib := g.Build(src, dagger.GoBuildOpts{
    Pkg: ".", ArtifactName: "libmyapp.a",
    Buildmode: dagger.GoBuildModeCArchive,
})

// Test returns combined stdout.
stdout, err := g.Test(ctx, src, dagger.GoTestOpts{Race: true})
```

See `tests/main.go` for one example per function.

## Examples

`examples/go/` is a runnable cookbook module: four recipes that call go the way
a downstream consumer would. Every recipe defaults to the sample module
vendored at `examples/go/sample/` (a two-package, stdlib-only
`example.com/greeter`), so each one runs with no arguments; pass `--source` to
point it at your own tree.

```sh
dagger -m github.com/z5labs/devex/daggerverse/go/examples/go call test-package
dagger -m github.com/z5labs/devex/daggerverse/go/examples/go call \
    build-binary export --path ./greeter
```

`build-binary` compiles a tree down to the one executable you ship;
`test-package` returns readable `go test` output; `module-hygiene` gates on
gofmt and vet before handing back a tidied source tree; `install-tool` shows
how a pinned Go CLI becomes a `*File` you can mount anywhere. The suite in
`tests/` runs every recipe, so the cookbook cannot silently rot against the
API.

## CI pipeline (`Ci` builder)

`Ci(source)` returns a builder that composes Go static checks and build
into a single staged pipeline.

### Stages

1. **Parallel checks** — enabled individually via `WithFmt()`, `WithVet()`,
   `WithLint(version, config)`, `WithTest(race)`. Errors from enabled
   checks are aggregated via `github.com/dagger/dagger/util/parallel`;
   stage 2 is short-circuited on any stage-1 failure.
2. **Build** — always runs after stage 1 succeeds. `WithBuild(pkg, binaryName)`
   customises the build parameters (both optional). `pkg` defaults to `.`;
   `binaryName` defaults to the basename of the `module` directive in
   `go.mod`. `Run` returns the built binary as a `*File`.

`Ci` is the entrypoint. Whatever a downstream pipeline does with the
returned binary — package, sign, scan, publish — is up to the caller.

### CLI

    dagger -m daggerverse/go call ci \
        --source=path/to/project \
        with-fmt with-vet with-test --race=true with-lint \
        with-build \
        run export --path=/tmp/my-app

### Go SDK

```go
// Language Ci produces the artifact; downstream pipeline composes it.
binary := dag.Go().Ci(src).
    WithFmt().
    WithVet().
    WithLint().
    WithTest(dagger.GoCiWithTestOpts{Race: true}).
    WithBuild().
    Run()

if _, err := dag.Container().
    From("gcr.io/distroless/static:nonroot").
    WithFile("/app", binary).
    WithEntrypoint([]string{"/app"}).
    Publish(ctx, "registry.example.com/my-app:latest"); err != nil {
    return err
}
```

The `WithBuild` second parameter is named `binaryName` (CLI flag
`--binary-name`) to avoid colliding with Dagger CLI's top-level
`--output/-o` flag.
