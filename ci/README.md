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
