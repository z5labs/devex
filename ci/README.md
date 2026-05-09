# ci

Orchestrator dagger module that runs every `daggerverse/<m>/tests/All()` check
in parallel. Invoked by `.github/workflows/ci.yml` on pull requests and pushes
to `main`.

## Run locally

```sh
dagger -m ci call test
```

## Adding a new daggerverse module

When you add `daggerverse/<m>/` with a sibling `tests/` module, wire it into
the orchestrator:

1. Append a dependency entry to `ci/dagger.json`:

   ```json
   { "name": "<m>-tests", "source": "../daggerverse/<m>/tests" }
   ```

   Use a unique local name (`<m>-tests`); the inner module's own `name` is
   always `tests`, so the alias is what becomes the generated binding.

2. Add a job line to `ci/main.go`:

   ```go
   jobs = jobs.WithJob("<m>", func(ctx context.Context) error {
       return dag.<M>Tests().All(ctx)
   })
   ```

3. Run `dagger develop` in `ci/` to regenerate bindings.
