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

That committed binding is the only file under `ci/` that does *not*
force the change-aware selector onto the full suite: `affectedpkg`
attributes `ci/internal/dagger/<toolchain>.gen.go` back to the toolchain
it was generated from (see `AggregatorBindings`), so a toolchain-adding
PR runs that toolchain's checks rather than the whole universe. Every
other path under `ci/` — including the module's own `dagger.gen.go` —
still fails open to the full suite.
