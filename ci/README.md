# ci

Root anchor module for `dagger check -l`. Each `daggerverse/<m>/tests`
suite is installed as a **toolchain** in the root `dagger.json`, which
makes its `+check` functions discoverable here as `<m>-tests:all`
(toolchains transitively surface `+check`; plain dependencies do not).

## Run locally

```sh
dagger check -l                   # list every <m>-tests:all check
dagger check 'kafka-tests:all'    # run one
dagger check                      # run them all (one engine)
```

CI fans each check onto its own runner via the `list` → `run` matrix in
`.github/workflows/ci.yml`, which keeps the per-suite engines isolated.

## Codegen freshness

`ci:generated` fails when a module's committed `dagger.gen.go` or
`internal/dagger/*.gen.go` differ from what `dagger develop` produces at
the pinned `engineVersion`. It covers every `dagger.json` in the
workspace — the root module's per-toolchain aggregator bindings under
`ci/internal/dagger/` included — and names each stale module, printing
its patch:

```
==> daggerverse/kafka/tests is not up-to-date:
<patch>
generated files are not up-to-date; run `dagger develop` in: daggerverse/kafka/tests
```

Dependency bindings embed the source location of every function
(`// kafka (../../../../../daggerverse/kafka/cluster_kafka.go:401:1)`), so
an edit that only shifts line numbers in `daggerverse/<m>` still leaves
every dependent module stale. Re-run `dagger develop` in the module *and*
in each dependent.

`ci:generated-self-test` guards that check: it runs the same comparison
against one module twice, pristine and then deliberately made stale, and
fails unless the stale copy is reported. `ci:generated` previously routed
through `Workspace.Generators()` — empty unless a module declares a
`+generator` function — and so passed unconditionally for months (#184);
the self-test is what makes that failure mode impossible to repeat
silently.

## Adding a new daggerverse module

When you add `daggerverse/<m>/` with a sibling `tests/` module:

1. Keep `+check` on `tests.Tests.All()` (the convention every existing
   module follows).
2. Append a toolchain entry to the root `dagger.json`:

   ```json
   { "name": "<m>-tests", "source": "daggerverse/<m>/tests" }
   ```

3. Run `dagger develop` at the repo root to regenerate bindings, and
   commit the new `ci/internal/dagger/<m>-tests.gen.go`.

No `ci/main.go` edit needed — toolchains surface their `+check`
functions directly. No `.github/workflows/ci.yml` edit needed either —
the matrix picks up the new check from `dagger check -l` output
automatically.

That committed binding is one of only two files under `ci/` that do
*not* force the change-aware selector onto the full suite: `affectedpkg`
attributes `ci/internal/dagger/<toolchain>.gen.go` back to the toolchain
it was generated from (see `AggregatorBindings`), so a toolchain-adding
PR runs that toolchain's checks rather than the whole universe. The
other is `ci/README.md`, which this module declares out of its source
context. Every other path under `ci/` — including the module's own
`dagger.gen.go` — still fails open to the full suite.

## What counts as a change

The selector asks whether a changed path is an **input** to a module,
not merely whether it sits inside one (#195). A module's inputs are its
Dagger *source context* — precisely the files Dagger uploads for it —
read from the engine via `ModuleSource.ContextDirectory`, never modelled
in Go. A file outside that context is shipped to no engine, so it cannot
affect any check the module feeds. That is a property Dagger enforces,
not one this package asserts about which files look like prose.

Narrow a module's inputs with a negated `include` pattern in its
`dagger.json` (`include` unions on top of the source directory, so only
`!` subtracts):

```json
{ "include": ["!README.md"] }
```

Every module with a root `README.md` declares it out this way, so a docs
edit no longer drags in that module's dependents. The consequence to
respect: **a module must not read a file it declared out.** Doing so
fails loudly at build time rather than silently under-running CI — which
is why `daggerverse/skill-gen`, whose `skill/render.go` embeds
`templates/README.md.tmpl`, declares nothing out.

Repo-level prose needs no declaration at all. The root module's source
is `ci/`, so its context is exactly `ci/**` plus the root `dagger.json`;
the top-level `README.md`, `LICENSE`, and `docs/**` were never inputs to
anything.

Attribution then falls out in four cases:

| changed path | selects |
| --- | --- |
| in a module's source context | that module and its dependents |
| in the root module's context (`ci/**`, root `dagger.json`) | the full suite |
| under `.github/workflows/` | the full suite — it governs how CI runs |
| in no module's source context | nothing beyond the always-on `ci:*` |

Two deliberate asymmetries. The **innermost** module owns a path, so
`daggerverse/crypto/tests/main.go` belongs to `crypto/tests` and not to
`crypto`, even though Dagger's context for `crypto` contains it — nested
modules are separate Go modules that never compile into their parent.
And a **deleted** path is attributed to its module rather than read as
declared out, because no head source context can contain it; that
over-runs when a declared-out file is deleted, which is the direction
this package errs in.

Selection is a diff against one base, so it cannot skip work a previous
run already proved good — a rebase onto an already-tested `main` re-runs
everything. Memoizing check results by input hash is tracked in
[#238](https://github.com/z5labs/devex/issues/238).
