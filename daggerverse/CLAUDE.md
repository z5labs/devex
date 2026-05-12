# Daggerverse — Dagger module notes

## Function caching (Go SDK)

Dagger caches function results by default with a 7-day TTL. Two consecutive
calls to the same function with the same args return the **same** cached value
without re-executing. This breaks tests for non-deterministic functions
(random, UUIDs, timestamps).

Control caching with a `+cache=` directive in the doc comment:

```go
// +cache="never"   // re-run on every invocation (use for random/non-pure)
// +cache="session" // cache only for the lifetime of one engine session
// +cache="10s"     // TTL with s/m/h units
```

Place the directive on its own line in the doc comment block above the function.

Reference: https://docs.dagger.io/extending/function-caching/

## Regenerating bindings

After editing `main.go` (adding/renaming functions, changing signatures, or
changing directives like `+cache`), run `dagger develop` in the module
directory to regenerate `dagger.gen.go` and `internal/dagger/*.gen.go`.

If module A depends on module B (e.g. `tests/` depends on `..`), run
`dagger develop` in **both** so A picks up B's new API.

## Module layout

- `<module>/main.go` — the module's exported functions (must be `package main`).
- `<module>/dagger.json` — module config: name, engineVersion, sdk, dependencies.
- `<module>/dagger.gen.go`, `internal/dagger/` — generated; do not edit.
- `<module>/tests/` — a separate module that depends on `..` and exposes test
  functions discoverable via `dagger call <test-name>` or `dagger call all`.

## Function name mangling

Go method names get re-cased for the GraphQL API: acronyms become uppercase in
generated bindings (`UuidV4` in source becomes `UUIDV4(ctx)` on the dag client),
and CLI names become kebab-case (`Sha256ShouldNotBeCached` → `sha-256-should-not-be-cached`).

## Useful commands

- `dagger functions` — list functions exposed by the current module.
- `dagger call <fn> [--arg=val]` — invoke a function.
- `dagger develop` — regenerate SDK bindings after source changes.
- `dagger version` — engine and CLI version.

## Common pitfalls

These have all bitten this repo at least once. They live here so the
next module author doesn't lose an hour to them.

### Long-running service commands go in `AsService(opts.Args)`, not `WithExec`

`Container.WithExec` is a *build step*: Dagger runs the command
synchronously and waits for it to exit before continuing the chain. A
long-running server (HTTP server, `nc -l`, daemon loop) never exits,
so `WithExec(server).AsService()` deadlocks — `AsService` is never
reached and any consumer container with a `WithServiceBinding` to it
hangs.

Wrong:
```go
dag.Container().From("python:3-alpine").
    WithExposedPort(8080).
    WithExec([]string{"python", "-u", "-c", script}).  // blocks forever
    AsService()
```

Right:
```go
dag.Container().From("python:3-alpine").
    WithExposedPort(8080).
    AsService(dagger.ContainerAsServiceOpts{
        Args: []string{"python", "-u", "-c", script},
    })
```

`WithExec` is still correct for *finite* build steps before
`AsService` (e.g. `apk add`, `pip install`). Once you reach the
actual service process, switch to `AsService(opts.Args)` — or
`opts{UseEntrypoint: true, Args:...}` if the image's entrypoint
already runs the server. The otel and grafana-stack modules use the
Args form throughout: see `daggerverse/otel/main.go:82` and
`daggerverse/grafana-stack/main.go:118`.

### Struct fields named `Type` break downstream codegen

An exported field literally named `Type` on a Dagger module struct
makes the own-module `dagger develop` succeed but breaks dependency
binding generation in any consumer module with:

```
Error: generate code: generate dependency files: render dependency file for "<dep>":
error formatting generated code: NNN:9: expected '}', found 'type' (and 2 more errors)
```

The generator camelCases struct fields into schema names; `Type` →
`type` collides with the Go (and GraphQL) keyword and emits
unparseable Go in the consumer's `tests/internal/dagger/<dep>.gen.go`.

Use `Kind`, `Mode`, `Format`, or any other descriptive name. The same
applies to other Go/GraphQL keywords on exported fields: avoid
`Query`, `Mutation`, `Schema`, `On`, `Fragment`, and the scalar names
(`Int`, `Float`, `String`, `Boolean`, `ID`). Run `dagger develop` in
a *consumer* module after adding a new exported field to surface
this early.

### Method parameters named `r` collide with the generated receiver

The codegen renders methods as `func (r *<Type>) Method(<args>) ...`,
hardcoding the receiver to `r`. A parameter named `r` compiles fine
in the source module but produces:

```
internal/dagger/<dep>.gen.go:NNN: r redeclared in this block
```

in any consumer module. Use descriptive parameter names (`route`,
`recv`, `ep`, `cfg`) — single-letter `r` is the one that bites. Most
likely to surface on chained `WithR(r *R)` builders; otel avoids it
with `WithReceiver(recv *Receiver)`.

## TDD loop for module implementation

When building or extending a `daggerverse/<module>` package, drive
features one test at a time. **Do not** write the full module then run
the suite; do not even write all tests then implement.

1. Pick the next test from the acceptance-criteria (or design) list,
   easiest first — pure-validation tests before render-only tests
   before service round-trips.
2. Write only that test in `<module>/tests/main.go`.
3. Run `dagger develop` in `<module>` (if module API moved) and in
   `<module>/tests`.
4. Run `dagger -m daggerverse/<module>/tests call <test-name-kebab>`
   and confirm it fails for the *expected* reason (compile error,
   missing factory, validation gap) — not an unrelated reason.
5. Implement the **minimum** code in `<module>/main.go` to flip that
   single test green.
6. Re-run the single test until green.
7. Only then move to the next test.

Run `dagger -m daggerverse/<module>/tests call all` only at the end,
after every individual test is green. Reason: a single failure inside
the parallel aggregator triggers a cross-feature debugging trip and
buries the actual root cause under red herrings (a YAML rendering bug
masquerades as a network-binding bug; a validation-message mismatch
masks a real cluster-ref bug). Tight loops keep the failure surface
to the single feature just added.
