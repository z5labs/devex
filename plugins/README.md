# Plugins

This directory holds the [Claude Code plugins](https://code.claude.com/docs/en/plugins)
published by the [`z5labs-devex` marketplace](../.claude-plugin/marketplace.json).
Keeping them here keeps the plugin catalog cleanly separate from the Dagger
tooling (`daggerverse/`, `ci/`) and the docs (`docs/`).

| Plugin | Provides |
| ------ | -------- |
| **[`backlog`](backlog/)** | `next-issue`, `run-backlog` and `setup-backlog` — an unattended issue-to-merge cycle for any GitHub repository, configured per repo by `.claude/backlog.json`. |
| **[`daggerverse`](daggerverse/)** | `plan-dagger-module` — a paced design workflow that drafts story issues for a new daggerverse module. |

## `backlog`

Takes one story issue from the backlog to a merged pull request — select,
worktree, implement, verify, PR, wait for checks, get a Copilot review and
answer it, label for auto merge, close the issue, clean up — then stops.
`run-backlog` repeats that with a fresh `issue-worker` subagent per iteration,
so each issue starts from clean context, and halts on `BACKLOG EMPTY`, on
`BLOCKED`, or at a bounded iteration count.

Nothing repository-specific lives in the skills. Six knobs live in
`.claude/backlog.json` — which label and milestone form the backlog, how issue
bodies declare dependencies, the commands that must pass before a pull request
opens, the merge label and workflow, whether a review is required, and where
worktrees go — described by
[`assets/backlog.schema.json`](backlog/assets/backlog.schema.json). The repo
slug and default branch are deliberately *not* among them: they are read from
`gh repo view`, so a fork or a rename cannot leave the config describing a
repository it is no longer in.

The cycle never runs `gh pr merge`. It labels the pull request and
[`assets/auto-merge.yaml`](backlog/assets/auto-merge.yaml) hands the merge to
GitHub, gated by the default branch's protection rules — which is what puts the
merge policy somewhere readable and what lets the loop run unattended at all.

The tail of the cycle is
[`scripts/finish-issue.sh`](backlog/scripts/finish-issue.sh) rather than five
more numbered steps. Waiting for the merge, closing the issue, verifying it
closed, dropping the worktree and deleting the local branch involve no judgment,
run at the point where an agent's context is fullest, and can be half-done
without anything looking wrong. The close matters most: a `Closes #N` line does
*not* close the issue when a workflow's `GITHUB_TOKEN` performs the merge, and
an issue left open is one the next iteration selects again. One call with a
meaningful exit code cannot be forgotten and asserts the close instead of
trusting it.

`setup-backlog` bootstraps a target repository: it writes the config with
`verify` and `dependencies.style` inferred from the repository rather than
asked, installs the workflow, creates the labels, and then *verifies* the
environment — squash-only merging, the default branch's required status checks,
and whether Copilot code review is really enabled — reporting what it cannot fix
without repository-admin rights.

## Convention

Each plugin is a self-contained directory:

```
plugins/
└── <name>/
    ├── .claude-plugin/
    │   └── plugin.json      # required manifest (only `name` is required)
    ├── skills/              # SKILL.md directories (auto-discovered)
    ├── commands/            # flat .md command files (auto-discovered)
    ├── agents/              # agent .md files (auto-discovered)
    └── hooks/               # hooks.json (auto-discovered)
```

- **`.claude-plugin/plugin.json`** describes the plugin. The only required field
  is `name` (kebab-case, used to namespace components, e.g.
  `<name>:<skill>`). `description`, `version`, and `author` are recommended.
- **Component directories** (`skills/`, `commands/`, `agents/`, `hooks/`) are
  discovered automatically when the plugin is installed — you only add a key to
  `plugin.json` to point at a non-default location.

See the [plugins reference](https://code.claude.com/docs/en/plugins-reference)
for the full `plugin.json` schema.

## Registering a plugin in the marketplace

After adding `plugins/<name>/`, append an entry to the `plugins` array in
[`../.claude-plugin/marketplace.json`](../.claude-plugin/marketplace.json):

```json
{
  "name": "<name>",
  "source": "./plugins/<name>",
  "description": "What the plugin does"
}
```

The relative `source` path is resolved from the marketplace root (the directory
containing `.claude-plugin/`), so `./plugins/<name>` points at this directory.
Relative paths only resolve when the marketplace is added via git. If the
catalog grows, set `metadata.pluginRoot` to `"./plugins"` in `marketplace.json`
so entries can use the bare `"source": "<name>"` shorthand.
