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
