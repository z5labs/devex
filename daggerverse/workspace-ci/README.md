# workspace-ci

Change-aware, memoized CI planning for a workspace of Dagger modules.

Point it at a commit range and it returns the checks to run — already routed at
the module that owns each one, with timeouts and memoization hashes applied — so
a CI system needs one call and at most a format shim.

```sh
# from anywhere in your workspace
dagger -m github.com/z5labs/devex/daggerverse/workspace-ci call plan \
  --base="$BASE_SHA" --head="$HEAD_SHA"
```

```json
[
  {
    "name": "daggerverse/pdf/tests:all",
    "module": "daggerverse/pdf/tests",
    "filter": "tests:all",
    "hash": "9f0c…",
    "timeout": 6,
    "jobTimeout": 10
  }
]
```

| field | meaning |
| --- | --- |
| `name` | the leg's display name, unique across the plan |
| `module` | the repo-relative module to invoke with `-m`, so a leg loads only what it runs |
| `filter` | the pattern to pass to `dagger check`; **empty** means run every check the module has |
| `hash` | the input hash a pass may be recorded under; **empty** means never memoize this leg |
| `timeout` | the check step's budget, in minutes |
| `jobTimeout` | the surrounding job's budget: `timeout` plus setup headroom, computed here because most CI expression languages have no arithmetic |

`--format=GITHUB_ACTIONS` emits the same array on one line, ready to write to
`GITHUB_OUTPUT` and expand with `fromJSON` as a matrix. (The CLI takes the enum
member name, hence the spelling.) Other CI systems are follow-ups.

`affected-modules` answers the attribution half on its own — which modules a
change reached — without loading any of them.

## Consuming it

Nothing needs to be installed. A module invoked by git ref keeps the same access
to the calling workspace that a local-path dependency has: `dagger -m
github.com/z5labs/devex/daggerverse/workspace-ci call plan` run from another
repository discovers that repository's modules, diffs its `.git`, and hashes its
sources. Verified against a separate workspace, which produced a plan identical
to the one a local checkout produced.

Installing it as a dependency or a toolchain also works, and a toolchain gets you
`workspace-ci:generated`, `workspace-ci:generated-self-test` and
`workspace-ci:selection-self-test` as checks of your own.

## What the planner will not do

**It will not load a module it does not have to.** Checks are enumerated per
module (`Module.checks`), never through a root module that installs every suite
as a toolchain — enumerating that way costs the load of every toolchain in the
workspace before any planning happens (~5m in this repository, against 12.3s of
actual planning). Only modules a change could reach are loaded, and the
run-everything path emits one leg per module rather than one per check, so it
loads none at all. Fewer, coarser legs there also means fewer simultaneous engine
boots.

**It will not return an empty plan.** A workspace it cannot read is an error: an
empty matrix skips the run job and passes the gate having run nothing. Everything
else fails safe towards running too much.

| what went wrong | what happens |
| --- | --- |
| no `dagger.json` anywhere | **error** |
| the diff range is unusable (new branch, all-zeros base) | everything runs |
| `.git` cannot be read | everything runs, nothing is memoized |
| a module's dependencies cannot be read | that module always runs |
| a module's source context cannot be read | everything under it counts as an input |
| a module's checks cannot be enumerated | one leg runs all of them |
| the memo store cannot be read | those legs just run |

## What counts as a change

The planner asks whether a changed path is an **input** to a module, not merely
whether it sits inside one. A module's inputs are its Dagger *source context* —
precisely the files Dagger uploads for it, read from the engine via
`ModuleSource.ContextDirectory`, never modelled in Go. A file outside that
context is shipped to no engine, so it cannot affect any check the module feeds.
That is a property Dagger enforces, not one this module asserts about which files
look like prose.

Narrow a module's inputs with a negated `include` pattern in its `dagger.json`
(`include` unions on top of the source directory, so only `!` subtracts):

```json
{ "include": ["!README.md"] }
```

The consequence to respect: **a module must not read a file it declared out.**
Doing so fails loudly at build time rather than silently under-running CI.

Attribution then falls out in four cases:

| changed path | selects |
| --- | --- |
| in a module's source context | that module and its dependents |
| in the root module's source context | every module |
| under a `--global-paths` prefix (default `.github/workflows/`) | every module |
| in no module's source context | nothing beyond the root module's own checks |

Two deliberate asymmetries. The **innermost** module owns a path, so
`daggerverse/crypto/tests/main.go` belongs to `crypto/tests` and not to `crypto`,
even though Dagger's context for `crypto` contains it — nested modules are
separate Go modules that never compile into their parent. And a **deleted** path
is attributed to its module rather than read as declared out, because no head
source context can contain it; that over-runs when a declared-out file is
deleted, which is the direction this module errs in.

The root module's checks always run and are never memoized. They are the ones
that answer questions about the workspace as a whole, and every global input
belongs to them.

### The aggregator-binding exception

If your root module installs toolchains, Dagger generates one binding per
toolchain under the root module's source
(`<root-source>/internal/dagger/<toolchain>.gen.go`). Repo convention regenerates
one on nearly every module change, and left alone each would land in the root
module's context and run everything. Each is instead attributed to the toolchain
it was generated from. That mapping is the only place dagger's kebab-casing rule
lives — it splits letter↔digit boundaries too, so toolchain `z5labs-tests` owns
`z-5-labs-tests.gen.go`.

## Skipping work a previous run already proved good

Selection is a diff against one base, so on its own it can only skip work
unaffected by *this* change — never work an earlier run already proved good. A
PR's second and later pushes re-run everything that was green on the first.
Memoization closes that gap: every leg gets an **input hash**, a passing leg
records it, and a later run that computes the same hash skips the leg.

The hash is a Merkle fold over **git blob object ids** — sorted `(path, blobOID)`
per module source context, folded over the leg's transitive dependency closure,
plus the leg's own name and the global inputs. Git object ids, not
`Directory.digest`: Dagger's digest format is explicitly not stable across
releases, so a persisted set keyed on it would be invalidated wholesale by every
engine bump. An engine bump still invalidates every leg, correctly —
`engineVersion` is a field in every module's `dagger.json`, and each one is in its
own module's source context.

The **global inputs** are the source contexts of the root module's whole
*dependency closure*, plus everything under `--global-paths`. The closure and not
the root module's own context, because the machinery that selects, routes and
records checks is free to live in a dependency — and once this module is that
dependency, hashing only the root's own sources would leave the planner outside
its own trust boundary.

### The store

Reading is all this module does. A write needs `ACTIONS_RUNTIME_TOKEN`, which
only a running workflow has, so recording a pass stays a CI-native step:

```yaml
- uses: actions/cache/save@v4
  if: ${{ success() && matrix.job.hash != '' }}
  with:
    path: .ci-memo
    key: workspace-ci-memo-v1-${{ matrix.job.hash }}
```

The entry's whole meaning is its key; nothing ever reads the payload back. Point
the planner at the store with `--memo-token`, `--memo-repo` and `--memo-refs`, or
pass hashes you read yourself as `--known-good='["…"]'`.

That read/write asymmetry is also what bounds the trust. A run can only write
cache entries under its own `GITHUB_REF`, so nominating the default branch and a
PR's own scope admits no other branch and no fork — stricter than GitHub's own
restore rules, which would also expose a stacked PR's base branch.

Within a PR's own scope the writer is that PR, so the planner refuses to honour
*any* recorded pass once a **global input** changed. Those are the only paths
that can alter how a pass is recorded, and a change to one also perturbs every
input hash, so a forged entry lands in a hash space no untampered tree ever
computes, and reverting the tamper restores the honest hashes rather than
matching the forged ones.

### The two root-context paths that are not global

Adding a module to a workspace is the change that touches least and, left alone,
would cost most: it necessarily edits files under the root module, and if all of
them were global inputs it would retire the entire store. Two are excluded from
both the global hash and the trust judgement, and the aggregator-binding rule
above already handles the third:

| file the change touches | treatment |
| --- | --- |
| the root `dagger.json` | not a global input |
| `<root-source>/internal/dagger/dagger.gen.go` (regenerated with it) | not a global input |
| `<root-source>/internal/dagger/<new>.gen.go` | attributed to the new toolchain |

**The root `dagger.json`** decides which checks *exist*, never what an existing
check *computes*. Its `engineVersion` is carried by every other module's
`dagger.json` too, each in its own source context. Its `toolchains` only decide
what a workspace-wide enumeration finds, and this planner enumerates per module.
Its `sdk`, `source`, `include` and `codegen` scope the root module, which is
never memoized — and `include`/`source` decide which paths are in the root
context at all, so hiding a file with them removes it from the global set and
moves the digest anyway.

**The core binding** is the sharper case, because it is the API surface the
hasher itself executes: a hand-patched `Glob` could make some module's hash go
blind to that module's sources, recording a pass on good content and matching it
against bad. What forecloses that is not the digest but the `generated` check —
it proves every committed generated file equals what `dagger develop` produces,
it belongs to the root module so it always runs, and it is never memoized. A
tampered binding is red at the gate on the very push that would act on it, and
reverting to go green restores the honest hash, which the recorded entry no
longer matches.

Generalised: **a generated file need not be a global input, because a
never-memoized check already proves it is derived from inputs that are.**

Both are subtractions of a path's *content*, not of set membership — every other
path in the root closure still reaches every hash. Selection is untouched: either
path changing still runs everything, because the set of checks really did change.
Together they give the intended shape: **adding a module runs the root module's
checks plus the new module's, and retires every untouched leg on its unchanged
hash.**

### Base-image drift

A source-derived hash cannot see upstream drift: checks that boot real services
from floating tags mean an identical tree does not imply an identical container.
The answer is an explicit bound on how long a recorded pass may be honoured —
`--memo-ttl`, 24h by default, applied against each cache entry's `created_at`.

Folding resolved image digests into the hash was rejected: it means resolving
every module's images, which is most of the cost memoization is meant to avoid,
and it breaks the git-object-id property above. Bounding the window instead caps
drift-blindness at a day, which is where essentially all the reuse is anyway —
memoization pays off across hours-scale iteration on a branch.

### What is never memoized

Same rule as selection: never skip a check a change could plausibly affect.

| condition | result |
| --- | --- |
| a root-module leg | always runs — its checks read the whole workspace, which its closure does not describe |
| a module's source context unreadable | it and its dependents are unhashable, so they run |
| a source path absent from `HEAD` (untracked, dirty tree) | its module is unhashable, so its dependents run |
| a global input changed | recorded passes are ignored wholesale |
| the store unreadable, or an entry older than the TTL | the leg runs |

A leg that cannot be hashed reports an empty hash, so a CI system records nothing
for it and it can never accidentally retire a later run.

## Codegen freshness

`generated` fails when a module's committed `dagger.gen.go` or
`internal/dagger/*.gen.go` differ from what `dagger develop` produces at the
pinned `engineVersion`. It covers every `dagger.json` in the calling workspace and
names each stale module, printing its patch:

```
==> daggerverse/kafka/tests is not up-to-date:
<patch>
generated files are not up-to-date; run `dagger develop` in: daggerverse/kafka/tests
```

Dependency bindings embed the source location of every function, so an edit that
only shifts line numbers still leaves every dependent module stale. Re-run
`dagger develop` in the module *and* in each dependent.

`generated-self-test` guards that check: it runs the same comparison against one
module twice, pristine and then deliberately made stale, and fails unless the
stale copy is reported. The check this was extracted from silently verified
nothing for months — it routed through `Workspace.Generators()`, which is empty
unless a module declares a `+generator` function — and the self-test is what makes
that failure mode impossible to repeat silently. It probes the first
dependency-free module in the workspace, or whichever one `--probe-module` names.

`selection-self-test` runs the change → modules → legs mapping, and the properties
a recorded pass depends on, against fixed in-process fixtures. It needs no engine
and no services, so it is cheap enough to run on every leg set.

## Two deliberate omissions

**No `PlanShouldNotBeCached` test**, though `plan` carries `+cache="never"` and
this repository's convention is that every such function gets a two-call test
proving it re-runs. `plan`'s only implicit input is the caller's live workspace,
which a test cannot mutate; any fixture passed as `--repo` changes the argument,
so the second call has a different cache key and the test would pass even with
the directive removed. The directive is still correct — a cached plan describes a
tree the planner never read — but it is unprovable, and a test that proves
nothing is worse than none. `generated-self-test` remains the real proof for the
codegen half.

**No end-to-end test of the memo store read.** The rules that matter — a
known-good hash drops its leg, a global input retires every recorded pass, an
unhashable leg is never dropped — are tested through `--known-good`, which is the
same code path. What the store itself contributes is prefix matching, TTL
filtering and ref scoping, which are unit-tested against a stub in `memostore`,
because reaching the real Actions cache API from a test would mean a live GitHub
token and a live cache.
