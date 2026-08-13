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

The consumer side of that was measured on 2026-08-13, anonymously, with
`oras` v1.2.3 — a client built on `oras-go`, so it inherits the fallback and
needs no flag to use it. `oras discover ghcr.io/actions/actions-runner:latest`
returns a real referrer (`application/vnd.dev.sigstore.bundle.v0.3+json`),
and the whole read path works against GHCR:

```
oras discover "$repo:$tag" --artifact-type <type> --format json   # .manifests[].digest
oras manifest fetch "$repo@$referrer"                             # .layers[0].digest
oras blob fetch "$repo@$layer" --output -                          # the document
```

Note `--format json` keys the list `manifests`, not `referrers`. The other
half of that measurement is the negative one, and it is what
`daggerverse/z5labs`'s package doc now warns adopters about:
`cosign verify-attestation` against an image whose attestations are
referrers only exits 1 with `Error: no matching attestations:` — a message
indistinguishable from an image that was never attested. cosign reads its
own `sha256-<hex>.att` tag convention and does not consult referrers unless
built with `--experimental-oci11`.

Two consequences worth knowing before debugging one of them:

- **The fallback tries to delete a manifest, and GHCR refuses.** Attaching a
  *second* referrer to one subject means replacing the tag's index, after
  which `oras-go` deletes the index it replaced. GHCR does not support
  manifest `DELETE` and answers `405 unsupported`, which fails the whole
  push — *after* the referrer and the updated index have both landed. Set
  [`remote.Repository.SkipReferrersGC`][skipgc] to keep the housekeeping
  from failing a publish; `daggerverse/oci` does, and the cost is one
  unreferenced index per replacement. Do not paper over it in a test
  registry with `REGISTRY_STORAGE_DELETE_ENABLED=true`: that makes the
  suite green against a registry no publish target resembles, which is how
  devex#360 reached a real release.
- **A referrer is addressed by digest, not by a tag of its own.** Push it
  with `oras.CopyGraph`, not `oras.Copy` — see `daggerverse/oci/artifact.go`.

[skipgc]: https://pkg.go.dev/oras.land/oras-go/v2/registry/remote#Repository

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

## Signing an image: copy cosign's layout, do not design one

A signature is worth exactly what the command that verifies it is worth, and
the command a consumer already has is `cosign verify`. So `daggerverse/z5labs`
writes cosign's layout rather than something of its own: for a manifest at
`sha256:<hex>`, a one-layer OCI **image** manifest under the tag
`sha256-<hex>.sig` in the same repository, the layer holding the simple-signing
payload under `application/vnd.dev.cosign.simplesigning.v1+json` and the
signature living in the layer's annotations.

Three things that are easy to get wrong and are not obvious from the spec:

- **Sign the index *and* every per-platform manifest.** A consumer verifies the
  tag, which resolves to the manifest list; their runtime then pulls the
  per-platform manifest, a different digest the index signature says nothing
  about. Signing only the index leaves `cosign verify <tag>` passing over bytes
  nothing checked, which is worse than not signing. This is what cosign's own
  `--recursive` is for.
- **The signature manifest needs a real image config**
  (`application/vnd.oci.image.config.v1+json` over `{}`), not the OCI empty
  descriptor `oras.PackManifest` picks. Readers go through
  go-containerregistry's image type; the artifact-manifest shape is legal OCI
  and is not what they parse. `daggerverse/oci`'s `PushLayer` exists for
  exactly this and assembles the manifest by hand.
- **Keyless signing needs a transparency log entry, not just a certificate.**
  The Fulcio certificate binding the ephemeral key to a workload identity lives
  for minutes, so by verification time it has expired and nothing establishes it
  was valid when it signed. The Rekor entry's countersignature is what does.
  Embed the bundle in the `dev.sigstore.cosign/bundle` annotation rather than
  leaving it to be looked up, so a consumer behind a network that cannot reach
  `rekor.sigstore.dev` can still verify.

### zot parses any tag ending in `.sig`, and a short digest panics it

The `oci` test suite runs against **zot**, not `registry:2`, and zot inspects
every pushed tag to decide whether it is a cosign signature
(`zot/pkg/meta.isSignature`). It assumes a full-length digest and slices
`tag[:71]`, so a made-up short tag like `sha256-0123456789abcdef.sig` panics the
handler. What reaches the client is `500 Internal Server Error` with an empty
body — which reads as a malformed manifest and is a malformed *tag*.

Compute signature tags from a digest the registry just returned, in tests as
well as in production. Half an hour went into the manifest before the trace was
read; `dagger trace <id> --progress=plain` had the panic and its stack in it all
along.

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

The **object**-returning counterpart is safe, and this has now been measured
rather than assumed (devex#402, Dagger v0.21.8). A method
`func (m *Z5labs) Go(source *dagger.Directory) *Go` — the name as both the
method and the returned object's type — generates, compiles and runs in a
consumer module:

```go
// source:   func (m *Z5labs) Go(source *dagger.Directory) *Go
// consumer: func (r *Z5Labs) Go(source *Directory) *Z5LabsGo
```

The reason is the cache field described just above, and nothing else. A
method name is emitted as a capitalized identifier, which is never a Go
keyword — `Go` is a legal identifier, only lowercase `go` is reserved. What
breaks is the *lowercased* cache field a scalar-returning method emits
(`go *string`), so a method that returns an object emits no field and there
is nothing to collide. The returned type namespacing to `Z5LabsGo` is a real
observation but not the mechanism; a type named `Go` would have been fine
too — as long as no dependency of that module already owns the name, which
is a separate constraint and is the next section.

So the rule is about the **return type**, not the name in isolation, and it
is per generated method rather than per module: adding a scalar-returning
`Go` method to any object — measured on a second object, not the keyword-named
one — breaks that object's generated type with `expected '}', found 'go'`.
A keyword-named object carrying only object-returning and non-keyword scalar
methods is fine.

Worth knowing while reading that pair: the schema name is derived from
the **module name**, not from the Go type's spelling. Module `z5labs` gives
`Z5Labs` in the bindings even though the Go source declares `type Z5labs
struct{}`, and secondary objects namespace onto that same normalized prefix.
The casing difference is the generator's, not a typo.

### A local object may not be named after one of your dependencies' objects

The measurement above says a keyword-named *object* is safe. It is — but only
where the name is free. A module's dependencies occupy their objects' bare
names in that module's own type space, so an object declared locally with a
name a dependency already owns is resolved as the **dependency's**, and the
module fails to load before any codegen happens:

```
failed to load module dependencies: failed to add object to module "z5labs":
failed to validate type def: object "Z5labs" function "Go" cannot return
external type from dependency module "go"
```

That is `daggerverse/z5labs` declaring `type Go struct{...}` while depending on
`daggerverse/go`, whose root object is literally `Go` (devex#403). The spike in
devex#402 could not have caught it: its throwaway module pair had no such
dependency, so the name was free there and is not here.

Two things worth carrying:

- It is about the **type**, not the method. `func (m *Z5labs) Go(source
  *dagger.Directory) *GoChain` loads fine and keeps `dagger call go ...` as the
  CLI path; only the returned type had to be renamed. So a collision costs a
  type's spelling, never the API's shape.
- The clash is with the dependency's object names as they appear *inside* this
  module (`Go`, `GoCi`, ... for a dep named `go`), not with the namespaced
  names a consumer sees (`Z5LabsGo`). Check `internal/dagger/<dep>.gen.go` for
  the names already taken before picking one.

### Unexported types are where module-object state belongs

A module object's fields are serialized across every call boundary, which
raises the question of whether a field holding a slice of structs needs
those structs registered as Dagger object types. It does not — unexported
Go structs round-trip fine (measured in devex#402):

```go
type App struct {
    Version  string     // +private
    Variants []*variant // +private   <- the +private is load bearing
}

type variant struct {
    Platform  dagger.Platform
    Container *dagger.Container
    Documents []document
}

type document struct {
    Name string
    Type string        // safe here; see below
    File *dagger.File
}
```

**Nothing about `variant` or `document` reaches the schema** — the consumer's
generated bindings contain no `Z5LabsVariant` and no `Z5LabsDocument`, only
the methods of `App` itself. The state round-trips intact, `*dagger.Container`
and `*dagger.File` included; those travel as IDs and resolve on the far side.
Verified both by chained resolution and by an explicit `ID()` →
`LoadZ5LabsAppFromID()` round trip, across three platforms, with the
containers and files still readable afterwards.

One ergonomic wrinkle when doing that ID round trip on a *dependency's*
object: the generated `ID()` on a dependency type returns the generic
`dagger.ID`, not the specific one, so `LoadZ5LabsAppFromID` needs an explicit
`dagger.Z5LabsAppID(id)` conversion. Own-module types are unaffected.

What keeps them out of the schema is that **no exposed API signature
references them** — not the lowercase initial by itself. Drop the `+private`
from `Variants` and you are asking the generator to expose a field whose type
it cannot register, which fails in the module's *own* `dagger develop`, before
any consumer is involved:

```
Error: generate code: template: module.go.tmpl:96:3: ... cannot code-generate
unexported type variant
```

Measured, because the causal link is easy to state backwards.

Two consequences:

- **The field-naming rules do not apply to unexported types.** A field
  literally named `Type` on `document` — the name that breaks dependency
  codegen outright on an *exported* struct — is harmless here, because there
  is no schema object for it to collide in. Confirmed by trying it. Name
  these fields for the code, not for the generator.
- Their fields still need to be **exported**, and getting this wrong fails
  silently and late. The round trip is `encoding/json`, so an unexported
  field is dropped on the way out and comes back as the zero value. Codegen
  still succeeds and the consumer still compiles. With a pointer, slice, map
  or interface field you at least get a crash the first time you touch it —
  measured, with a lower-cased `file *dagger.File`, giving `invalid memory
  address or nil pointer dereference`. With a scalar you get **no** crash: an
  unexported `count int` or `name string` comes back `0` or `""` and the
  module computes on wrong data indefinitely. The panic is the lucky
  outcome. The struct is unexported; its fields are not.

The registered alternative was never run — the criteria for this spike called
for it only if the unexported form failed, and it did not — so what follows is
reasoning rather than measurement: a registered helper type would add a schema
object with no methods that callers can see and must ignore, and would drag
the exported-field naming rules along with it. On that basis prefer the
unexported form, bearing in mind the silent-zeroing trap above is the price.

### `[]dagger.Platform` parameters carry variants intact

Nothing in this repo took a `dagger.Platform` as an *input* before — every
use constructed one — so it was unverified whether a slice of them survived
a call boundary. It does, order preserved and variant preserved:
`linux/amd64`, `linux/arm64` and `linux/arm/v7` all arrive byte-identical to
what the caller passed (devex#402). `Platform` is a string scalar in the
schema, so nothing rewrites the variant component in transit across a call
boundary.

That is a statement about **parameter marshaling only**. Platform values are
normalized in containerd-style handling elsewhere in the stack, and this
spike says nothing about what `WithPlatform`, image building or a
`Platform()`-returning call do with a `linux/arm/v7`; treat those as
untested.

### Backticks in a parameter's doc comment rename its CLI placeholder

A parameter's doc comment becomes that flag's usage string, and the flag
parser reads a **backticked word** in a usage string as the placeholder to
print in place of the type. So

```go
// The package to build, in `go build` package syntax.
//
// +optional
// +default="."
pkg string,
```

renders as `--pkg go build` rather than `--pkg string` — the type is gone
and the help now names something that is not a value the flag takes. It is
silent: the module builds, the bindings generate, and only `--help` shows
it.

Write parameter docs in plain prose. Backticks are fine in a *function* or
*type* doc comment, which is rendered as prose; it is only the per-parameter
comments that become usage strings. If a parameter's doc really needs to
quote a command, name it without the backticks.

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
