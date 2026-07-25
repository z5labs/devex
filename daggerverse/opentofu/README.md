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

`Destroy` with neither state nor a backend is rejected too: `tofu` would
happily report "0 destroyed" and exit 0, which reads as a successful teardown
while the real infrastructure — whose state was never supplied — stays up.

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
lock file refreshed with `tofu providers lock`.

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
| `Config.WithBackendConfig(name, value)` | `-backend-config=name=value`. |
| `Config.WithBackendConfigFile(file)` | `-backend-config=<file>`. |
| `Config.WithWorkspace(name)` | `tofu workspace select -or-create`. |
| `Config.WithoutPluginCache()` | Disable the shared provider cache. |
| `Config.WithState(state)` | File-carried state. |
| `Config.Fmt()` | `tofu fmt -check -diff -recursive`; the diff, failing on drift. |
| `Config.Validate()` | `tofu validate`, after an `init -backend=false`. |
| `Config.Init()` | `tofu init`; returns the root module plus `.terraform.lock.hcl`. |
| `Config.Plan(destroy, targets)` | A directory of `plan.tfplan`, `plan.json`, `plan.txt`, `changes`. |
| `Config.Apply(plan, targets)` | A directory of `terraform.tfstate`, `outputs.json`, `apply.log`. |
| `Config.Destroy(targets)` | A directory of `terraform.tfstate`, `outputs.json`, `destroy.log`. |
| `Config.Outputs()` | `tofu output -json`. |
| `Config.Show()` | `tofu show`. |
| `Config.Ci()` | A chained CI pipeline builder over this configuration. |
| `Ci.WithFmt()` | Enable the `fmt` stage. |
| `Ci.WithValidate()` | Enable the `validate` stage. |
| `Ci.WithPlan(failOnChanges)` | Enable the `plan` stage; `failOnChanges` makes it a drift gate. |
| `Ci.Check()` | Run the enabled stages in parallel; aggregated error. |
| `Ci.Run()` | The same stages, returning the plan artifacts. |

### What each stage emits

`Fmt` returns the diff and fails on drift, so it is usable as a CI gate;
rewriting in place is a follow-up. Because Dagger drops a function's value
whenever its error is non-nil, a failing run carries the diff in the *error*.

`Validate` initialises with `-backend=false` first, so a configuration
declaring a remote backend validates with no credentials and without touching
the backend at all.

`Init` strips `.terraform/` from its result: with the shared provider cache in
play those entries are symlinks into a cache volume that does not exist outside
the container. The lock file is the portable artifact, and it is what a repo
commits.

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

`Apply` and `Destroy` fail hard on a non-zero `tofu` exit, and the error text
carries `tofu`'s own diagnostics. Because Dagger drops a function's value when
its error is non-nil, a partially failed apply forfeits the state it produced
in file-carried mode; the remote-backend path is unaffected, since the backend
already holds the state.

This is a deliberate trade. The alternative — always returning the directory
with an `exit_code` file inside it — turns a failed apply into a silent green
whenever a caller forgets to inspect it.

## Caching

Every operation that touches real infrastructure carries `+cache="never"`, and
the directive repeats on each of them individually rather than being assumed to
propagate from `Config`.

That directive governs the *function* result; the `WithExec` layers underneath
are still content-addressed, so `Plan`, `Apply`, `Destroy`, `Outputs` and
`Show` each stamp a per-call random nonce onto the run that must genuinely
re-execute.

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
