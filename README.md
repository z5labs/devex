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
| [`certificate-management`](daggerverse/certificate-management) | Manage X.509 certificate authorities and issue TLS certificates. |
| [`crypto`](daggerverse/crypto) | Common crypto utilities — file digests and ephemeral keys. |
| [`dgraph`](daggerverse/dgraph) | Spin up a Dgraph graph-database cluster. |
| [`envoy`](daggerverse/envoy) | Build Envoy proxy configurations and components. |
| [`flash`](daggerverse/flash) | Codeify firmware flashing as Dagger functions. |
| [`go`](daggerverse/go) | Wrap the Go CLI surface (build, test, vet, fmt, run). |
| [`grafana-stack`](daggerverse/grafana-stack) | Spin up Loki, Tempo, and Mimir as Dagger services. |
| [`java`](daggerverse/java) | Wrap the JVM toolchain — the JDK plus Maven and Gradle. |
| [`kafka`](daggerverse/kafka) | Spin up a Kafka-wire-compatible cluster. |
| [`otel`](daggerverse/otel) | Spin up the OpenTelemetry Collector as a service. |
| [`postgres`](daggerverse/postgres) | Spin up a single-node PostgreSQL 17 primary. |
| [`qemu`](daggerverse/qemu) | Boot guest systems under [QEMU](https://www.qemu.org/). |
| [`random`](daggerverse/random) | Generate random values. |
| [`z5labs`](daggerverse/z5labs) | Scaffold project archetypes (GoApp / GoLib). |
| [`zig`](daggerverse/zig) | Wrap the [Zig](https://ziglang.org/) toolchain. |

See [`daggerverse/CLAUDE.md`](daggerverse/CLAUDE.md) for module conventions
(function caching, code generation, tests layout).

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
| [`daggerverse`](plugins/daggerverse) | `/plan-dagger-module` — paces a design conversation and drafts story issues for a new daggerverse module. |

See [`plugins/README.md`](plugins/README.md) for the plugin layout. To develop
against an unmerged local checkout, run `/plugin marketplace add .` from the
repo root instead.

## CI

Checks run through Dagger via the [`ci/`](ci/) module, wired into GitHub Actions
in [`.github/workflows/ci.yml`](.github/workflows/ci.yml). Each module has a
sibling `tests/` module exposed as a toolchain in
[`dagger.json`](dagger.json).

## License

[MIT](LICENSE) © Z5labs and Contributors
