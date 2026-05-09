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
