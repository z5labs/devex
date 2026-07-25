# opentofu

A Dagger module wrapping the [`tofu`](https://opentofu.org/) CLI, so an
infrastructure repo's format, validate, plan, apply and destroy steps become
`dagger call`s instead of a wrapper script plus a pile of exported environment
variables.

It targets [OpenTofu](https://opentofu.org/) (MPL-2.0) rather than Terraform
(BUSL since 1.6), and deliberately does not try to also drive a `terraform`
binary.

## The assembled container

OpenTofu stopped supporting direct use of its official image as of 1.10. What
upstream still publishes is `ghcr.io/opentofu/opentofu:<version>-minimal`, an
image containing only `/usr/local/bin/tofu`. This module therefore assembles
its own container: the binary is copied off the `-minimal` image onto a small
Alpine base that also carries `git` (module sources) and CA certificates (the
provider registry), neither of which `-minimal` has.

```go
dag.Opentofu()                                                // ghcr.io/opentofu/opentofu:1.12.5-minimal on alpine:3.22
dag.Opentofu(dagger.OpentofuOpts{Version: "1.11.0"})          // pin a release
dag.Opentofu(dagger.OpentofuOpts{Version: "1.11.0-minimal"})  // same image, suffix spelled out
dag.Opentofu(dagger.OpentofuOpts{Base: "alpine:3.21"})        // different base
```

The `-minimal` suffix is appended when absent, so both spellings of a version
resolve identically. `base` must be Alpine-family — the module runs
`apk add git ca-certificates` on it.

`Container()` is the escape hatch for every subcommand this module does not
wrap: `dagger call container with-exec --args=tofu,console` keeps the long
tail reachable.

## Config, not file

`Config` takes a `*Directory`, not a lone `*File`: `tofu` resolves
sub-modules, `.tfvars` files and the dependency lock file relative to the root
module. The tree is *copied* into the container, not mounted, because `tofu`
writes next to the configuration — `.terraform/`, the lock file, the plan
file, and in file-carried mode the state.

The type is named `Config` rather than `Workspace` or `Module`: `Workspace`
collides with both `tofu workspace` (a state concept, exposed separately as
`WithWorkspace`) and Dagger v0.21's core `Workspace` type, and `Module`
collides with Dagger's `Module`.

Variables, credentials and backend settings apply to nearly every subcommand,
so they are hoisted onto `Config` as chained modifiers rather than repeated as
optional parameters across eight lifecycle signatures.

## State

State is the one thing a container cannot keep, so the module makes the two
viable strategies explicit and mutually exclusive.

**File-carried state** — `WithState(file)`, or no state at all for a first
apply. The state is written into the container and every mutating operation
hands the resulting `terraform.tfstate` back out in its output directory.
Fully hermetic, no backend required, and the caller owns persistence.

```sh
dagger -m github.com/z5labs/devex/daggerverse/opentofu call \
  config --source=. \
  with-state --state=terraform.tfstate \
  apply export --path=./out
```

**Remote backend** — the configuration's own `backend` block plus
`WithBackendConfig` / `WithBackendConfigFile` and secret environment
variables. State never leaves the backend, and no state file is emitted.

Calling `WithState` alongside `WithBackendConfig*` is rejected with an error
naming both modes rather than silently picking a winner.

A backend standing up inside the same pipeline — a state server, an
S3-compatible service — is unreachable until it is bound:
`WithServiceBinding(alias, service)` puts it on the tofu container's network
under `alias`. That is the seam the suite's own MinIO fixture uses, and it
works for anything else tofu has to dial (a provider's API, a git server
hosting module sources).

`Destroy` with neither state nor a backend is rejected too: `tofu` would
happily report "0 destroyed" and exit 0, which reads as a successful teardown
while the real infrastructure — whose state was never supplied — stays up.

## State manipulation

The lifecycle functions are what a pipeline runs unattended. Once state and
reality have diverged, an operator reaches for a different set — and reaching
for them through `Container()` means assembling argv, mounting the source and
threading credentials by hand, which is the work this module exists to remove.
So they are wrapped too.

```sh
# What is under management?
dagger -m github.com/z5labs/devex/daggerverse/opentofu call \
  config --source=. with-state --state=terraform.tfstate state-list

# Rename a resource in state to match one renamed in the configuration.
dagger -m github.com/z5labs/devex/daggerverse/opentofu call \
  config --source=. with-state --state=terraform.tfstate \
  state-mv --from=random_pet.old --to=random_pet.new \
  file --path=terraform.tfstate export --path=terraform.tfstate
```

Every one of them writes state, so every one returns a `*Directory` carrying
the resulting `terraform.tfstate` exactly the way `Apply` does: emitted in
file-carried mode, absent under a remote backend, and forfeited entirely on a
non-zero exit.

That last part is the sharp edge. `StateMv`, `StateRm` and `Import` are
destructive against state in ways a failed run cannot undo, and a run that
fails hands back nothing — so hold your own copy of the input state before
starting, the same discipline as `terraform state pull` before a manual edit.

`StateList` answers with an empty listing when there is no state yet. `tofu`
itself refuses a wholly absent state file, but "no state" and "an emptied
state" hold the same answer to what is under management, and a listing that
distinguished them would only make the caller handle a case with no content.

`StateShow` returns one JSON document, not the JSON *stream* `tofu state show
-json` writes — that stream opens with a UI message naming the version it ran,
which no caller asked for. Note that `-json` prints sensitive values in full
regardless of how the variable behind one was declared, so treat the result as
sensitive whenever the resource is.

`Graph` is the one read-only member, and the one derived from the
configuration rather than from live state, so it is the one that caches per
session rather than never.

## Secrets

`WithSecretVar(name, secret)` binds `TF_VAR_<name>` as a container environment
secret rather than passing `-var name=value`, so the plaintext never enters
argv, the CLI log, or a saved plan's command line. Provider credentials go
through `WithSecretVariable` (`AWS_ACCESS_KEY_ID`, `TF_TOKEN_app_terraform_io`,
…) and are likewise `*Secret`, never strings.

Note what this does *not* cover: `tofu show -json` is not redacted, so a
sensitive value that reaches a resource attribute lands in `plan.json` and in
the state regardless of how it was supplied. Declare such variables
`sensitive = true` and treat the emitted state as sensitive.

## Provider cache

Provider downloads are cached in a shared `CacheVolume` mounted at
`TF_PLUGIN_CACHE_DIR`, with `LOCKED` sharing — the tofu provider cache is not
concurrency-safe, and a parallel suite would otherwise have several `tofu init`
runs unpacking into it at once.

It is on by default and opted out of with `WithoutPluginCache()`. That is a
`Without*` modifier rather than a `pluginCache bool` defaulting to `true`,
because a `+default=true` bool cannot be set back to `false` from the Go SDK:
the zero value is dropped before it reaches the engine.

One consequence: providers installed from the cache are recorded in
`.terraform.lock.hcl` with `h1:` hashes only. A repo whose committed lock file
carries just the registry `zh:` hashes needs either `WithoutPluginCache()` or a
lock file refreshed with `Lock()`, which bypasses the cache for exactly this
reason.

## Function surface

| Name | Purpose |
|---|---|
| `Container()` | The assembled tofu image — the escape hatch for unwrapped subcommands. |
| `Version()` | The OpenTofu release the container ships. |
| `Config(source)` | Bind a root module tree to the toolchain. |
| `Config.WithVar(name, value)` | `-var name=value`. A pair, not a map, because Dagger functions cannot accept map params. |
| `Config.WithSecretVar(name, secret)` | `TF_VAR_<name>` as an environment secret — never argv. |
| `Config.WithVarFile(file)` | `-var-file`, staged outside the root module. |
| `Config.WithEnvVariable(name, value)` | A plain environment variable on every exec. |
| `Config.WithSecretVariable(name, secret)` | A secret environment variable — how provider credentials arrive. |
| `Config.WithServiceBinding(alias, service)` | Make a Dagger service reachable from every tofu exec under `alias`. |
| `Config.WithBackendConfig(name, value)` | `-backend-config=name=value`. |
| `Config.WithBackendConfigFile(file)` | `-backend-config=<file>`. |
| `Config.WithWorkspace(name)` | `tofu workspace select -or-create`. |
| `Config.WithoutPluginCache()` | Disable the shared provider cache. |
| `Config.WithState(state)` | File-carried state. |
| `Config.Fmt()` | `tofu fmt -check -diff -recursive`; the diff, failing on drift. |
| `Config.Format()` | `tofu fmt -recursive`; the rewritten tree, for the caller to export. |
| `Config.Validate()` | `tofu validate`, after an `init -backend=false`. |
| `Config.Init()` | `tofu init`; returns the root module plus `.terraform.lock.hcl`. |
| `Config.Lock(platforms)` | `tofu providers lock`; a `.terraform.lock.hcl` covering every named platform. |
| `Config.Plan(destroy, targets)` | A directory of `plan.tfplan`, `plan.json`, `plan.txt`, `changes`. |
| `Config.Apply(plan, targets)` | A directory of `terraform.tfstate`, `outputs.json`, `apply.log`. |
| `Config.Destroy(targets)` | A directory of `terraform.tfstate`, `outputs.json`, `destroy.log`. |
| `Config.Outputs()` | `tofu output -json`. |
| `Config.Show()` | `tofu show`. |
| `Config.StateList()` | `tofu state list`; the addresses under management, one per line. |
| `Config.StateShow(address)` | `tofu state show -json`; one resource's state as a JSON document. |
| `Config.StateMv(from, to)` | `tofu state mv`; the resulting state directory. |
| `Config.StateRm(addresses)` | `tofu state rm`; the resulting state directory. |
| `Config.Import(address, id)` | `tofu import`; the resulting state directory. |
| `Config.Refresh()` | `tofu apply -refresh-only -auto-approve`; the resulting state directory. |
| `Config.Taint(address)` | `tofu taint`; the resulting state directory. |
| `Config.Untaint(address)` | `tofu untaint`; the resulting state directory. |
| `Config.Graph()` | `tofu graph`; the dependency graph in DOT. |
| `Config.Ci()` | A chained CI pipeline builder over this configuration. |
| `Ci.WithFmt()` | Enable the `fmt` stage. |
| `Ci.WithValidate()` | Enable the `validate` stage. |
| `Ci.WithPlan(failOnChanges)` | Enable the `plan` stage; `failOnChanges` makes it a drift gate. |
| `Ci.Check()` | Run the enabled stages in parallel; aggregated error. |
| `Ci.Run()` | The same stages, returning the plan artifacts. |

### What each stage emits

`Fmt` returns the diff and fails on drift, so it is usable as a CI gate.
Because Dagger drops a function's value whenever its error is non-nil, a
failing run carries the diff in the *error*.

`Format` is its rewrite counterpart: it returns the corrected tree rather than
a verdict, leaving the caller's own directory untouched until they export it.

```sh
dagger -m github.com/z5labs/devex/daggerverse/opentofu call \
  config --source=. format export --path=.
```

`Validate` initialises with `-backend=false` first, so a configuration
declaring a remote backend validates with no credentials and without touching
the backend at all.

`Init` strips `.terraform/` from its result: with the shared provider cache in
play those entries are symlinks into a cache volume that does not exist outside
the container. The lock file is the portable artifact, and it is what a repo
commits.

`Lock` regenerates that same lock file for platforms other than the one it runs
on. An ordinary `init` records hashes for its own platform, so a lock file
produced by a `linux_amd64` CI job fails `tofu init` on a developer's
`darwin_arm64` machine; naming every platform a repo builds on puts all of
their hashes in one file. It runs with the provider cache disabled — the cache
holds packages for this platform only, and locking is about what the registry
publishes for the rest.

`tofu providers lock` also dies sporadically inside a container exec, with the
Go runtime's `fatal error: found pointer to free object` from a corrupted heap
rather than any diagnostic of its own — an upstream crash in tofu's provider
installer, close kin to [moby/buildkit#6445][buildkit-6445]. It is not
reproducible on demand, so `Lock` re-runs a crashed attempt up to three times
(cache-busted, or the failed exec would be replayed from the layer cache) and
reports the crash only if every attempt dies. Every other non-zero exit —
including a platform the provider does not publish — fails on the first run.

[buildkit-6445]: https://github.com/moby/buildkit/issues/6445

```sh
dagger -m github.com/z5labs/devex/daggerverse/opentofu call \
  config --source=. \
  lock --platforms=linux_amd64,darwin_arm64,windows_amd64 \
  file --path=.terraform.lock.hcl export --path=.terraform.lock.hcl
```

`Plan` performs a single `tofu` run and derives `plan.json` and `plan.txt`
from the saved plan file — one run, because under `+cache="never"` a second
`tofu plan` to obtain the JSON form could legitimately disagree with the
first. `changes` is `none` or `changes`, from `-detailed-exitcode`.

Everything stateful returns a `*Directory` rather than a module object: Dagger
v0.21 detaches module objects returned from `+cache="never"` functions when a
consumer reads their fields lazily, and a `*Directory` is a core type that
crosses the boundary intact.

## The Ci pipeline

`Ci` bundles the check stages a repo runs on every change into one call, so CI
is `dagger call config --source=. ci with-fmt with-validate check` rather than
a hand-wired sequence of `dagger call`s.

```go
// A pull-request gate: format, validity, and a plan that has to succeed.
dag.Opentofu().Config(source).Ci().WithFmt().WithValidate().WithPlan().Check(ctx)

// A drift detector: a non-empty plan against live infrastructure fails.
dag.Opentofu().
    Config(source).
    WithSecretVariable("AWS_ACCESS_KEY_ID", key).
    Ci().
    WithValidate().
    WithPlan(dagger.OpentofuCiWithPlanOpts{FailOnChanges: true}).
    Check(ctx)
```

The builder hangs off `Config` rather than off the root type — a divergence
from `Zig.Ci(source)` and `Kicad.Ci(source)`. Every stage beyond `fmt` needs
the variables, credentials and backend settings already bound to a `Config`,
and re-declaring them on `Ci` would duplicate nine modifiers.

The enabled stages run in parallel and their errors are *aggregated*: a
configuration that is unformatted and invalid reports both in one round trip,
rather than hiding the validation error behind the formatting one. Stages are
opt-in, so a `Check` reports on exactly what was asked for — and a pipeline
with no stages enabled is an error rather than a pass, because a check that
inspects nothing and returns green is the purest false green there is.

`Run` performs the same stages and returns the plan artifacts — `plan.tfplan`,
`plan.json`, `plan.txt`, `changes` — for whatever consumes them downstream: a
review gate that renders the plan, an `Apply` that takes the saved plan, an
artifact attached to a pull request. It plans whether or not `WithPlan` was
called, since it has to produce the directory it returns; when `WithPlan` did
enable the stage, that single plan run is both the check and the artifact.

## Failure is an error, not a file

Every function that writes state — `Apply`, `Destroy`, and the state-surgery
set above — fails hard on a non-zero `tofu` exit, and the error text carries
`tofu`'s own diagnostics. Because Dagger drops a function's value when its
error is non-nil, a partially failed apply forfeits the state it produced in
file-carried mode; the remote-backend path is unaffected, since the backend
already holds the state.

This is a deliberate trade. The alternative — always returning the directory
with an `exit_code` file inside it — turns a failed apply into a silent green
whenever a caller forgets to inspect it.

## Caching

Every operation that touches real infrastructure carries `+cache="never"`, and
the directive repeats on each of them individually rather than being assumed to
propagate from `Config`.

That directive governs the *function* result; the `WithExec` layers underneath
are still content-addressed, so each of those functions stamps a per-call
random nonce onto the run that must genuinely re-execute. The two that read
configuration rather than infrastructure — `Fmt`/`Format` and `Graph` — carry
`+cache="session"` and no nonce, so they are free to come back from the layer
cache.

The same fact bites callers: two selections off one `+cache="never"` call are
two invocations. Reading `plan.json` and `changes` off a single `Plan()` would
be two different plans unless the directory is pinned first — resolve it to an
ID and reload it, as `tests/main.go`'s `pin` helper does.

## Tests

```sh
dagger -m daggerverse/opentofu/tests call all
```

Fixtures under `tests/fixtures/` use `hashicorp/random` and `hashicorp/local`
only, so nothing needs a cloud credential. The random provider's resources
exist purely in state, which is what makes the state round-trip assertions
meaningful — there is no out-of-band object to drift away underneath them.

The remote-backend half of the suite is hermetic too. `tests/backend.go`
stands up a MinIO service per test, mints its root credential with
`dag.Random().Sha256()` and crosses it as a `*Secret`, and points the `s3`
backend at it through `WithServiceBinding`. Against that it proves state lives
in the bucket and not in the returned directory, that `WithBackendConfigFile`
selects the same backend as the individual `WithBackendConfig` calls, that
`WithWorkspace` keeps two workspaces' states apart, and that two applies racing
for one state either serialise or fail on the lock — with the losing apply's
work never silently overwritten.

One endpoint detail that fixture has to work around: the `s3` backend's
endpoint override lives under a nested `endpoints` attribute, which
`-backend-config=name=value` cannot express. The fixture sets
`AWS_ENDPOINT_URL_S3` instead, which is also what makes the flag form and the
file form carry an identical list of settings.
