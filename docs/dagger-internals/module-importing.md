# Module importing

How a Dagger module declares, loads, and consumes another module — with
a focus on why types can be shared across SDKs but native language types
cannot cross a module boundary.

> Engine version: `v0.20.8`. Source permalinks are pinned to
> `dagger/dagger` commit
> [`74bff7d`](https://github.com/dagger/dagger/tree/74bff7d10fd78dd6935c60c4514558598f216451).
> See [README.md](./README.md) for the sourcing rule.

## Overview

Every `daggerverse/` module in this repo declares its dependencies in
`dagger.json`. For example `daggerverse/kafka/dagger.json`:

```json
{
  "name": "kafka",
  "engineVersion": "v0.20.8",
  "sdk": { "source": "go" },
  "dependencies": [
    { "name": "certificate-management", "source": "../certificate-management" },
    { "name": "crypto",                  "source": "../crypto" },
    { "name": "random",                  "source": "../random" }
  ]
}
```

That declaration is consumed at two distinct times, by two distinct
mechanisms:

- **Codegen time** (`dagger develop`) — the SDK generates client
  bindings so the module's source code can *call* its dependencies.
- **Call time** (`dagger call`) — the engine loads each dependency
  module, runs its SDK runtime to register type definitions, and serves
  them in one unified GraphQL schema.

The two are independent: codegen produces source files on disk; call
time spins up containers in the engine. Neither step ever links the
dependency's compiled code into the consumer.

## How a module gets run

The engine is the only GraphQL server. An imported module is **not** a
GraphQL service of its own — it is a container with an entrypoint that
the engine execs in one of two modes
([`core/sdk.go` `Runtime` interface doc, L304-L322](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/sdk.go#L304-L322)):

- **No object selected** — the entrypoint walks the module's own type
  information (Go reflection, Python introspection, …) and calls back
  into the engine's GraphQL API to register the module's typedefs.
  Done once per load.
- **Object/function selected** — the entrypoint executes that function
  and writes the result to a meta file the engine reads.

When this page refers to the engine calling a "GraphQL function" on an
SDK (e.g. `codegen` in the next section), that field lives in the
*engine's* schema; the engine resolves it by exec'ing the SDK's own
runtime container — the same exec-the-entrypoint mechanism, applied to
a meta-module (the SDK) rather than a user module. The detailed walk
through this — including the `withExec` + meta-file dance — is in
[Client codegen vs. server runtime](#client-codegen-vs-server-runtime)
below.

## Declaring a dependency: codegen time vs. call time

### Codegen time (`dagger develop`)

`dagger develop` regenerates the module's client bindings. An SDK opts
into this by implementing the `CodeGenerator` interface — the engine
calls the SDK's `codegen` GraphQL function, passing the module source
**and an introspection-JSON file of the schema visible to the module**:

> `core/sdk.go`,
> [`CodeGenerator` interface, L99-L130](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/sdk.go#L99-L130):
> ```go
> // SDK must implement the `Codegen` function with the following signature:
> //   codegen(
> //       modSource: ModuleSource!
> //       introspectionJSON: File!
> //   ): GeneratedCode!
> ```

The `introspectionJSON` argument is the whole story: codegen does not
read the dependency's Go/Python/TypeScript source. It reads a GraphQL
introspection schema. The engine builds that file from the module's
dependency set —
[`core/schema/module.go` `moduleIntrospectionSchemaJSON`, L1005-L1011](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/schema/module.go#L1005-L1011)
calls `mod.Deps.SchemaIntrospectionJSONFileForModule(ctx)`.

**Experiment — codegen produces one client file per dependency.** The
generated client files are not committed (`daggerverse/kafka/.gitignore`
ignores `/dagger.gen.go` and `/internal/dagger`); `dagger develop`
recreates them:

```
$ cd daggerverse/kafka && dagger develop
✔ develop 4.0s

$ ls internal/dagger/
certificate-management.gen.go
crypto.gen.go
dagger.gen.go
random.gen.go
```

One `*.gen.go` per entry in the `dependencies` array
(`certificate-management`, `crypto`, `random`), plus an `internal/dagger`
`dagger.gen.go` for the core API and a `dagger.gen.go` at the module
root for the module itself. The generated code carries source-map
comments back to the dependency's own source:

```
$ grep -nE 'type (Random|Crypto) struct' internal/dagger/random.gen.go internal/dagger/crypto.gen.go
internal/dagger/random.gen.go:72:type Random struct { // random (../../../../daggerverse/random/main.go:17:6)
internal/dagger/crypto.gen.go:63:type Crypto struct { // crypto (../../../../daggerverse/crypto/main.go:37:6)
```

So at codegen time a dependency is just a set of generated client stubs
(`Random`, `Crypto`, `CryptoRsaKey`, …) that issue GraphQL queries. No
dependency code is imported; nothing executes yet.

### Call time (`dagger call`)

At call time the dependency is a *running module*, not a source file.
The consumer's `dagger.json` dependency list is resolved into a
dependency DAG (`mod.Deps`). Each dependency module is loaded and its
type definitions installed into the served schema —
[`core/module.go` `Install`, L565-L632](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/module.go#L565-L632)
walks the module's `ObjectDefs` / `InterfaceDefs` / `EnumDefs` and
installs each into the `dagql` server. The dependencies a module can see
are exactly its **direct** dependencies — type resolution against deps
goes through
[`modTypeFromDeps`, L760-L771](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/module.go#L760-L771),
which is gated on `checkDirectDeps` and never recurses into transitive
deps.

**Experiment — a module's surface at call time.** `dagger functions`
introspects the live schema the engine assembled:

```
$ dagger -m daggerverse/random functions
Name       Description
serial     Serial generates a random n-byte X.509 serial number ...
sha-256    Sha256 generates a random n-byte value and returns its SHA-256 hash ...
sha-512    Sha512 generates a random n-byte value and returns its SHA-512 hash ...
uuid-v-4   UuidV4 generates a random UUID version 4 and returns it as a string.
uuid-v-7   UuidV7 generates a random UUID version 7 and returns it as a string.
```

## The GraphQL boundary

A module never links against its dependency. The only contract between
them is the **GraphQL schema**. The introspection JSON passed to
`codegen` (above) is the language-neutral description of that schema;
the dependency's SDK is irrelevant to the consumer.

This is why a Go module can depend on a Python module (or vice versa)
with no special handling: the consumer's SDK generates client bindings
purely from the introspection JSON, and the dependency's SDK runtime
answers GraphQL queries. Each SDK implements three engine-facing
GraphQL functions, all of which take the same `introspectionJSON: File!`
argument and are entirely SDK-internal —
[`core/sdk.go` `SDK` interface, L387-L408](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/sdk.go#L387-L408):

| SDK capability   | GraphQL function | Used by         |
| ---------------- | ---------------- | --------------- |
| `CodeGenerator`  | `codegen`        | `dagger develop`|
| `Runtime`        | `moduleRuntime`  | `dagger call`   |
| `ModuleTypes`    | `moduleTypes`    | self-calls      |

Because both directions of the boundary — generating a client and
answering a query — are defined only in terms of the GraphQL schema,
the SDKs on either side are decoupled. The schema is the interface
definition language; the introspection JSON is its serialization.

## How values cross the boundary

### Objects cross as IDs

Every object type in the schema — core types *and* module-defined types
— is addressable by an opaque **ID scalar**. When a value is passed
between modules, the object itself stays in the engine; only its ID
crosses the wire.

**Experiment — IDs in the schema.** Introspecting the `random` module's
schema shows both a core object (`File`) and a module object (`Random`)
expose an `id` field, and each has a matching `*ID` scalar (output
elided to the relevant fields with `…`):

```
$ echo '{ f: __type(name:"File") { kind fields { name type { ofType { kind name } } } }
          r: __type(name:"Random") { kind name }
          i: __type(name:"RandomID") { kind name description } }' | dagger -m daggerverse/random query

{
    "f": {
        "fields": [
            …
            {
                "name": "id",
                "type": {
                    "ofType": {
                        "kind": "SCALAR",
                        "name": "FileID"
                    }
                }
            },
            …
        ],
        "kind": "OBJECT"
    },
    "i": {
        "description": "The `RandomID` scalar type represents an identifier for an object of type Random.",
        "kind": "SCALAR",
        "name": "RandomID"
    },
    "r": {
        "kind": "OBJECT",
        "name": "Random"
    }
}
```

An ID encodes the full call chain that produced the object, so the
receiving side can reconstruct (or look up) the object without the
producing module needing to be the same SDK — or even still running in
the same form.

### Core types vs. module-defined types

A type def resolves to one of three origins —
[`core/module.go` `ModTypeFor`, L709-L758](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/module.go#L709-L758):

- **Primitives** (`String`, `Int`, `Float`, `Boolean`, `Void`) — always
  pass.
- **Core types** — objects owned by the built-in `daggercore` module
  (`Container`, `Directory`, `File`, `Secret`, `Service`, …). The core
  module name is the constant `ModuleName` —
  [`core/moddeps.go` L19](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/moddeps.go#L19):
  `ModuleName = "daggercore"`.
- **Module-defined types** — objects/interfaces/enums declared by a
  specific user module, namespaced to it.

### Why arbitrary native types cannot cross modules

A native language type (a Go `*rsa.PrivateKey`, a Python `dict`) is not
in the schema at all, so it can never cross. But the boundary is
stricter than that: **even a rich type defined by one user module
cannot appear in another user module's public API.** This is enforced
at module load time —
[`core/module.go` `validateObjectTypeDef`, L847-L936](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/module.go#L847-L936).

For an object's fields
([L879-L887](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/module.go#L879-L887)):

```go
// fields can reference core types and local types, but not types from other modules
if sourceMod != nil && sourceMod.Name() != ModuleName && sourceMod != mod {
    return fmt.Errorf("object %q field %q cannot reference external type from dependency module %q",
        obj.OriginalName, field.OriginalName, sourceMod.Name())
}
```

The same rule is repeated for function **return types**
([L902-L910](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/module.go#L902-L910):
`cannot return external type from dependency module`) and function
**arguments**
([L915-L933](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/module.go#L915-L933):
`cannot reference external type from dependency module`). The condition
`sourceMod.Name() != ModuleName` is what whitelists core types: a
function may freely accept or return `*dagger.File` or `*dagger.Secret`
(owned by `daggercore`) but never a `*dagger.KeyStore` defined by the
`certificate-management` module.

**The practical pattern.** Because rich dependency types cannot cross,
a module that needs material produced by another module accepts the
underlying *core* primitives and re-hydrates internally. `kafka`'s
internal CA does exactly this — `daggerverse/kafka/internal_ca.go` calls
`dag.Crypto()`, `dag.Random()`, and `dag.CertificateManagement()`,
moving only `*dagger.File` and `*dagger.Secret` (plus strings) across
each boundary, and reconstitutes rich objects with the dependency's
`Load*` helpers on its own side. A module that produces such material
mirrors the shape: return `*dagger.File` + `*dagger.Secret`, not a
local struct, and ship a `Load*FromPkcs12`-style constructor so
downstream modules have a clean re-hydration path.

## Client codegen vs. server runtime

It is worth being precise about what lives where:

- **Client-side (generated by codegen, compiled into the consumer).**
  The `internal/dagger/*.gen.go` files. Calling `dag.Random().Sha256()`
  from `kafka` runs *generated stub code* that builds a GraphQL query.
  It contains no `random` module logic.

- **Server-side (executes in the engine).** The actual `random` module
  code runs in **its own runtime container**, separate from `kafka`'s.
  Each module's runtime is built from its SDK —
  [`core/module.go` `LoadRuntime`, L1147-L1166](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/module.go#L1147-L1166)
  asks the SDK for a `Runtime`, and
  [`core/schema/module.go` `moduleRuntime`, L888-L899](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/schema/module.go#L888-L899)
  exposes it as a `Container`.

For a container-based SDK the runtime is a `Container` with an
entrypoint. A function call mounts a metadata directory and execs that
entrypoint —
[`core/sdk.go` `ContainerRuntime.Call`, L166-L257](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/sdk.go#L166-L257)
does a `withExec` with `useEntrypoint: true` and reads the result back
from a meta file. The entrypoint has two modes —
[`core/sdk.go` `Runtime` interface doc, L304-L322](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/sdk.go#L304-L322):
with no object selected it **registers type definitions**; with an
object selected it **executes the requested function**.

So when `kafka` calls `dag.Random().Sha256()`:

1. `kafka`'s generated client emits a GraphQL query.
2. The engine routes it to the `random` module.
3. The engine starts `random`'s **own runtime container** (its own SDK,
   its own image) and execs its entrypoint to run `Sha256`.
4. The result returns to `kafka` as a plain GraphQL value.

`kafka` and `random` never share a process, an address space, or a
language runtime. Each module is an isolated container; the GraphQL
schema is the only thing between them.

The unified schema the consumer sees is filtered so that only
module-sourced functions appear as top-level entrypoints — core `Query`
functions are stripped when `hideCore` is set —
[`core/schema/module.go` `stripCoreQueryFunctions`, L941-L965](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/schema/module.go#L941-L965).
The list of a module's installed dependencies is itself queryable —
[`moduleDependencies`, L1121-L1139](https://github.com/dagger/dagger/blob/74bff7d10fd78dd6935c60c4514558598f216451/core/schema/module.go#L1121-L1139).

## Open questions / unverified

- **Transitive dependency visibility.** `modTypeFromDeps` only checks
  *direct* deps (`checkDirectDeps`). The exact behavior when a
  dependency itself re-exposes a transitive type (e.g. via an
  interface) was read from source but not exercised with a live
  multi-level experiment here.
- **Cross-SDK demonstration.** The cross-SDK claim is argued from
  source (the SDK-agnostic `introspectionJSON` boundary) only — every
  module in this repo is Go, so no live "Python module consumed from
  Go" experiment was run. The mechanism is source-backed; an end-to-end
  cross-SDK call was not.
- **ID portability across sessions.** Object IDs encode a call chain;
  whether/how an ID minted in one engine session can be resolved in a
  later session was not tested.
- **Runtime container reuse.** Whether two modules using the same SDK
  and version share a runtime base image layer in the engine cache was
  not measured; only that each module loads its own `Runtime` was
  confirmed from source.
