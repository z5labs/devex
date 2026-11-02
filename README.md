# devex

z5labs developer-experience monorepo. It bundles two things:

- A collection of **[Dagger](https://dagger.io) modules** under
  [`daggerverse/`](daggerverse/) — reusable, composable building blocks for CI
  pipelines and local development.
- A **Claude Code plugin marketplace** that ships the same AI tooling we use
  in-repo, installable into anyone's Claude Code.

## Daggerverse modules

Install any module into your own Dagger project with:

```sh
dagger install github.com/z5labs/devex/daggerverse/<module>
```

| Module | Description |
| ------ | ----------- |
| [`bruno`](daggerverse/bruno) | Run Bruno API collections — a pass/fail gate or a JUnit report — or generate one from OpenAPI. |
| [`certificate-management`](daggerverse/certificate-management) | Manage X.509 certificate authorities and issue TLS certificates. |
| [`crypto`](daggerverse/crypto) | Common crypto utilities — file digests and ephemeral keys. |
| [`dgraph`](daggerverse/dgraph) | Spin up a Dgraph graph-database cluster. |
| [`envoy`](daggerverse/envoy) | Build Envoy proxy configurations and components. |
| [`flash`](daggerverse/flash) | Codeify firmware flashing as Dagger functions. |
| [`go`](daggerverse/go) | Wrap the Go CLI surface (build, test, vet, fmt, run). |
| [`grafana-stack`](daggerverse/grafana-stack) | Spin up Loki, Tempo, and Mimir as Dagger services. |
| [`java`](daggerverse/java) | Wrap the JVM toolchain — the JDK plus Maven and Gradle. |
| [`kafka`](daggerverse/kafka) | Spin up a Kafka-wire-compatible cluster. |
| [`oci`](daggerverse/oci) | Talk to an OCI registry — push, copy, attach, inspect. |
| [`opentofu`](daggerverse/opentofu) | Drive the OpenTofu (`tofu`) lifecycle — fmt, validate, plan, apply, destroy. |
| [`otel`](daggerverse/otel) | Spin up the OpenTelemetry Collector as a service. |
| [`postgres`](daggerverse/postgres) | Spin up a single-node PostgreSQL 17 primary. |
| [`qemu`](daggerverse/qemu) | Boot guest systems under [QEMU](https://www.qemu.org/). |
| [`random`](daggerverse/random) | Generate random values. |
| [`workspace-ci`](daggerverse/workspace-ci) | Plan change-aware, memoized CI for a workspace of Dagger modules. |
| [`z5labs`](daggerverse/z5labs) | Standardized Go CI and releases: `Go` checks a source tree, `Go.App` builds and `App.Publish` ships a signed, attested multi-arch image. |
| [`zig`](daggerverse/zig) | Wrap the [Zig](https://ziglang.org/) toolchain. |

See [`daggerverse/CLAUDE.md`](daggerverse/CLAUDE.md) for module conventions
(function caching, code generation, tests layout).

### Releasing a Go application

[`z5labs`](daggerverse/z5labs) is the opinionated pipeline the rest of these
modules are wired into. Everything hangs off one entry point:

```sh
# check a source tree — gofmt, go vet, golangci-lint, go test -race
dagger -m github.com/z5labs/devex/daggerverse/z5labs call go --source=. ci

# build a multi-arch app, then ship it
dagger -m github.com/z5labs/devex/daggerverse/z5labs call \
  go --source=. \
  app --version=v1.2.3 --platforms=linux/amd64,linux/arm64 \
  with-registry --address=ghcr.io --username="$GITHUB_ACTOR" --auth=env:GITHUB_TOKEN \
  with-oidc --request-url="$ACTIONS_ID_TOKEN_REQUEST_URL" --request-token=env:ACTIONS_ID_TOKEN_REQUEST_TOKEN \
  publish --repositories=z5labs/hello
```

`publish` returns one digest-pinned reference per published tag, and every
published digest carries an SPDX and a CycloneDX document per platform plus a
signed SLSA provenance statement — a publish that cannot produce provenance
fails rather than shipping without it. `app` also exposes `container` and
`containers`, which return the very images `publish` pushes, so a check can run
against the artifact that ships rather than against a rebuild of it.

The version is yours and is validated as an image tag; the commit comes from
HEAD and from nothing else, so two builds of one `(commit, version)` pair are
byte-identical. Every image carries one standardized `PATH` with
`/usr/local/bin` on it for an extension's executables — see the module's package
doc, which records why both values are fixed.

## Claude Code plugin marketplace

This repo is also a [Claude Code plugin
marketplace](https://code.claude.com/docs/en/plugin-marketplaces) named
`z5labs-devex`. Add it to your Claude Code:

```
/plugin marketplace add z5labs/devex
```

Then install a plugin:

```
/plugin install daggerverse@z5labs-devex
```

| Plugin | Provides |
| ------ | -------- |
| [`backlog`](plugins/backlog) | `/backlog:run-backlog` — works a repository's story backlog unattended, one issue at a time, from selection through pull request to a label-driven auto merge. |
| [`daggerverse`](plugins/daggerverse) | `/plan-dagger-module` — paces a design conversation and drafts story issues for a new daggerverse module. |
| [`hosted-agents`](plugins/hosted-agents) | `/hosted-agents:review-plugin` — reports which of a plugin's components will work, need setup or break in Claude Code on the web, a Copilot cloud sandbox and a Copilot Actions session, with file:line, remedy and where the fix belongs. |

See [`plugins/README.md`](plugins/README.md) for the plugin layout. To develop
against an unmerged local checkout, run `/plugin marketplace add .` from the
repo root instead.

### In cloud sessions

A [cloud session](https://code.claude.com/docs/en/claude-code-on-the-web) ignores
the plugins and marketplaces declared in `.claude/settings.json`, because adding
them needs the workspace trust dialog and a cloud session never shows one.
[`.claude/hooks/install-plugins.sh`](.claude/hooks/install-plugins.sh) declares
them in the session's user settings instead, with the marketplace pointing at
the checkout, so the plugins load from the branch the session is on. It is a
byte-identical copy of the `backlog` plugin's
[`assets/install-plugins.sh`](plugins/backlog/assets/install-plugins.sh), which
other repositories get from `backlog:setup-backlog`;
[`.claude/hooks/install-plugins_test.sh`](.claude/hooks/install-plugins_test.sh)
fails if the two drift apart.

Claude Code loads plugins when it starts, so where the script runs decides
when they appear:

- **The environment's [setup script](https://code.claude.com/docs/en/cloud-environments#setup-scripts)**,
  which runs before Claude Code starts. The plugins are there from the first
  prompt. Add this line to it:

  ```
  bash /home/user/devex/.claude/hooks/install-plugins.sh /home/user/devex || true
  ```

  Setup scripts belong to the environment, not the repository, so if the
  environment also serves other repositories, the line still names this one.
- **The `SessionStart` hook in `.claude/settings.json`**, which runs it in every
  cloud session without any environment setup. Claude Code has started by then,
  so run `/reload-plugins` once to load them, or start the next session.

#### Running the backlog on another repository

The same script delivers the `backlog` plugin to a cloud session on any
repository, not only this one. In a repository that declares the `z5labs-devex`
marketplace rather than being it, the script copies the declared `github`
source into user settings. When it runs from the environment's setup script, it
also registers the marketplace and installs the plugin with `claude plugin`
before Claude Code starts. Settings alone are not enough for a marketplace
fetched from GitHub: on a fresh configuration, Claude Code 2.1.316 registered
it on the first start and installed its plugins only on the next, and a cloud
container is fresh every session.

1. **Prepare the repository.** Run `/backlog:setup-backlog` in it, from a local
   checkout with the plugin installed (`/plugin marketplace add z5labs/devex`,
   then `/plugin install backlog@z5labs-devex`). Its "Cloud sessions" step copies
   the script to `.claude/hooks/install-plugins.sh`. It merges three entries
   into `.claude/settings.json`: the marketplace, `backlog@z5labs-devex`, and a
   `SessionStart` hook that runs the script. It commits nothing, so commit and
   push both files.
2. **Set up the cloud environment.** The report from step 1 ends with the
   environment's setup script, written out in full. Paste it into the
   environment's [setup script](https://code.claude.com/docs/en/cloud-environments#setup-scripts).
   It does three things, in this order:
   - It writes the permission allow-list, and the `autoMode` statement if you
     want one, to `~/.claude/settings.json`. A cloud session drops the
     `permissions.allow` rules in a repository's `.claude/settings.json`,
     because it never trusts the workspace. It keeps nothing in
     `~/.claude/settings.json` between sessions. And `autoMode` is read only
     from user settings. So the setup script, which runs before every session,
     is the one place these stay in force.
   - It runs `bash /home/user/<repo>/.claude/hooks/install-plugins.sh /home/user/<repo> || true`,
     so the plugin loads from the first prompt.
   - It installs the tools the repository's `verify` commands need, at the
     versions its CI pins. The cloud image's own copies can be too old: on
     2026-11-01 its `golangci-lint` 2.5.0 refused z5labs/humus's Go 1.27 code.
     [`.claude/hooks/install-dagger.sh`](.claude/hooks/install-dagger.sh) is the
     example for `dagger`. Allow the hosts each install fetches from under the
     environment's network access.
3. **Run it.** Start a cloud session on the repository and run
   `/backlog:run-backlog`, or `/backlog:next-issue` for one issue.

Two parts of the cycle are local-only, because the session's GitHub proxy
refuses what they need. One is the `local` reviewer, which mints a GitHub App
token. The other is project-scoped selection (`select.project`,
`--project-value`), which reads Projects over GraphQL. So in a cloud session,
`copilot` is the only reviewer a `["copilot", "local"]` roster has. With no
trailing `"none"`, a run there ends in `BLOCKED` whenever Copilot is
unavailable. Work the backlog from a local checkout until it is back.

## Dagger in cloud sessions

A cloud session starts without Dagger or a running container runtime. The
`SessionStart` hook
[`.claude/hooks/install-dagger.sh`](.claude/hooks/install-dagger.sh) installs
the Dagger CLI at the version pinned by `dagger-version` in
[`change-aware-ci.yml`](.github/workflows/change-aware-ci.yml), puts it on
`PATH`, and starts `dockerd` for the engine, so `dagger check` runs as it does
in CI.

The environment's network policy has to allow two hosts:

- `dl.dagger.io`, which serves the install script and the CLI.
- `registry.dagger.io`, which serves the engine image.

Add them under the environment's [Network access → Allowed
domains](https://code.claude.com/docs/en/cloud-environments#network-access). If
either one is blocked, the hook prints a line naming it, and the session starts
without Dagger.

## CI

Every module has a sibling `tests/` module whose suite is a Dagger check.
[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs them through
[`daggerverse/workspace-ci`](daggerverse/workspace-ci), which plans each change:
it diffs the commit range, works out which modules the change could reach, and
returns one leg per check to run — skipping those a previous run already proved
good — each routed at the module that owns it. Two checks run whatever changed:
the Go SDK's `dagger-go-sdk:stale`, which proves every committed binding is what
`dagger generate` writes, and the [`ci/`](ci/) module's `ci:generated`, which
proves that check covers every module and can fail.

The repository is a Dagger v1 workspace: a root `dagger.toml`, a
`dagger-module.toml` per module, and a committed `dagger.lock` that pins every
image digest and the SDK module chain. Every CI job fails if it leaves
`dagger.lock` changed, so commit whatever entries a new image adds.

The Actions half of that — engine image caching, one `dagger check` per leg,
recording a pass, and the single status check branch protection requires — is
[`.github/workflows/change-aware-ci.yml`](.github/workflows/change-aware-ci.yml),
a `workflow_call` workflow anyone can use. `ci.yml` is a caller of it like any
other repository would be, differing in pointing at the in-tree planner so that a
change to the planner is planned by the changed planner, and in one job of its
own that runs the planner's tests directly whenever a CI file changes. Adopting it
elsewhere is one `uses:`:

```yaml
jobs:
  ci:
    uses: z5labs/devex/.github/workflows/change-aware-ci.yml@main
    secrets: inherit
```

with `ci / CI Gate` as the required check. See
[`daggerverse/workspace-ci/README.md`](daggerverse/workspace-ci/README.md#github-actions)
for the inputs and the permissions a caller needs.

The manually-triggered
[`update-dagger.yml`](.github/workflows/update-dagger.yml) workflow bumps the
Dagger pin across every module and opens a PR. Its PR edits
`change-aware-ci.yml` (a workflow file), which the default `GITHUB_TOKEN` may not
push, so it authenticates with a
dedicated **update-dagger GitHub App** via two repository secrets:

| Secret | Value |
| ------ | ----- |
| `UPDATE_DAGGER_APP_ID` | the App's numeric App ID |
| `UPDATE_DAGGER_APP_KEY` | the App's PEM private key |

Install the App on this repo with `Contents`, `Pull requests`, and `Workflows`
all set to **Read and write** (`Workflows: write` is what `GITHUB_TOKEN` can't
have). The short-lived token is minted per run, so nothing needs rotating.

The scheduled [`update-dagger-lock.yml`](.github/workflows/update-dagger-lock.yml)
workflow runs `dagger lock update` every Monday and opens a PR with the refreshed
pins, through the same App: a pull request opened with `GITHUB_TOKEN` triggers no
CI, and running every suite against the new pins is the point of it.

## License

[MIT](LICENSE) © Z5labs and Contributors
