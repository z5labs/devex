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

## Skipping work a previous run already proved good

Selection is a diff against one base, so on its own it can only skip work
unaffected by *this* PR — never work an earlier run already proved good.
A PR's second and later pushes re-run everything that was green on the
first, because the diff is still measured from the PR base. Memoization
closes that gap (#238): every check gets an **input hash**, a passing
check records it, and a later run that computes the same hash skips the
check.

The hash is a Merkle fold over **git blob object ids** — sorted
`(path, blobOID)` per module source context, folded over the check's
transitive dependency closure, plus the check's own name and the global
inputs. Git object ids, not `Directory.digest`: Dagger's digest format is
explicitly not stable across releases, so a persisted set keyed on it
would be invalidated wholesale by every engine bump. An engine bump still
invalidates every check, correctly — `engineVersion` is a field in all
~50 `dagger.json` files, and each is in its own module's source context.

Two checks on one module hash apart (the name is in the digest), and the
per-toolchain aggregator bindings are folded into their own toolchain
rather than the global inputs, for the same reason `AggregatorBindings`
exists (#179): repo convention regenerates one on nearly every module
change, and treating it as global would mean memoization never hit.

The store is the **GitHub Actions cache**: a passing leg writes a marker
entry keyed `ci-memo-v1-<hash>`, and the `list` job reads back the keys.
Only two cache scopes are read — the default branch, and a PR's own scope
— both of which GitHub itself confines, since a run can only write caches
under its own `GITHUB_REF`. No other branch and no fork can plant an
entry CI will read; that is stricter than GitHub's own restore rules,
which would also expose a stacked PR's base branch.

Within a PR's own scope the writer is that PR, so the selector refuses to
honour *any* recorded pass once a **global input** changed —
`.github/workflows/**` or the root module's source context. Those are the
only paths that can alter how a pass is recorded, and a change to one
also perturbs every input hash, so a forged entry lands in a hash space
no untampered tree ever computes and reverting the tamper restores the
honest hashes rather than matching the forged ones. See `MemoTrusted`.

### The two root-context paths that are not global

Adding a daggerverse module is the change that touches least and, left
alone, would cost most. It necessarily edits three files under the root
module, and if all three were global inputs it would retire the entire
store. So `nonGlobalRootPaths` excludes two of them from both the global
hash and the trust judgement, and `AggregatorBindings` already handled
the third:

| file the PR touches | treatment |
| --- | --- |
| `dagger.json` (the toolchain entry) | not a global input (`rootConfig`) |
| `ci/internal/dagger/dagger.gen.go` (regenerated: one ID type + one loader) | not a global input (`coreBinding`) |
| `ci/internal/dagger/<new>-tests.gen.go` | attributed to the new toolchain (#179) |

**`dagger.json`** decides which checks *exist*, never what an existing
check *computes*. Everything it carries is covered downstream:

| field | already covered by |
| --- | --- |
| `engineVersion` | all 47 `dagger.json` carry it, and the other 46 are each in their own module's source context |
| `toolchains` | repointing an entry changes `Check.OriginalModule`, hence the closure, hence the hash; and no non-`ci` leg ever loads the root module — `ci.yml` routes each at its own tests module |
| `sdk`, `source`, `include`, `codegen` | scope the root module, which is never memoized; and `include`/`source` decide which paths are in the root context at all, so hiding a file with them removes it from the global set and moves the digest anyway |

**`ci/internal/dagger/dagger.gen.go`** is the sharper case, because it is
the API surface the hasher itself executes: a hand-patched `Glob` could
make some module's hash go blind to that module's sources, recording a
pass on good content and matching it against bad. What forecloses that
is not the digest but **`ci:generated`** — it proves every committed
generated file equals what `dagger develop` produces, it always runs,
and it is never memoized (with `ci:generated-self-test` guarding it,
after #184). A tampered binding is red at the gate on the very push that
would act on it, and reverting to go green restores the honest hash,
which the recorded entry no longer matches.

Generalised: **a generated file need not be a global input, because a
never-memoized check already proves it is derived from inputs that are.**
That is the reasoning `AggregatorBindings` applies to the per-toolchain
bindings, extended to the one binding attributable to no toolchain.

Both are subtractions of a path's *content*, not of set membership —
every other root-context path still reaches every hash, so the authored
machinery under `ci/**` stays covered. Selection is untouched: either
path changing still runs the full universe, because the set of checks
really did change. Together they give the intended shape: **adding a
module runs `ci:*` plus the new module's checks, and retires every
untouched check on its unchanged hash.**

### Base-image drift

A source-derived hash cannot see upstream drift: these checks boot real
services from floating tags, so an identical tree does not imply an
identical container. The answer is an explicit **24h bound** on how long
a recorded pass may be honoured (`MEMO_TTL_SECONDS` in
`.github/workflows/ci.yml`, applied against each cache entry's
`created_at`).

Folding resolved image digests into the hash was rejected: it means
resolving every module's images, which is most of the cost memoization is
meant to avoid, and it breaks the git-object-id property above. Bounding
the window instead caps drift-blindness at a day, which is where
essentially all the reuse is anyway — memoization pays off across
hours-scale PR iteration.

### What is never memoized

Same rule as selection: never skip a check a change could plausibly
affect.

| condition | result |
| --- | --- |
| `ci:*` checks | always run — `ci:generated` reads the whole workspace, which its closure does not describe |
| a module's source context unreadable | that module's dependents are unhashable, so they run |
| a source path absent from `HEAD` (untracked, dirty tree) | its module is unhashable, so its dependents run |
| a global input changed | recorded passes are ignored wholesale |
| the store unreadable, or an entry older than 24h | the check runs |

A check that cannot be hashed reports an empty hash, and the workflow
records nothing for it — so an unmemoizable check can never accidentally
retire a later run.
