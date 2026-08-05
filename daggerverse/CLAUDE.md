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

## SBOM production belongs to the ecosystem module

An SBOM is produced by the module that owns the ecosystem — `go` for Go
binaries, `java` for JVM artifacts, and so on. There is no shared `sbom` module,
and no generic scanner either above the ecosystem modules or inside one.

Generic scanners exist because ecosystem tooling is written in a dozen different
languages, so no one format project can add native support everywhere. That
constraint does not apply here: every module is Go, so a module can import the
formats' own Go libraries, map its ecosystem data onto their types, and write
the document itself. A scanner would be re-deriving from the outside what the
module already holds.

Conventions for these functions:

- **Name the format, not the concept.** `CycloneDx()` / `Spdx()`, never
  `Sbom(format string)`. A stringly-typed switch has no per-format doc comment
  and no compile-time meaning — the same reason build flags are named inputs
  rather than a `[]string`.
- **Map onto the format's own Go library; do not serialize by hand.** Spec
  conformance is the library's job and the mapping is yours. Hand-rolled
  SPDX or CycloneDX output means owning a spec revision forever.
- **Resolve the dependency graph once, render every format from it.** Two
  formats derived from two separate resolutions can disagree about a component
  or a licence, and nothing downstream can adjudicate which is right. One
  resolution makes them consistent by construction.
- **The document's subject is the built artifact; its inputs may include the
  source.** The document must describe what shipped, not the tree that was
  present. But a compiled artifact carries no licence text — Go binaries embed
  module paths, versions and hashes only — so licence resolution needs the
  module cache or source. Taking source as an input is not the same as
  describing it.
- **Return a `*dagger.File`.** The consumer attaches or publishes it; it should
  not have to re-serialize. Write it with `dag.CurrentModule().WorkdirFile`,
  per the runtime-I/O pattern used elsewhere in this repo.
- **Keep documents comparable across modules.** Same spec version, same subject
  identification, describing the built artifact. Two ecosystem modules emitting
  structurally different SPDX is the cost of this layout, and the only thing
  preventing it is this convention.
- **Test for a compliant document, not a well-formed one.** The library
  guarantees valid syntax; it does not guarantee the fields a consumer requires
  are populated. Assert the required elements are present.

Licence identification is probabilistic — classifiers report coverage, not
verdicts. SPDX models this properly with declared vs concluded licences; decide
what a low-confidence match becomes rather than emitting whatever the classifier
returned.

Generation and attachment stay separate concerns. Whatever pushes bytes to a
registry does not know that those bytes are an SBOM.

## Attaching attestations: GHCR has no referrers API

Measured against `ghcr.io` on 2026-08-05, anonymously, against three public
repositories:

```
GET /v2/<repo>/referrers/<digest>        -> 404 MANIFEST_UNKNOWN
GET /v2/<repo>/manifests/sha256-<hex>    -> 200   (ghcr.io/sigstore/cosign/cosign)
```

The OCI distribution spec says a registry that does not implement the
referrers API answers 404 there, and a client then falls back to the
**referrers tag scheme** — an index stored under the tag `sha256-<hex>` of
the subject's digest. GHCR takes the fallback path, and the 200 on the
second line is a real cosign attestation index sitting on that tag today,
so the fallback is not theoretical. `oras-go` does the fallback itself;
nothing in this repo has to choose.

Two consequences worth knowing before debugging one of them:

- **The fallback needs manifest deletion.** Attaching a *second* referrer to
  one subject means replacing the tag's index, which means deleting the one
  it replaces. `registry:2` has deletion off by default, so the first
  attachment succeeds and the second fails with `405 unsupported` from a
  `DELETE`. Test registries need `REGISTRY_STORAGE_DELETE_ENABLED=true`;
  GHCR allows the delete.
- **A referrer is addressed by digest, not by a tag of its own.** Push it
  with `oras.CopyGraph`, not `oras.Copy` — see `daggerverse/oci/artifact.go`.

## Provenance: the identity comes from the token, never from a parameter

Anything a caller could have supplied attests to nothing. A build identity —
repository, workflow ref, commit, run id — is only provenance if it comes out
of a token the CI provider signed, so the module takes the *token request
machinery* (`ACTIONS_ID_TOKEN_REQUEST_URL` and its bearer, or any provider's
equivalent) and derives every identifying field from the exchanged token's
claims. There is deliberately no `repository` parameter to pass.

Two rules that follow, and that are easy to undo by accident:

- **Do not gate provenance on configuration.** "Attest when configured" is
  attestation nobody can rely on, because an image published without one is
  indistinguishable from an image published with one until somebody goes
  looking. A publish that cannot produce provenance fails.
- **Do not relax it for the test suite's shape.** Gating on
  `registryService != nil` carves the relaxation into exactly the shape of
  the tests and leaves the production path as the only unexercised one.
  `daggerverse/z5labs/tests` instead runs a *real* token endpoint as a
  service and exchanges a real token; only the signing key differs from
  production, where an ephemeral key is certified by the public sigstore CA.

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

### Scalar-returning methods named after Go keywords break the same way

The generator also caches every *scalar*-returning method on a private
struct field of the consumer's generated type, named after the GraphQL
field. A method named `Import` returning `(int, error)` therefore emits

```go
type <Dep><Type> struct {
    query *querybuilder.Selection

    import *int   // ← not valid Go
}
```

and the consumer's `dagger develop` fails with the same
`error formatting generated code: NNN:9: expected '}', found 'import'`.

This bites only methods whose return type is a scalar (`int`, `string`,
`bool`, `Void`); one returning `*dagger.File` or a module object gets no
cache field and is unaffected — which is why `Client.Export` (returns a
`*dagger.File`) is fine while `Client.Import` was not, and became
`ImportFile`. Avoid `Import`, `Range`, `Select`, `Go`, `Func`, `Map`,
`Chan`, `Default`, `Package`, and the rest of the Go keyword list as
scalar-returning method names.

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
