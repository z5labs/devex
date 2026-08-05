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

`affected-modules` answers the attribution half on its own — which modules a
change reached — without loading any of them. `record-pass` is the other end of
`hash`: it writes the entry a later run's plan reads back, for the store this
module owns both sides of. See [The store](#the-store).

## Formats

`--format` takes the enum member name, hence the spelling of each below.

| `--format=` | emits |
| --- | --- |
| `JSON` | the canonical form: the indented array above. The default |
| `GITHUB_ACTIONS` | the same array on one line, ready to write to `GITHUB_OUTPUT` and expand with `fromJSON` as a matrix |
| `JENKINS` | Groovy: a map of leg name to closure, ready to hand to a declarative pipeline's `parallel` step |

Other CI systems are follow-ups.

### GitHub Actions

`GITHUB_ACTIONS` is only the data half. The other half — caching the engine
image, fanning the matrix out through `dagger/checks`, recording a pass to the
Actions cache, and exposing one status check branch protection can require — is
published as a reusable workflow, so adopting it is one `uses:`:

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
  actions: read          # lets the planner read the memoization store
jobs:
  ci:
    uses: z5labs/devex/.github/workflows/change-aware-ci.yml@main
    secrets: inherit     # only for DAGGER_CLOUD_TOKEN; omit if you have none
```

Then require **`ci / CI Gate`** in branch protection — GitHub names a called
workflow's job `<caller job name> / <job name>`, so the context follows the
caller's job name and moves only when that is renamed. There is no per-check
YAML: the matrix is the plan verbatim, so a new module, a new check or a renamed
one changes nothing in the consumer's repository.

Everything on the constructor is an input of the same name in kebab-case
(`split-modules`, `timeouts`, `default-timeout`, `memo-refs`, `memo-ttl`), plus
four the workflow owns rather than the planner: `module` (which planner to call,
so a repository developing one can point at its own), `dagger-version`,
`runs-on`, and `max-parallel`. `base` and `head` are derived from the event — a
pull request's base and head, else the push's before/after — and are inputs only
so a trigger the workflow does not recognise can supply them.

Memoization is on by default and needs no configuration: the workflow trusts
exactly the two Actions cache scopes GitHub itself confines — the default branch,
and, on a pull request, that pull request's own scope — and reads them with the
run's own `GITHUB_TOKEN`. Without `actions: read` the store simply cannot be
read, which costs speed and never correctness. `memoize: false` turns off both
the read and the recording.

The workflow uses the `ACTIONS_CACHE` store, so its recording step is
`actions/cache/save` as above. `GIT_REFS` is for consumers whose CI system has no
equivalent — and for GitHub consumers who would rather have one call do both
halves; moving the workflow onto it is tracked separately.

Three things a caller owns rather than the workflow:

- **Permissions.** The workflow declares none, so it inherits the calling job's
  token. This is deliberate: a called workflow that *requests* a permission its
  caller cannot grant fails the run outright, which would make `actions: read` a
  hard adoption barrier rather than an optional speed-up.
- **The status check's name**, as above.
- **Concurrency**, which belongs with the triggers.

### Jenkins

A plan is data in every other format because every other CI system takes data.
Jenkins' `parallel` step takes `Map<String, Closure>` and nothing else, so a JSON
plan has to be turned into closures by a `collectEntries` the consumer writes and
maintains. `JENKINS` emits the closures.

```groovy
stage('run') {
  steps {
    sh "dagger -m github.com/z5labs/devex/daggerverse/workspace-ci call plan --base=${BASE_SHA} --head=${HEAD_SHA} --format=JENKINS > plan.groovy"
    script { parallel load('plan.groovy') }
  }
}
```

```groovy
[
  'daggerverse/pdf/tests:all': {
    stage('daggerverse/pdf/tests:all') {
      timeout(time: 6, unit: 'MINUTES') {
        sh 'dagger -m \'daggerverse/pdf/tests\' check \'tests:all\''
      }
    }
  }
]
```

Three properties worth knowing before you wire it up:

- **A branch allocates no `node`.** The pipeline's own `agent` directive already
  decided where work runs; an emitted `node` would override that choice and need
  a checkout of its own. Branches therefore run in parallel on one agent. To put
  each on its own, wrap the closure — that wrapper is the one thing this format
  does not remove.
- **`jobTimeout` is not rendered**, because a branch has no surrounding job to
  bound. Each branch carries its leg's step budget as a `timeout`.
- **`hash` is not rendered either**, so memoization here means reading
  `--format=JSON` alongside and re-associating the hashes with the branches by
  leg name. `record-pass` now removes the harder half of that — a Jenkins
  pipeline with a `GIT_REFS` store can do the recording with one call and no
  cache action — but the branches still do not carry the hash to record.

An empty plan emits `[:]`, an empty `Map`, which `parallel` accepts as a no-op —
`[]` is an empty `List` and `parallel` would reject it.

## Consuming it

Nothing needs to be installed. A module invoked by git ref keeps the same access
to the calling workspace that a local-path dependency has: `dagger -m
github.com/z5labs/devex/daggerverse/workspace-ci call plan` run from another
repository discovers that repository's modules, diffs its `.git`, and hashes its
sources. Verified against a separate workspace, which produced a plan identical
to the one a local checkout produced.

On GitHub Actions there is nothing to call by hand either — the reusable workflow
above does it. Pin it and the module to the same ref: the workflow's `module`
default is the floating `github.com/z5labs/devex/daggerverse/workspace-ci`, so a
consumer pinning `change-aware-ci.yml@v1.2.3` should pass
`module: github.com/z5labs/devex/daggerverse/workspace-ci@v1.2.3` alongside it,
or the workflow it pinned will plan with whatever the planner has since become.

To also adopt `generated`, `generated-self-test`, `selection-self-test` and
`memo-store-self-test` as checks of your own, install this module as a
**dependency of your root module** and declare them there:

```go
// +check
// +cache="never"
func (m *Root) Generated(ctx context.Context) error {
	return dag.WorkspaceCi().Generated(ctx)
}
```

`dag.CurrentWorkspace()` resolves to the caller's workspace from inside a
dependency, so the check reads your repository, not this one. Repeat
`+cache="never"` on the wrapper: the directive on the function being called does
not propagate to the one calling it.

Declaring them on the **root** module specifically is what makes them work as
intended — a plan always runs the root module's checks and never memoizes them,
which is the premise `generated` rests on. Installing this module as a
*toolchain* instead surfaces those checks to `dagger check`, but not to a plan:
enumeration reads `Module.checks`, which reports a module's own checks and not
its toolchains'. A toolchain check is therefore one no plan ever emits a leg for.

## What the planner will not do

**It will not load a module it does not have to.** Checks are enumerated per
module (`Module.checks`), never through a root module that installs every suite
as a toolchain — enumerating that way costs the load of every toolchain in the
workspace before any planning happens (~5m in this repository, against 12.3s of
actual planning). Only modules a change could reach are loaded, and the
run-everything path emits one leg per module rather than one per check, so it
loads none at all. Fewer, coarser legs there also means fewer simultaneous engine
boots.

### When one coarse leg is too coarse

A coarse leg runs a module's whole suite in one engine on one runner. That is free
when a module's checks share their containers — 13 checks of a Grafana stack land
in ~1m30s — and it is wrong when each check boots a stack of its own. A Kafka
suite whose five checks bring up five different distributions puts **87** brokers,
controllers and registries into a single engine, where a slow start becomes a
failed one.

`--split-modules` is the escape hatch: the modules it names are enumerated even
when everything runs, so their checks get a leg each.

```sh
dagger -m … call --split-modules=daggerverse/kafka/tests plan --base=… --head=…
```

Each named module costs one load, which is the thing this path exists to avoid —
so name the ones that need it, not every module with more than one check. A name
that matches no module in the plan is reported on stderr rather than ignored.

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

There are two, chosen with `--memo-store`. They differ in exactly one way that
matters — whether this module can write them — and that difference propagates
into two different trust arguments, so they are spelled out separately below.

Either one is pointed at with `--memo-token`, `--memo-repo` and `--memo-refs`
(and `--memo-api` for GitHub Enterprise Server). `--memo-refs` is the list of git
refs whose scopes may hold a pass, spelled in full, and it defaults to none: a
scope a run can write is one that has to be chosen deliberately. You can also
skip the store entirely and pass hashes you read yourself as
`--known-good='["…"]'`.

#### `ACTIONS_CACHE` — read here, written by the workflow

The default, and what `change-aware-ci.yml` uses. An entry is a GitHub Actions
cache key, `workspace-ci-memo-v1-<hash>`, and nothing else; nothing ever reads
the payload back. Writing one needs `ACTIONS_RUNTIME_TOKEN`, which only a running
workflow has, so recording stays a CI-native step:

```yaml
- uses: actions/cache/save@v4
  if: ${{ success() && matrix.job.hash != '' }}
  with:
    path: .ci-memo
    key: workspace-ci-memo-v1-${{ matrix.job.hash }}
```

`record-pass` reports `UNSUPPORTED` against this store rather than pretending.

That read/write asymmetry is also what bounds the trust, and **GitHub enforces
it**: a run can only write cache entries under its own `GITHUB_REF`, so
nominating the default branch and a PR's own scope admits no other branch and no
fork — stricter than GitHub's own restore rules, which would also expose a
stacked PR's base branch.

#### `GIT_REFS` — read and written here

A store this module owns both sides of, so `record-pass` works and a CI system
with no equivalent of `actions/cache/save` can memoize at all. An entry is a git
ref in `--memo-repo` and, again, nothing else:

```
refs/workspace-ci-memo/v1/<scope>/<input-hash>/<unix-seconds>
```

`<scope>` is the hex encoding of the recording run's ref — hex because a raw ref
name contains slashes, which would make `refs/heads/main` a path prefix of
`refs/heads/main/feature` and let a branch anyone can create read and poison the
default branch's scope. Decode one with `printf %s <scope> | xxd -r -p`. The
timestamp is in the name because GitHub's ref listing carries no date, and a TTL
read on every plan must not cost an API call per entry. The ref points at the
commit that passed, so `git log` on it shows what proved the hash.

Recording is one call per green leg:

```
dagger -m <planner> call \
  --memo-store=GIT_REFS --memo-token=env:GH_TOKEN \
  --memo-repo="$REPO" --memo-refs="$REF" \
  record-pass --hash="$HASH" --ref="$REF" --commit="$SHA"
```

It prints one word — `RECORDED`, `ALREADY_RECORDED`, `REFUSED`, `SKIPPED`,
`UNSUPPORTED` or `FAILED` — and **never fails**. Recording happens after the work
is already green, so a store outage has to cost a later run its time and nothing
else; a caller who wants it loud compares the word. Recording a hash the store
already holds writes nothing and does not refresh its timestamp, because doing so
would extend a TTL whose whole job is to bound how long a pass may be honoured.
An empty hash — a leg the planner could not hash — records nothing. The one hard
error is a call that names no ref, because with no ref there is no scope to judge
and a silent refusal would be indistinguishable from one that was judged.

#### The trust argument for module-side writes

This is the part that is **not** inherited from the Actions cache, and it is
stated here rather than left implicit because the difference is easy to miss:

- **GitHub isolates cache scopes. It does not isolate refs.** A token that can
  create a ref can create any ref, including one under another branch's scope. So
  the module refusing to write from a ref outside `--memo-refs` is a guard
  against *accident*, not against a malicious writer, and it is the reason
  `record-pass` returns `REFUSED` rather than writing quietly.
- **The enforcement is the credential.** On GitHub Actions a fork pull request's
  `GITHUB_TOKEN` is read-only, so a fork cannot record at all — the same boundary
  the Actions cache gives, arrived at differently. Grant `contents: write` only
  to the runs whose scopes you nominated, and protect
  `refs/workspace-ci-memo/*` with a repository ruleset if you want the boundary
  enforced server-side rather than by which jobs hold which token.
- **Everything downstream is unchanged.** A ref-store entry is honoured on
  exactly the same terms as a cache entry: the same TTL, the same per-scope
  reads, and the same wholesale refusal below.

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
`--memo-ttl`, 24h by default, applied against each entry's write time — an
Actions cache entry's `created_at`, or the timestamp in a ref entry's own name.

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

`memo-store-self-test` does the same for the store this module writes: that a
pass lands under its own ref's scope, that recording it twice writes nothing and
refreshes no TTL, that entries read back, that one past the TTL does not, and
that one ref's scope stays out of another's. It drives the real HTTP client
against an in-process stub of GitHub's API, so it too needs no network, no
credential and no services. It is a check and not only a Go test because this is
the half of memoization that fails silently — a store that quietly takes nothing
looks exactly like one nobody has recorded into, and a scope that leaks costs
correctness rather than time.

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

**No end-to-end test against a real store.** The rules that matter on the read
side — a known-good hash drops its leg, a global input retires every recorded
pass, an unhashable leg is never dropped — are tested through `--known-good`,
which is the same code path. What each store itself contributes is prefix
matching, TTL filtering, ref scoping and, for `GIT_REFS`, recording: those are
covered against an in-process stub, in `memostore`'s Go test and in
`memo-store-self-test`, because reaching the real APIs from a test would mean a
live GitHub token, a live cache and refs written into a real repository. The
`record-pass` tests in `tests/` therefore assert the decisions the module makes
before the store is reached — which store, which ref, which hash — and prove a
trusted ref really does go on to the store by watching it fail against one that
cannot answer.
