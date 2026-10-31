# Plugins

This directory holds the [Claude Code plugins](https://code.claude.com/docs/en/plugins)
published by the [`z5labs-devex` marketplace](../.claude-plugin/marketplace.json).
Keeping them here keeps the plugin catalog cleanly separate from the Dagger
tooling (`daggerverse/`, `ci/`) and the docs (`docs/`).

| Plugin | Provides |
| ------ | -------- |
| **[`backlog`](backlog/)** | `next-issue`, `run-backlog` and `setup-backlog` — an unattended issue-to-merge cycle for any GitHub repository, configured per repo by `.claude/backlog.json`. |
| **[`daggerverse`](daggerverse/)** | `plan-dagger-module` — a paced design workflow that drafts story issues for a new daggerverse module. |
| **[`hosted-agents`](hosted-agents/)** | `review-plugin` — says, per hosted agent target, which of a plugin's components will work, which need setup and which will fail, from data-driven target profiles and an offline scan. |

## `backlog`

Takes one story issue from the backlog to a merged pull request — select,
worktree, implement, verify, PR, wait for checks, get a review from the
configured roster and answer it, label for auto merge, close the issue, clean
up — then stops.
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

Native auto merge does not exist everywhere. A private repository on GitHub
Free cannot turn it on, and branch protection and rulesets answer `403: Upgrade
to GitHub Pro or make this repository public`. Zaba505/backup's operator had to
fork the merge step by hand to get past it. The workflow therefore reads
`autoMergeAllowed` over GraphQL at merge time. REST `allow_auto_merge` is
omitted for a token without push access, so its absence means nothing. Where the
setting is `false`, the workflow waits for every other check on the pull request,
for at most fifteen minutes, then merges with `--match-head-commit` using the App
token, so the `closed` jobs still fire. It reads the checks with `GITHUB_TOKEN`
under `checks`, `statuses` and `actions` read, because the App's Contents / Pull
requests / Issues grant cannot read them. Both steps are run straight out of the
asset against a fake `gh` by
[`scripts/auto-merge_test.sh`](backlog/scripts/auto-merge_test.sh).

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

The wait is tied to one commit, the one just pushed. It used to read `gh pr
checks <pr>`, which names no commit. Right after a push, before anything had
registered for the new head, that read returned the previous head's settled
runs, and z5labs/humus #676 exited 0 on them straight after its review fix. The
script now confirms the pull request's head is the pushed SHA (exit 5 if it
never becomes it). It reads that commit's check runs and statuses and drops
any check that ran on another commit. It also treats a required status check
of the base branch that has not registered yet as pending. Those are the set
GitHub waits for anyway, which closes the second window: the fast checks are
green before the slow ones exist.

The review gate is
[`scripts/await-review.sh`](backlog/scripts/await-review.sh) — request from one
rung, wait, and classify what landed, as `0 completed / 1 refused / 2 nothing
yet / 3 unavailable / 5 unclassifiable / 6 Copilot errored on this pull
request`. "Did the reviewer review this?" looks like one question
and is two, and the two used to sit sixty lines apart: the wait exited on the
first `reviewed` event, while whether that review had *declined* the work was a
separate command under its own heading. Copilot declines a pull request over 300
files with a review whose body says so, and that decline satisfies any
`length > 0` test — which is how a cycle once merged with no review at all. The
merge label is the assertion that a review completed, so the assertion is now an
exit code, pinned offline by
[`scripts/await-review_test.sh`](backlog/scripts/await-review_test.sh).

Copilot also posts a review when its own run crashes ("encountered an error
and was unable to review"). That wording matched nothing, so z5labs/humus
#747 to #750 each exited 0 on a review that never happened. The fix does not
match wording, because Copilot rewords its reviews. The gate reads the
`copilot-pull-request-reviewer` check run on the reviewed commit instead,
re-requests once on a failure, and exits 6 if the failure repeats. Exit 6
advances the roster without retiring Copilot for the run.

`review.reviewers` makes that gate an ordered roster — `["copilot", "local",
"none"]` — tried in order and failing over on **availability alone**. It
replaced a boolean whose two settings were "Copilot gates the merge" and
"nothing does", which forced a repository whose monthly Copilot allowance had
run out to choose between no loop and no review, permanently, in a tracked file.
The `local` rung is an adversarial review by a fresh, context-free subagent,
posted as a `COMMENT` review under a GitHub App identity by
[`scripts/post-review.sh`](backlog/scripts/post-review.sh) — never under the
operator's token, which opened the pull request — so it lands on the timeline
as a `reviewed` event and one gate covers every rung. A rung that *refused* the
work does not advance the roster: a reviewer with different limits is not a
second opinion on work the first one could not read, and advancing there is
precisely how unreviewable work reaches the default branch.

That App's credentials come from two environment variables, and `review.app`
names which two — `BACKLOG_REVIEW_APP_ID`/`BACKLOG_REVIEW_APP_KEY` by default.
An App is installed per *account*, so an operator with repositories under an
organisation and a personal account has two reviewer Apps and one environment to
hold both; with the names fixed, working the other account began by re-exporting
the pair from memory. Forgetting was not loud: the wrong account's App is not
installed here, the installation lookup says so, and that is exit 3 —
UNAVAILABLE, which the roster reads as a rung that is *down* and fails over. The
names are names, not values, so they belong in the tracked config while the
credentials stay in the environment. The merge side is untouched: repository
secrets are already per repository, so `secrets.BACKLOG_APP_ID` resolves to a
different App in each account with no collision.

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

Its wait reads the merge workflow's runs as well as the pull request. A run
that fails arms nothing, so the pull request stays OPEN for ever. Reading only
its state, that looked exactly like a merge queued behind a slow check:
Zaba505/backup #13 spent its whole re-run budget waiting on a run that had
already failed. Each poll now classifies the `merge.workflow` runs on the
pull request's head commit. A failed or cancelled run is exit 5, at once,
naming the run. The classification is exposed as `--classify-runs` on stdin
and pinned offline by
[`scripts/finish-issue_test.sh`](backlog/scripts/finish-issue_test.sh).

Some criteria can only be met once that merge has landed — comment on the bug
report's discussion with a link to the merged pull request, then close it as
resolved. A criterion whose text begins `After merge:` is one of those, and
[`scripts/post-merge-criteria.sh`](backlog/scripts/post-merge-criteria.sh)
extracts them, pinned offline by
[`scripts/post-merge-criteria_test.sh`](backlog/scripts/post-merge-criteria_test.sh).
The pull request lists them unchecked, and the worker carries them out in order
once `finish-issue.sh` exits 0, records what each did in one comment on the
closed issue, and ends `BLOCKED` on the first that fails. The cycle is the
place for this because it is already waiting for the merge; a job in the merge
workflow would run only for App-token merges and be overwritten every time the
workflow is reinstalled. The issue still closes the moment the merge lands:
holding it open for a follow-up would get the merged work selected again.

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
environment — squash-only merging, whether native auto merge is available, the
default branch's required status checks, and whether Copilot code review is
really enabled — reporting what it cannot fix without repository-admin rights,
and telling a `403` that means missing admin rights from one that names a plan
limit no admin can lift, and offering a second reviewer rung rather than
a downgrade when Copilot is not there.

The reviewer credential it verifies by **minting a token**, not by checking that
a variable is exported. Setup is the one moment when a human is present and
nothing has merged yet, and the mint performs in order the four things that can
be individually wrong — the key does not sign, the ID and the key are two
different Apps', the App is not installed on this repository, the installation
lacks Pull requests: write — each of which gets its own sentence and its own
remedy, pinned offline by
[`scripts/app-token_test.sh`](backlog/scripts/app-token_test.sh). All four are
exit 3 at rung time, because the roster only ever needed to know the rung cannot
run; the operator needs to know which of the four, so the distinction lives in
the message. Succeeding also prints the App's login, which is the identity the
reviews will appear under. The merge credential cannot be checked that way — a
repository secret's value is unreadable outside a workflow run — so its row
stays a presence check and says so, and its first real proof is the first real
merge.

## `hosted-agents`

A plugin that works on a laptop can fail in a hosted agent session, and nothing
tells its author until someone runs it there. This repository learned that by
hand three times. A cloud session ignored the plugins `.claude/settings.json`
declares ([#469](https://github.com/z5labs/devex/issues/469)). `dagger check` needed a pinned CLI, a daemon, an engine routed
through the session's proxy and two allow-listed hosts ([#468](https://github.com/z5labs/devex/issues/468)). And the session's
GitHub proxy blocks GraphQL, so `gh pr`, `gh issue` and `gh project` return 403,
which puts the `backlog` cycle out of reach of a cloud session for a reason no
check reported. `review-plugin` is the check that would have.

It reports, per hosted target, which components will work, which need setup and
which will fail. Each finding gives the file and line, the evidence, the remedy,
and whether the fix belongs in the plugin, the repository or the environment.
There are three targets:

- **`claude-code-cloud`**: Claude Code on the web.
- **`copilot-cloud-sandbox`**: a GitHub Copilot session started from a
  repository's **Agents** tab, in a GitHub-hosted cloud sandbox.
- **`copilot-actions`**: a Copilot session on an Actions runner.

The facts are data, not code. Each target is a profile,
[`assets/targets/<id>.json`](hosted-agents/assets/targets/), described by
[`assets/target.schema.json`](hosted-agents/assets/target.schema.json). Every
rule carries what it matches, a severity, a remedy, `fix_in`, its source URLs,
a `basis` and the date it was last checked. These facts change: the Copilot
coding agent was renamed the cloud agent, gained a sandbox and its own plugin
settings in the space of a few months, and several of its pages still describe
one session type as if it were the only one. Where sources disagree, the rule
cites all of them and is `unconfirmed`, and the report says so. `observed` is
for what someone saw in a real session, with a link. It outranks `docs`: the
docs say a cloud session does not read user settings, and [#470](https://github.com/z5labs/devex/pull/470) watched plugins
declared there load. GraphQL's 403, `gh auth status` failing on the proxy's
placeholder token, and the 403 for a repository the session was not given were
all observed while building this plugin. Adding a target means adding a file.

The scan is
[`scripts/scan-plugin.sh`](hosted-agents/scripts/scan-plugin.sh), for the same
reason `select-issue.sh` is a script. A check an agent retypes each run cannot
be tested. The scan reads the manifest, skills, commands, agents, hooks,
`.mcp.json`, `.lsp.json` and scripts. It prints one JSON document, or with
`--format markdown` the report, and makes no network calls. It matches commands
only where they run: scripts, hook commands, fenced shell blocks and tool
frontmatter. A skill whose prose says "never run `gh pr merge`" is not a finding.
Neither is the same command in a JSON block or an error message. In a fenced
block or a script it is. `select-issue.sh` once had the opposite bug, where a
fenced example counted as a declaration. The rules for which context counts are
pinned by [`scripts/scan-plugin_test.sh`](hosted-agents/scripts/scan-plugin_test.sh).

Where a command runs also sets how bad it is. In a script, GraphQL's 403 is an
exit code the caller cannot recover from: the component breaks. In a skill's
fenced block, the model sees the error and the REST route it names, so the
component degrades. That is why `daggerverse:plan-dagger-module`, whose last step
runs `gh issue create`, degrades in a cloud session, while every `backlog` skill
breaks there through the scripts it runs.

Verdicts follow calls the scan can see:

- a skill that runs `scripts/x.sh` from a fenced block;
- a script that runs another by name;
- a fenced `Agent(subagent_type: …)` spawn;
- a fenced `<plugin>:<skill>` mention.

A component's verdict is the worst finding it reaches, and the report names the
path. Delivery comes first. Each profile's delivery rules decide, from the
repository the plugin is delivered from, whether it reaches a session at all. A
plugin that cannot be delivered is `breaks` for every component, and the report
says so before anything else.

The skill adds what a pattern cannot see and labels it. It reads skill and agent
prose for the assumptions the profiles mark `review`: "stop and wait for the
user", a wait bounded at two hours, a flow that opens several pull requests, an
agent told in prose to invoke a skill. It records them as `basis: review`, citing
a profile rule. The scan merges them in and recomputes verdicts. It refuses a
finding that cites no rule of the profile, and nothing merged can drop or
downgrade a scanned finding.

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
