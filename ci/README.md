# ci

Root anchor module for `dagger check -l`. Re-exposes each
`daggerverse/<m>/tests.All()` as a `ci:<m>-tests-all` check so that
`.github/workflows/ci.yml` can discover them at job start and fan each
one onto its own runner with a fresh engine.

The wrappers exist because Dagger does not transitively walk `+check`
on dependencies — only checks defined directly on the loaded module
show up in `dagger check -l`. Each wrapper is a one-call passthrough
to the dep.

## Run locally

```sh
dagger check -l                       # list every ci:<m>-tests-all check
dagger check 'ci:kafka-tests-all'     # run one
dagger check                          # run them all (one engine)
```

CI fans each check onto its own runner via the `list` → `run` matrix in
`.github/workflows/ci.yml`, which keeps the per-suite engines isolated.

## Adding a new daggerverse module

When you add `daggerverse/<m>/` with a sibling `tests/` module:

1. Keep `+check` on `tests.Tests.All()` (the convention every existing
   module follows).
2. Append a dependency entry to the root `dagger.json`:

   ```json
   { "name": "<m>-tests", "source": "daggerverse/<m>/tests" }
   ```

3. Add a wrapper method on `Ci` in `ci/main.go`:

   ```go
   // +check
   func (c *Ci) <M>TestsAll(ctx context.Context) error {
       return dag.<M>Tests().All(ctx)
   }
   ```

4. Run `dagger develop` at the repo root to regenerate bindings.

No `.github/workflows/ci.yml` edit needed — the matrix picks up the new
check from `dagger check -l` output automatically.
