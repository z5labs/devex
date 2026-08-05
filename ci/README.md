# ci

The workspace's root module: the checks that must run for every change,
whatever it touched.

That is the whole of its job. Which checks a change needs, which module
owns each one, and which a previous run already proved good is
[`daggerverse/workspace-ci`](../daggerverse/workspace-ci)'s work, and
[`.github/workflows/change-aware-ci.yml`](../.github/workflows/change-aware-ci.yml)
calls that module directly — this one is not in the path. It exists because the
planner always runs the root module's checks and never memoizes them,
which makes it the right home for the three that read the workspace as a
whole rather than any one module's closure:

| check | what it proves |
| --- | --- |
| `ci:generated` | every committed `dagger.gen.go` and `internal/dagger/*.gen.go` matches what `dagger develop` produces at the pinned `engineVersion` |
| `ci:generated-self-test` | `ci:generated` can actually fail — it makes one module deliberately stale and demands a red |
| `ci:selection-self-test` | the planner's change → modules → legs mapping still holds against its fixtures |

All three delegate; `ci/main.go` is three one-line calls.

```sh
dagger check                    # run all three
dagger check 'ci:generated'     # run one
```

`ci:generated` names each stale module and prints its patch:

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

## Running checks locally

There are no toolchains, so `dagger check` at the repo root runs the
three above and nothing else. A module's own suite is run at that module:

```sh
dagger -m daggerverse/kafka/tests check      # one module's checks
dagger -m daggerverse/kafka/tests call all   # or call the suite directly
```

To see what CI would run for a branch, ask the planner:

```sh
dagger -m daggerverse/workspace-ci call plan \
  --base="$(git merge-base origin/main HEAD)" --head=HEAD
```

Add `--diagnostics` for why: which modules the change reached, which had
to be loaded, and which legs a recorded pass retired. From a git worktree
`.git` is a file rather than a directory, which the planner cannot read —
pass `--repo` a real clone, or accept that it plans the full suite.

## Adding a new daggerverse module

Add `daggerverse/<m>/` with a sibling `tests/` module and keep `+check`
on `tests.Tests.All()`, the convention every existing module follows.
That is all: nothing here enumerates modules, nothing lists them in
`dagger.json`, and no workflow needs an edit — the planner walks the
workspace for `dagger.json` and asks each module for its own checks.

## Why no toolchains

The root `dagger.json` used to install all ~23 `daggerverse/<m>/tests`
suites as toolchains, because that was how a workspace-wide `dagger check
-l` could enumerate them. Enumeration no longer works that way — the
planner asks each module for its checks (`Module.checks`) — and the
toolchains were retired with it (#290). Three things fall out:

- **`dagger check` at the root no longer runs everything.** That is the
  DX cost, and it is small: a full local run was ~20 minutes of
  containers that nobody performed, while the per-module commands above
  are what the loop actually uses.
- **The 23 `ci/internal/dagger/<m>-tests.gen.go` bindings are gone.**
  Those bindings embed the source *location* of every function in the
  suite they were generated from, so any edit to any tests module — a
  comment, a blank line — left the root module's copy stale and turned
  `ci:generated` red until someone re-ran `dagger develop` at the root
  and committed the churn. That tax was paid on nearly every PR.
- **The run-everything path stops being one enormous leg.** A plan that
  selects everything emits one coarse leg per module, whose `dagger
  check` carries no filter. With toolchains installed, the root module's
  coarse leg would have run every suite in the workspace inside a single
  job.

`workspace-ci` still supports toolchains for workspaces that keep them —
it attributes each `<root-source>/internal/dagger/<toolchain>.gen.go`
back to the toolchain it was generated from (#179), rather than letting a
regenerated binding run the whole suite. This repository just no longer
has any for that rule to fire on.
