---
name: review-plugin
description: Review whether a Claude Code plugin works in hosted agent sessions — Claude Code on the web (`claude-code-cloud`), a GitHub Copilot session in a cloud sandbox started from a repository's Agents tab (`copilot-cloud-sandbox`), and a Copilot session on an Actions runner (`copilot-actions`). Runs an offline scan of the plugin's scripts, hook commands, fenced shell blocks and tool frontmatter against per-target profiles, reads its skill and agent prose for assumptions no pattern can see, and writes a Markdown report — per target, which components work, which need setup and which break, each finding with file:line, evidence, remedy, where the fix belongs and its sources. Use this whenever someone asks whether a plugin, skill or agent will work "in the cloud", "on the web", "in a cloud session", "with Copilot", "in the Copilot coding agent" or "in a hosted agent", why a plugin fails there, or what has to change before it can run unattended in one. Takes a plugin directory, and optionally `--target <id>` (repeatable) and `--out <file>`. Skip it for a plugin's behaviour on a laptop, which needs no review.
allowed-tools: Bash, Read, Glob, Grep, Write
---

# review-plugin

Report, per hosted target, which of a plugin's components will work, which need setup and which
will fail — and, for every finding, the file and line, the remedy, and whether the fix belongs in
the plugin, the repository or the environment.

Two things produce the findings, and the report keeps them apart:

- **The scan** — `scripts/scan-plugin.sh`. Deterministic, offline, tested. It matches what a
  pattern can see where commands *run*: scripts, hook commands, fenced shell blocks, tool
  frontmatter. Its findings carry the profile rule's basis — `docs`, `observed` or `unconfirmed`.
- **You** — reading the prose for what a pattern cannot see: "stop and wait for the user", a wait
  bounded at two hours, a flow that opens several pull requests, an agent told in prose to invoke a
  skill. Your findings are `basis: review`, and the report marks them so.

You **never drop or downgrade a scan finding**, and the scan enforces that: your findings are merged
in, and nothing you add can remove one or lower a verdict. If you think a scan finding is wrong,
say so in the report's notes and leave the finding where it is — the fix for a wrong finding is a
change to the profile or the scanner, with a test, not a reviewer's opinion.

You **do not edit the plugin under review**, and you write nothing inside it. The review JSON and
the report go in a scratch directory, or where `--out` says.

## Arguments

- `<plugin-dir>` — the directory holding `.claude-plugin/plugin.json`. Required. If the user named
  a plugin rather than a path, find it (`plugins/<name>`, or the marketplace's `source`); if there
  is no such directory, stop and say so.
- `--target <id>` — repeatable; `claude-code-cloud`, `copilot-cloud-sandbox`, `copilot-actions`. Default: all.
- `--out <file>` — where to write the report. Default: a fresh scratch directory, named in your reply.

## Step 1 — Scan

```bash
work=$(mktemp -d) && echo "work=$work"
"${CLAUDE_PLUGIN_ROOT}/scripts/scan-plugin.sh" <plugin-dir> [--target <id>]... >"$work/scan.json"
echo "exit $?"
```

The first line prints the scratch directory. Below it is written `<work>`: use the printed path, in full, everywhere it appears. The Read and Write tools do not expand `$work`, and a later Bash call does not keep the variable.

| Exit | Meaning | Do |
|---|---|---|
| 0 | scanned; no finding breaks | go on |
| 1 | scanned; at least one finding breaks | go on — this is a result, not a failure |
| 2 | not a plugin, a profile unreadable, bad arguments | stop and report the message on stderr verbatim |

The scan finds the repository the plugin is delivered from by itself (the git work tree around
it) and judges delivery from it. If the plugin lives outside the repository that will deliver it,
pass `--repo <that repository>`.

Read `<work>/scan.json`. For each target it holds `delivery`, `components` (each with its `verdict`
and the `rules` behind it), `findings`, `guards` — each script line that carries a rule's guard —
and `review_rules` — the rules the profile leaves to you, each with what to `look_for`. The
top-level `edges` are the calls the scan could see.

A script that already handles a restriction is rated by what happens there. A command rule with a
`guard` rates every match in a script at the guard's severity when another line of code in that
script matches the guard's pattern — a test of `CLAUDE_CODE_REMOTE` that skips the step, a grep
that recognises the proxy's refusal by its message. A finding rated that way names its guard
(`guard.line`), and whatever reaches the script inherits that severity.

## Step 2 — Read the prose

Read every skill, agent and command of the plugin — the whole file, not a grep — with the
`review_rules` of each target in front of you. For each one, find the passages it describes. These
are the ones the profiles define today:

| Rule | What to look for |
|---|---|
| `waits-for-user` | "Stop and ask", "wait for the user", a checkpoint after each step, "do not proceed without" an answer |
| `bash-ten-minute-cap` (`claude-code-cloud`) | one command expected to block longer than 10 minutes in the foreground, or a long build or test run (a verify step, a full suite) started with no explicit Bash `timeout`, which is moved to the background at the 2-minute default |
| `pushes-own-branch` (`claude-code-cloud`) | a flow that creates a branch other than the session's own, pushes it and works from it |
| `session-files-not-kept` (`claude-code-cloud`) | a file written outside the committed repository — `~/.claude/settings.json`, `.claude/settings.local.json` — that a later session is expected to find |
| `session-time-limit` (`copilot-actions`) | a flow that runs past an hour end to end, or a wait bounded beyond it — a two-hour bound, a loop of five-minute waits |
| `several-pull-requests` (Copilot) | a flow that opens more than one pull request, or works across branches |
| `several-repositories` (Copilot) | work in repositories other than the one the session started in |
| `needs-preinstalled-tools` (Copilot) | a CLI, runtime or daemon a stock environment lacks |
| `background-work-lost-on-reclaim`, `background-work-across-resume` | waits, watchers or subagents that must survive the session going idle |

Take the rule's own `severity` in that target. Do not raise or lower it: the profile decided it,
with sources. An `info` rule such as `pushes-own-branch` is still worth recording: it tells the
reader the assumption was checked and holds.

Prose can also tell the model to run a command outside any fenced block — "3. **Auth.** `gh auth
status`". That is a command where it runs, and a scanned rule applies to it. Cite that rule. Where
the rule gives a lower `severity_in.fence`, use it, because the model sees the failure and can
adapt. The exception is prose that makes the failure final for the whole component, such as "run
these before the first iteration and stop if any fails": then use the rule's base `severity`. A
failure confined to one step, such as a last step that cannot create its issues, keeps the fence
severity.

A script can hide a command from the scan behind a shell function of its own: `app_api /app`,
where `app_api` runs `gh api`. That is still a command where it runs. Cite the scanned rule at the
line of the call, at the rule's script severity — unless the script carries that rule's guard.
Then the call is cited at the guard's severity, as the scan rates the calls it can see: the
backlog plugin's `app-token.sh` tests `CLAUDE_CODE_REMOTE` before it calls `app_api /app`, so
`non-repository-endpoint` is cited there at `degrades`, not `breaks`. `guards` in `scan.json`
says which scripts carry which rule's guard; leave `severity` out of the finding and the merge
gives it the guard's.

Subagents follow the same reasoning. On the Copilot targets the scan gives
`subagents-unsupported` its lower `severity_in.frontmatter` for a tool that the frontmatter only
declares, because a declaration says a component *may* spawn, not that it depends on it. Where the
prose spawns a subagent the flow cannot do without — "spawn the reviewer for each file", with
nothing said for a session that has no `Agent` tool — cite the rule at its base `severity`, at the
line of that passage. Where the prose says what to do without the tool, the declaration's lower
severity stands and there is nothing to add.

Then look for **calls written only in prose**. The scan follows a script run from a fenced block, a
fenced `Agent(subagent_type: …)` spawn and a fenced `<plugin>:<skill>` mention; it does not follow
"invoke the `backlog:next-issue` skill" written as a sentence. Where a component's instructions
make it run another component of the plugin, the caller inherits the callee's worst finding in each
target: add a finding on the caller, citing the callee's rule, at the callee's severity, with
`"via": ["<callee>"]`, and the line in the caller where the call is written as the location.

Do not report what the scan already reports, and do not report prose that merely *mentions* a
command. A finding needs a passage that makes the component do the thing.

## Step 3 — Write the review findings and the report

Write your findings to `<work>/review.json` with the Write tool — a JSON array, empty if you found
nothing:

```json
[
  {
    "target": "copilot-cloud-sandbox",
    "rule": "waits-for-user",
    "component": "daggerverse:plan-dagger-module",
    "file": "plugins/daggerverse/skills/plan-dagger-module/SKILL.md",
    "line": 42,
    "evidence": "**Stop. Ask the user to pick one (or propose their own).** Do not proceed without an explicit name.",
    "via": []
  }
]
```

- `target`, `rule`, `component`, `file`, `line` and `evidence` are required. `component` and
  `file` are spelled as the scan spells them; `rule` must be a rule of that target's profile —
  a finding citing anything else is refused (exit 2).
- `severity` defaults to the rule's — on a script that carries the rule's guard, the guard's.
  `remedy` and `fix_in` default to the rule's; give your own only when the rule's would mislead
  for this passage. `sources` are the rule's.
- What you give is checked, and a finding that fails a check is refused:
  - `fix_in` must be `plugin`, `repository` or `environment`;
  - `sources`, if given, must be a non-empty list of https URLs;
  - `via` must be a list of component names;
  - leave out `component_kind`: it is the named component's.
- Do not set `basis`; the merge sets it to `review`.

Merge them and render the report:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/scan-plugin.sh" <plugin-dir> [--target <id>]... \
  --review "<work>/review.json" --format markdown >"<work>/report.md"
echo "exit $?"
```

Exit 2 here means a review finding was refused; stderr says which and why. Fix the finding — never
drop the scan's — and run it again.

Then append a `## Reviewer's notes` section to the report, only if there is something to say:

- assumptions you found that no profile rule covers, each with its `file:line` and the rule you
  would add (these are not findings, because nothing backs them yet);
- a scan finding you believe is wrong, and why — left in the report, untouched;
- anything you could not read.

Copy the report to `--out` if one was given.

## Step 4 — Reply

Lead with the delivery line and the component table for each target — the first part of the report,
verbatim — and say where the full report is. Say which verdicts are unconfirmed and that a profile
is only as current as its checked date. Keep the findings themselves in the file.

## Profiles

The facts live in `${CLAUDE_PLUGIN_ROOT}/assets/targets/<id>.json`, one file per target,
described by `assets/target.schema.json`. Every rule carries its sources, a `basis` and the date it
was last checked; where the sources disagree it is `unconfirmed` until someone observes it in a
real session, and an observation is recorded under `observed` with a link to where. Observed
outranks docs. Adding a target means adding a file. Changing a fact means changing the rule, its
`checked` date, and — for a scanned rule — the fixture under `scripts/testdata/rules/<rule-id>`
that `scripts/scan-plugin_test.sh` runs.
