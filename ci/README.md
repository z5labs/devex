# ci

The workspace's root module: the checks that must run for every change,
whatever it touched.

That is the whole of its job. Which checks a change needs, which module
owns each one, and which a previous run already proved good is
[`daggerverse/workspace-ci`](../daggerverse/workspace-ci)'s work, and
[`.github/workflows/change-aware-ci.yml`](../.github/workflows/change-aware-ci.yml)
calls that module directly — this one is not in the path. It exists because the
planner always runs the root module's checks and never memoizes them,
which makes it the right home for the one check that reads the workspace as
a whole rather than any one module's closure:

| check | what it proves |
| --- | --- |
| `ci:generated` | every committed `dagger.gen.go` and `internal/dagger/*.gen.go` matches what codegen produces at the pinned `engineVersion`; that the comparison can fail; and that every module in the workspace was swept |

It delegates; `ci/main.go` is one call, plus the `checkErr` that reads a
dependency's check back as an error (since Dagger v1.0.0-beta.15 a `+check`
reaches its caller as a deferred `*dagger.Check`).

```sh
dagger check                    # run it
dagger check 'ci:generated'     # the same, by name
```

It used to have two siblings, `ci:generated-self-test` and
`ci:selection-self-test`, and both are gone (#446). Between them they cost about
four billed minutes and two engine boots on every pull request, and neither had
failed in two months. `generated` now carries its own proofs, on every run:

- **It can fail.** Beside the sweep it builds a bare module of its own, makes that
  module's bindings stale, and fails unless the drift is reported. The module is
  synthetic so that its cost is fixed and nothing outside workspace-ci's own code
  is an input to it. This is #184's lesson: the check it was extracted from
  verified nothing for months.
- **It looked at everything.** Every committed generated file must belong to a
  module the sweep covered, found by globbing for the files rather than by asking
  the discovery that drove the sweep. A module configured in a shape discovery
  does not recognise turns it red instead of being silently skipped — which is
  exactly what a half-migrated tree did before discovery learned
  `dagger-module.toml`.

The selection self-test went the other way: everything it reads is in the root
module's closure, so a change to it already runs everything. It still runs, as
`workspace-ci:selection-self-test`, both in that full run and in the
planner-tests job below.

`ci:generated` names each stale module and prints its patch:

```
==> daggerverse/kafka/tests is not up-to-date:
<patch>
generated files are not up-to-date; regenerate: daggerverse/kafka/tests
```

Dependency bindings embed the source location of every function
(`// kafka (../../../../../daggerverse/kafka/cluster_kafka.go:401:1)`), so
an edit that only shifts line numbers in `daggerverse/<m>` still leaves
every dependent module stale. Re-run `hack/regen.sh`, which regenerates the
whole tree in dependency order.

## Running checks locally

There are no toolchains, so `dagger check` at the repo root runs
`ci:generated` and nothing else. A module's own suite is run at that module,
named with `--module`:

```sh
dagger -m daggerverse/kafka/tests check --module tests
```

`--module` matters. On Dagger v1.0.0-beta.15, in a workspace still configured by
`dagger.json`, `dagger check -m <dir>` also selects the root module's checks, so
leaving it out runs `ci:generated` alongside the suite you asked for. And
`dagger -m <dir> call all` proves nothing any more: `all` is a `+check`, and
calling one only prints the deferred check (`Check@xxh3:…`) and exits 0 whether
it passes or not. Only `dagger check` runs a check to a verdict.

To see what CI would run for a branch, ask the planner:

```sh
dagger -m daggerverse/workspace-ci call plan \
  --base="$(git merge-base origin/main HEAD)" --head=HEAD
```

Add `--diagnostics` for why: which modules the change reached, which had
to be loaded, and which legs a recorded pass retired. From a git worktree
`.git` is a file rather than a directory, which the planner cannot read —
pass `--repo` a real clone, or accept that it plans the full suite.

## The planner's own tests

The plan decides what CI runs, so it cannot be what decides whether the
planner itself is tested: a change that stopped the planner scheduling its own
tests would be planned by that same changed planner. `ci.yml` therefore has a
job of its own, *Planner tests*, which a plain `git diff` against the base turns
on whenever a CI file changes — `.github/workflows/**`, `.github/scripts/**`,
`ci/**`, the root module's config, `dagger.toml`, `daggerverse/workspace-ci/**`
and `hack/**`. It runs `workspace-ci:selection-self-test`,
`workspace-ci:memo-store-self-test` and the fixture suite in
`daggerverse/workspace-ci/tests`, and `CI Gate` requires it, reading a skip as a
pass. It is a job rather than a separate workflow with a `paths` filter because a
workflow skipped that way leaves its check Pending: it could not be required, and
auto-merge would not wait for it.

## Adding a new daggerverse module

Add `daggerverse/<m>/` with a sibling `tests/` module and keep `+check`
on `tests.Tests.All()`, the convention every existing module follows.
That is all: nothing here enumerates modules, nothing lists them in
`dagger.json`, and no workflow needs an edit — the planner finds every
directory holding a `dagger.json` or a `dagger-module.toml` and asks each
module for its own checks.

## Why no toolchains

The root `dagger.json` used to install all ~23 `daggerverse/<m>/tests`
suites as toolchains, because that was how a workspace-wide `dagger check
-l` could enumerate them. Enumeration no longer works that way — the
planner asks each module for its own checks (`Module.checks` then, workspace
artifacts since Dagger v1.0.0-beta.15) — and the toolchains were retired with
it (#290). Three things fall out:

- **`dagger check` at the root no longer runs everything.** That is the
  DX cost, and it is small: a full local run was ~20 minutes of
  containers that nobody performed, while the per-module commands above
  are what the loop actually uses.
- **The 23 `ci/internal/dagger/<m>-tests.gen.go` bindings are gone.**
  Those bindings embed the source *location* of every function in the
  suite they were generated from, so any edit to any tests module — a
  comment, a blank line — left the root module's copy stale and turned
  `ci:generated` red until someone regenerated the root module
  and committed the churn. That tax was paid on nearly every PR.
- **The run-everything path stops being one enormous leg.** A plan that
  selects everything emits one coarse leg per module, whose `dagger
  check` carries no filter. With toolchains installed, the root module's
  coarse leg would have run every suite in the workspace inside a single
  job.

`workspace-ci` used to keep supporting toolchains for workspaces that
kept them, attributing each `<root-source>/internal/dagger/<toolchain>.gen.go`
back to the toolchain it was generated from (#179). That was dropped in
#446: Dagger v1 moved toolchains into workspaces, refuses to load a module
config that still declares them, and generates no aggregator bindings, so
the rule had nothing left to fire on anywhere.
