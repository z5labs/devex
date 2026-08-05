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
`BLOCKED`, or at a bounded iteration count. A worker that hands back control
with its issue unfinished says `IN FLIGHT` and is *resumed* rather than
replaced — a fresh worker would take a different issue and leave the first one's
pull request open for good.

Nothing repository-specific lives in the skills. The knobs live in
`.claude/backlog.json` — which label, milestone and optional project field value
form the backlog, how issue bodies declare dependencies, the commands that must
pass before a pull request opens, the merge label and workflow, whether a review
is required, and where worktrees go — described by
[`assets/backlog.schema.json`](backlog/assets/backlog.schema.json). The repo
slug and default branch are deliberately *not* among them: they are read from
`gh repo view`, so a fork or a rename cannot leave the config describing a
repository it is no longer in.

The cycle never runs `gh pr merge`. It labels the pull request and
[`assets/auto-merge.yaml`](backlog/assets/auto-merge.yaml) hands the merge to
GitHub, gated by the default branch's protection rules — which is what puts the
merge policy somewhere readable and what lets the loop run unattended at all.

Four parts of the cycle are scripts rather than numbered steps, for the same
reason: they carry no judgment, and a procedure an agent retypes cannot be
tested.

Picking the issue is
[`scripts/select-issue.sh`](backlog/scripts/select-issue.sh) — label, milestone,
limit and dependency convention in, one issue or `BACKLOG EMPTY` or `BLOCKED`
out, as an exit code. It replaced three `awk`/`sed`/`grep` pipelines that lived
in the skill as prose, all of which were wrong: a sentence reading *this issue is
not blocked by anything* opened a dependency list, a fenced code block quoting
the convention counted as a declaration, GitHub's `- [ ] #12` task-list form
extracted nothing, and a cross-repository `owner/repo#N` terminated the list and
took the real dependencies below it with it. Three of those made an issue
*silently eligible*, which is the failure that gets work done in the wrong order
rather than not at all. Those bodies are now
[`scripts/select-issue_test.sh`](backlog/scripts/select-issue_test.sh), which
needs no network.

The same script can narrow the backlog to one value of a single-select field on
a GitHub project — `--project-value workspace-ci` on a repository that groups
its work by `Module` — for repositories where the axis worth working along is
one the label and milestone filters cannot see. The value is read over GraphQL
as a `ProjectV2ItemFieldSingleSelectValue`, checked against the field's declared
options so a typo cannot resolve to an empty backlog, and intersected with this
repository's issues because an org-level project spans repositories. Every way
it can fail is exit 4: falling back to the unfiltered backlog when a scope was
asked for is the same class of failure as a silently eligible issue.

A repository that has adopted GitHub's typed issue dependencies can declare
`dependencies.style: "native"` and stop having its bodies parsed at all: the
`blockedBy` edges are read over GraphQL, and a typed edge cannot be written
ambiguously, survives a rewording of the body, and removes the whole class of
defect the paragraph above catalogues. It is opt-in and never reached by
fallback, because an unpopulated edge set and an unblocked issue are the same
response — a repository that writes its ordering in prose would come back
"nothing blocks this" for every issue, which is the silently-eligible failure
arrived at from the other direction. So a body parse that finds nothing never
escalates to it, and `setup-backlog` reports that typed dependencies exist
rather than inferring the style from their absence.

Waiting for CI is
[`scripts/await-checks.sh`](backlog/scripts/await-checks.sh) — `0 settled / 1
failed / 2 still running / 3 no checks reported`. It replaced `gh pr checks
--watch` handed to a background monitor, which was the cycle's one real
deadlock. A monitor reports *after* the turn that armed it, and a worker
subagent ends its turn by **returning** to its caller — so the wait died with
the agent, and the resumed worker held for an event that could never arrive
while the rule against busy-waiting stopped it checking by hand. One measured
iteration cost about 2.4M tokens across thirteen resume nudges, against a
300–375k band, and one of those resumes made no tool calls at all. All three
waits are now bounded, blocking calls sharing one contract: exit 2 means "not
finished, call me again", in the same turn.

Its judgment is one function — fail-fast over pending siblings, `skipping` as
settled, an unreadable or empty response as *not reported yet* rather than as an
answer — exposed as `--classify` on stdin and pinned offline by
[`scripts/await-checks_test.sh`](backlog/scripts/await-checks_test.sh). A red
check read as pending is a wait that times out; read as settled, it is a merge.

The review gate is
[`scripts/await-review.sh`](backlog/scripts/await-review.sh) — request, wait,
and classify what landed, as `0 completed / 1 declined / 2 timed out / 3 could
not request`. "Did Copilot review this?" looks like one question and is two, and
the two used to sit sixty lines apart: the wait exited on the first `reviewed`
event, while whether that review had *declined* the work was a separate command
under its own heading. Copilot declines a pull request over 300 files with a
review whose body says so, and that decline satisfies any `length > 0` test —
which is how a cycle once merged with no review at all. The merge label is the
assertion that a review completed, so the assertion is now an exit code.

The tail of the cycle is
[`scripts/finish-issue.sh`](backlog/scripts/finish-issue.sh) rather than five
more numbered steps. Waiting for the merge, closing the issue, verifying it
closed, dropping the worktree and deleting the local branch involve no judgment,
run at the point where an agent's context is fullest, and can be half-done
without anything looking wrong. The close matters most: a `Closes #N` line does
*not* close the issue when a token performs the merge — the closing reference
registers and nothing acts on it — and an issue left open is one the next
iteration selects again. One call with a meaningful exit code cannot be
forgotten and asserts the close instead of trusting it.

Setting up the App token matters more than it looks. GitHub creates no workflow
run from an event triggered by `GITHUB_TOKEN`, so a workflow that merges with
its own token emits a `closed` event that starts nothing — and every job hanging
off that event is skipped in silence. Merging with a GitHub App installation
token instead is what makes `close-linked-issues` and `delete-merged-branch` run
at all; `setup-backlog` checks for the two secrets and reports the degradation
when they are absent.

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
    ├── hooks/               # hooks.json (auto-discovered)
    ├── assets/              # files a skill copies into a target repo
    └── scripts/             # executables a skill calls, with their tests
```

- **`.claude-plugin/plugin.json`** describes the plugin. The only required field
  is `name` (kebab-case, used to namespace components, e.g.
  `<name>:<skill>`). `description`, `version`, and `author` are recommended.
- **Component directories** (`skills/`, `commands/`, `agents/`, `hooks/`) are
  discovered automatically when the plugin is installed — you only add a key to
  `plugin.json` to point at a non-default location.
- **`assets/` and `scripts/`** are not discovered; a skill reaches them through
  `${CLAUDE_PLUGIN_ROOT}`. Put a step there rather than in prose whenever it is
  deterministic — a script can be tested, and a procedure written out for a
  model to retype cannot.

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
