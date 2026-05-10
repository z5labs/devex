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
| `Build(source, pkg, output, flags)` | `go build -o /out/[output] ...`; returns `/out` as a `*Directory`. `pkg` defaults to `./...`. |
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
```

## Go SDK quick reference

```go
g := dag.Go() // or dag.Go(dagger.GoOpts{Version: "1.23"})

// Build returns the /out directory containing the produced binaries.
out := g.Build(src, dagger.GoBuildOpts{Pkg: "./...", Output: "myapp"})

// Test returns combined stdout.
stdout, err := g.Test(ctx, src, dagger.GoTestOpts{Race: true})
```

See `tests/main.go` for one example per function.
