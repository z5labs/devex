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

See [`plugins/README.md`](plugins/README.md) for the plugin layout. To develop
against an unmerged local checkout, run `/plugin marketplace add .` from the
repo root instead.

## CI

Every module has a sibling `tests/` module whose suite is a Dagger check.
[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs them through
[`daggerverse/workspace-ci`](daggerverse/workspace-ci), which plans each change:
it diffs the commit range, works out which modules the change could reach, and
returns one leg per check to run — skipping those a previous run already proved
good — each routed at the module that owns it. The [`ci/`](ci/) module holds only
the three checks that must run whatever changed.

The Actions half of that — engine image caching, the `dagger/checks` fan-out,
recording a pass, and the single status check branch protection requires — is
[`.github/workflows/change-aware-ci.yml`](.github/workflows/change-aware-ci.yml),
a `workflow_call` workflow anyone can use. `ci.yml` is a caller of it like any
other repository would be, differing only in pointing at the in-tree planner so
that a change to the planner is planned by the changed planner. Adopting it
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

## License

[MIT](LICENSE) © Z5labs and Contributors
