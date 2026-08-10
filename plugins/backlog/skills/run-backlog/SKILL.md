---
name: run-backlog
description: Drive a repository's story backlog unattended, one issue at a time, by spawning a fresh `issue-worker` subagent per iteration and halting on `BACKLOG EMPTY`, on `BLOCKED`, or at a bounded iteration count. Use this whenever the user wants the backlog worked continuously rather than a single issue — "run the backlog", "work through the issues", "keep taking stories until you run out", "drain the v0.3.0 milestone", "work through the workspace-ci stories", "work the In Progress stories", or a `/loop` that repeats the cycle. Takes an optional integer argument setting the maximum number of iterations, the bare word `all` meaning every optional narrowing is dropped, and optional `--label <name>`, `--milestone <title>`, `--no-milestone-filter`, `--project-value <value>`, `--project-field <name>`, `--project-owner <login>`, `--project-number <n>` and `--no-project-filter` arguments restricting the whole run, and an optional `--reviewers <a,b,c>` argument replacing the configured reviewer roster for one run. Skip this when the user wants exactly one issue (`backlog:next-issue`) or wants the repository bootstrapped first (`backlog:setup-backlog`).
allowed-tools: Agent, SendMessage, Bash, Read, Glob, Grep, TaskCreate, TaskUpdate, ScheduleWakeup
---

# run-backlog

Work the backlog until it is empty, something blocks, or the iteration bound is reached.

You are the driver, not the worker. Every iteration is done by a **fresh subagent** and none
of it happens in your context.

That is the whole design, and it is not an optimisation. One issue's implementation — the
files read, the tests written, the CI logs, the review thread — is far more context than the
decision "keep going or stop" needs. Working an issue yourself gets you three or four
iterations in before the context that made the first one go well has been squeezed out by
the details of the third. A subagent per iteration means iteration twelve starts as clean as
iteration one, and all you ever hold is a list of one-line outcomes.

## 0. Preflight

Run these once, before the first iteration, and stop if any fails:

1. **Config present.** `jq . .claude/backlog.json`. If it is missing or does not parse,
   stop and say so, naming `backlog:setup-backlog`. Do not start a loop that will fail
   identically on every iteration.
2. **Main checkout clean.** `git status --porcelain` in the repository root. Uncommitted
   changes you did not make are a stop condition for the cycle, so they are a stop condition
   for the loop; report what is dirty and let the user decide.
3. **Auth.** `gh auth status`. An expired token turns every iteration into the same
   opaque failure. If this run carries a **project** scope (below) — or the config pins one —
   the scopes it prints must include **`read:project`**; `repo` does not imply it, and without
   it every iteration fails identically at selection. `gh auth refresh -s read:project` grants
   it; report and stop rather than dropping the scope and running the whole backlog. A run
   narrowed only by milestone or label needs nothing beyond `repo`.

Also settle the **bound** now. It is the skill's integer argument when one was given;
otherwise default to **10** iterations. Say the number in your first message. An unbounded
loop against a large backlog is not the user asking for autonomy, it is the user losing the
chance to look at the first result before the twentieth lands on top of it.

And settle the **selection arguments**. Everything the repository's `.claude/backlog.json`
pins is a default that this run can replace, and the whole run then uses the same set:

```
/run-backlog 5 --project-value workspace-ci
/run-backlog 5 --project-field Status --project-value "In Progress"
/run-backlog 5 --milestone v0.3.0
/run-backlog 5 all
/run-backlog 5 --reviewers copilot,local
```

The argument order does not matter. The integer is the bound and the bare word `all` is
defined below. Every argument beginning `--` **except `--reviewers`** is passed through to
selection, and those are exactly the ones `scripts/select-issue.sh` takes, so a request the
script cannot express is a request this skill cannot accept either. `--reviewers` is the one
that goes elsewhere — to step 7 of each worker — and it has its own section below:

| argument | what it names | when the user has given you one |
| --- | --- | --- |
| `--project-value <value>` | the value to scope to | "work through the workspace-ci stories" |
| `--project-field <name>` | which single-select to read it from | "work the **In Progress** stories" — a value on an axis other than the configured one |
| `--project-owner <login>` | the board's owner | the repository's config has no `select.project`, or the user named another board |
| `--project-number <n>` | the board's number | same |
| `--no-project-filter` | no project scope, whatever the config pins | "ignore the module, work the lot" |
| `--milestone <title>` | the milestone to drain | "drain v0.3.0" |
| `--no-milestone-filter` | no milestone, whatever the config pins | "ignore the milestone" |
| `--label <name>` | which label the backlog is | "work the **bug** backlog this time" |

A user asking to "work through the workspace-ci stories" is asking for `--project-value`
alone: the config already names the board and the usual axis. `--project-field` is what makes
"work the In Progress stories" expressible without an edit to `.claude/backlog.json`,
`--project-owner`/`--project-number` cover the repository whose config names no board at all,
and `--milestone` is what finally makes "drain the milestone" — advertised in this skill's own
description for a long time before it was expressible — an actual request.

### `all`

`all` means **every optional narrowing is dropped**: no milestone, no project scope, whatever
the config pins for either. It becomes `--all` on the selection call, one argument, and it
exists so the user does not have to know which axes this particular repository happens to
configure.

It is defined here because it was reached for before it existed. A `/run-backlog all`
invocation had to have a meaning invented for it at the driver — it was read as an iteration
bound and quietly defaulted to 10 — which is a request answered by guessing. `all` is not a
bound: `/run-backlog all` is ten iterations of the *unnarrowed* backlog, and
`/run-backlog 3 all` is three of it. It does not touch the label, which says what the backlog
is rather than narrowing it, so `all --label bug` is "the whole bug backlog" and is accepted.

**Every selection argument is the user's to give — infer none of them.** Do not read the
issues and guess a field, do not guess which field a value belongs to when the user names only
a value, and do not supply a milestone the user did not name. If a value is not one of the
configured field's options, or a milestone does not exist, selection fails at exit 4 naming
the real options or the real milestones — and that failure is the answer to report, not a
prompt to try another one.

With nothing given, pass nothing and let the config decide, which normally means the whole
backlog. Say the arguments in your first message alongside the bound — the field as well as
the value, since the same value can exist on two of a board's fields — because every outcome
below is then a statement about that selection and not about the backlog.

### `--reviewers`

`--reviewers copilot,local,none` replaces the repository's `review.reviewers` roster for this
run. It is not a selection argument — it goes to step 7 of each worker rather than to
`select-issue.sh` — but it is settled here with the others, and it is threaded into every
worker prompt on exactly the same terms.

It exists because a reviewer can be *unavailable through no fault of the configuration*, which
is what separates it from every selector above. A monthly Copilot allowance runs out on a
date nobody chose; an organisation has an outage. The configured roster is right for the
repository and wrong for this month, and the alternative to a per-run override is editing a
tracked file in every repository the operator runs the loop against — and then remembering to
edit it back.

Do not invent one. With nothing given, pass nothing: the repository's roster is the default
and it is usually correct. `--reviewers none` is the shape to be careful with — it is the
whole run merging unreviewed, so say so in your first message and again in the closing report,
rather than only in the per-iteration rows.

Check it before the first spawn, against the same three rules `scripts/select-issue.sh`
applies to the configured roster: every rung is one of `copilot`, `local` and `none`; the list
is not empty; and `none`, if present, is last, because no rung after it can ever run. A roster
that breaks one of those is a stop, not a thing to correct on the user's behalf — say which
rule and what they gave you. Checking it here rather than letting the first worker discover it
is the same reasoning that put the config's roster check at selection: a bad roster should
cost a sentence, not an implementation and a green pull request.

## 1. The loop

For each iteration up to the bound:

```
Agent(
  subagent_type: "issue-worker",
  description: "backlog iteration <i>",
  prompt: "Run one iteration of the backlog cycle in <absolute path to the repository root>.
           Invoke the backlog:next-issue skill and follow it exactly. Take exactly one issue
           end to end, then stop. Return the cycle's report as your final message, with its
           first line unchanged — BACKLOG EMPTY or BLOCKED where the skill calls for it."
)
```

When the run carries selection arguments, add one more line to the prompt, repeating **every**
one of them, verbatim and in full:

```
           This run is scoped: pass --project-field Status --project-value "In Progress"
           to select-issue.sh at step 1.
```

When the run carries `--reviewers`, add a line for it too, and it goes to step 7 rather than
to selection:

```
           This run's reviewer roster is --reviewers copilot,local,none. Use it at step 7 in
           place of review.reviewers.
```

And when earlier iterations found a rung **UNAVAILABLE**, name those rungs and their reasons —
see below for which ones qualify:

```
           These reviewer rungs already reported themselves UNAVAILABLE this run and are not
           worth probing again: copilot (Copilot code review is not enabled for this
           organisation).
```

Every iteration gets all of them, `all` (as `--all`) included. Dropping one does not narrow
that iteration:

- Drop `--project-value` and the iteration widens to the whole backlog.
- Drop `--project-field` and the iteration either falls back to the configured axis — a
  different dimension, quietly — or fails, depending on whether the value happens to be an
  option on that field too. The quiet one is the danger: the worker selects an out-of-scope
  issue and merges it, and nothing in its report will look wrong.
- Drop `--project-owner` or `--project-number` on a repository whose config names no board
  and the iteration fails at exit 4 — loud, but it burns an iteration on every pass.
- Drop `--milestone` or `--no-milestone-filter` and the iteration falls back to the
  configured milestone, which is the quiet failure again: the run works a milestone the user
  did not ask for, or reports the configured one empty and halts.
- Drop `--label` and the iteration works a different backlog entirely.
- Drop `--all` and every narrowing the user asked to be rid of comes back.

So the rule is the same for all of them, and it is the one already stated for the value: the
arguments you settled at preflight go into every prompt unchanged, or the run stops.

Check the agent types available to you before the first spawn. Depending on how the plugin
was installed the worker may be listed as `issue-worker` or as `backlog:issue-worker`; use
the name exactly as listed. If neither is present the plugin's `agents/` directory was not
picked up — stop and say so rather than falling back to a general-purpose agent, which will
not have the `Skill` tool and cannot invoke the cycle at all.

Give the worker the repository root as an **absolute path**. It runs with a working-directory
override, which is why `EnterWorktree` is unavailable to it and why `backlog:next-issue`
leads with plain `git worktree add`.

### After each iteration

Read the worker's final message and act on its first line:

| first line | do |
| --- | --- |
| `BACKLOG EMPTY` | **Halt.** The backlog holds no matching open issues. This is success, not failure. Under a scope it means *that scope* is drained, and the rest of the backlog is untouched — say which. |
| `BLOCKED …` | **Halt.** Report the worker's reason verbatim. |
| `IN FLIGHT …` | **Resume that same worker.** Its issue is unfinished and its pull request is open; do not start the next iteration. See below. |
| anything else | The iteration finished. Record a one-line outcome and continue to the next one. |

Also halt if:

- The worker returns nothing, errors, or is skipped. A silent worker is not an empty
  backlog, and treating it as one hides a broken plugin install behind a clean finish.
- Two consecutive iterations report the same issue number. That means the issue did not
  close — the cycle would re-implement merged work on every remaining iteration. Report it;
  the cause is usually a merge that did not land, or the explicit issue close being skipped.
- The bound is reached. Say so plainly: a loop that stopped at its bound with work left is a
  different outcome from a drained backlog, and the user's next move differs.

Record per iteration: issue number and title, pull request number, merged / blocked, **which
rung of the roster reviewed it**, how many times the worker had to be resumed, and the token
cost the agent result reports for that worker. Nothing else. Keep the running list short
enough that you can still see all of it at the end.

### Remembering a rung that is down, and not remembering one that is slow

A worker's report names any rung it found **UNAVAILABLE** — the gate's exit 3, which is the
rung saying it cannot review at all. That is a different outcome from a rung that **refused**
the work (exit 1) or one whose review the gate could not classify (exit 5, a `bot:<login>`
rung with no configured refusal wording); both of those halt the worker rather than being
carried anywhere. Accumulate the
unavailable ones across the run and thread them into every subsequent worker prompt, exactly
as you thread the selection arguments. The memory belongs here and nowhere else: every iteration is a fresh subagent, so
no worker can carry it, and a state file on disk would outlive the run and keep a rung retired
after its quota reset overnight.

Two failures read alike in a report and are not alike, and only the first is remembered:

- **The rung reported itself UNAVAILABLE.** Synchronous — the mutation, the REST fallback,
  the re-check, or, for the `local` rung, credentials absent from the environment at zero
  network cost. One to three seconds, and the rung *told us* it was down. Re-probing it across ten
  iterations costs about thirty seconds in total; not remembering it costs nothing much
  either, which is why this is cheap insurance rather than a load-bearing optimisation.
- **Nothing arrived.** Inferred from silence, after five minutes and four re-runs. Silence is
  also exactly what a slow-but-working reviewer looks like. Remembering this one retires a
  working reviewer for the rest of the run, and re-probing it costs three hours and twenty
  minutes across ten iterations — so it is re-probed every iteration anyway, and never
  recorded.

A worker that says a rung went silent is not reporting a rung that is down. If a report is
ambiguous, do not record the rung: the cost of an extra probe is seconds and the cost of a
wrong retirement is every remaining iteration's review quality.

### A worker that comes back mid-cycle

`IN FLIGHT` is the worker saying it handed control back with its issue unfinished: a pull
request open, a worktree on disk, the issue not closed. It is not an empty backlog, it is not
a block, and it is not a finished iteration. The three-branch table above used to have no row
for it, so the driver recorded an outcome and spawned the next iteration — **abandoning an
open pull request**, with nothing in the run pointing at what happened.

Resume the **same** worker. A fresh one would select a different issue and leave the first
one's pull request open for good:

```
SendMessage(
  <the worker's agent id or name>,
  "Continue the cycle from where you stopped. Nothing is running on your behalf — any wait
   from your last turn is over. Query the gate you had reached once, act on what it says,
   and finish the iteration."
)
```

A resume does **not** consume an iteration of the bound; no new issue was taken. It is not
free either — it re-costs the worker's whole context, 178–192k tokens on the runs this was
measured on — so allow **two** resumes for one worker. If it comes back `IN FLIGHT` a third
time, halt and report `BLOCKED` naming the issue, the pull request and the gate it could not
get past: something is wrong that a fourth nudge will not fix.

A worker that returns mid-cycle *without* the sentinel is the same situation with the label
missing. If a report names a pull request that never reached `MERGED` and does not begin
`BLOCKED`, treat it as `IN FLIGHT` — resume it — rather than as a completed iteration.

### What an iteration should cost

A worker that takes one story from selection to a merged pull request costs roughly
**300–375k tokens** and 300–390 assistant turns, at about a thousand tokens a turn. Three
consecutive iterations measured against a real repository came in at 370k / 346k / 405k.

An iteration far outside that band is almost always about *how it waited*, and there are two
**opposite** failures that land in the same place. Their remedies contradict each other, so it
is worth saying which one you saw rather than just that the iteration was expensive.

- **It watched the wait.** The worker filled the time while a wait was outstanding with no-op
  `sleep` calls and re-reads of the wait's output file — 106 turns out of 419 on one measured
  iteration, about 100k tokens, a quarter of it, to buy under a minute of real waiting. The
  iterations either side spent 0 and 2 turns that way on comparable work. Signature: a very
  high turn count for the work done, inside a single worker. Remedy: **fewer** turns.
- **It stopped dead at the wait.** The worker armed a wait that did not survive the turn
  boundary, handed control back, and on resume held for an event that could never arrive. One
  measured resume made **zero tool calls in 6.5 seconds** while CI had been green for some
  time; that iteration needed thirteen resume nudges at 178–192k tokens each — about 2.4M
  tokens — and it did eventually merge, which is what makes it easy to miss. Signature: many
  resumes, each costing a full context, several of them doing almost nothing. Remedy: the
  opposite one — **act**, with a single direct status query, exactly where the first failure
  says to do nothing.

Both are departures from the cycle. `backlog:next-issue` step 6 addresses each by name, and
the second is why `Monitor` is not in the worker's tools at all. Name whichever the table
shows: the iteration count and the outcomes look identical either way, and the cost and resume
columns are the only place a run that regressed is visible.

## 2. What you never do yourself

- **Never run `gh pr merge`**, and never work an issue yourself when a worker fails. Beyond
  the permission classifier refusing an agent-driven merge, a subagent that merges emits a
  security banner into its caller's context which blocks the caller's next agent spawn.
  That is your context and your next spawn: the loop dies on the *following* iteration, with
  nothing at that point pointing back at the merge. If a worker's report suggests it merged
  by hand, halt and say so — the next spawn is likely to fail regardless.
- **Never implement the issue in your own context** when the worker reports `BLOCKED`. The
  block is a request for a human, which is the one thing a loop cannot supply.
- **Never spawn the next iteration over an open pull request.** A worker that came back with
  its issue unfinished gets resumed, not replaced. The new worker would take a different
  issue, and the first one's pull request, worktree and open issue would be left behind with
  no one holding them.
- **Never widen the bound mid-run** because the backlog turned out to be longer. Finish,
  report, and let the user re-run.
- **Never reorder or shorten the reviewer roster**, and never add `none` to it because a rung
  went quiet. The order is the operator's policy about what assurance the default branch gets,
  and a run that appends `none` to keep moving has decided that on their behalf. A roster that
  runs out is a `BLOCKED` worker and a halted loop, which is the correct outcome.
- **Never drop, widen or edit any selection argument mid-run**, and never respond to a worker
  that halted on a selection failure by re-spawning it with an argument removed or changed.
  Dropping the value turns "work the workspace-ci stories" into "work the backlog"; dropping
  or changing the field turns it into "work some other dimension" — which is worse, because it
  still looks scoped in the report; dropping the milestone works a release the user did not
  name. All of them are the failure a scoped run exists to prevent.
- **Never edit `.claude/backlog.json` to make a selection resolve.** Every selector has an
  argument, so a selection that will not assemble — an unresolvable scope, a milestone that no
  longer exists — is a missing argument on this run and, at most, an issue to file against the
  repository whose config went stale. Rewriting the repository's committed description of its
  own backlog to suit one run outlives the run.

## 3. Report

Close with the iteration table, the selection arguments and the reviewer roster the run was
under if it had either, the reason the loop stopped in its own words (`BACKLOG EMPTY`,
`BLOCKED`, bound reached, worker failure, a worker that could not be got past `IN FLIGHT`),
every rung found unavailable during the run with its reason, and — if anything merged because
the roster reached `none` — that fact, once, for the whole run.

The table carries the token cost per iteration and the rung that reviewed each one, so keep
both columns in the closing report rather than summarising them away. The cost column is what
makes the next run comparable to this one, and the only signal that separates an expensive
iteration from a busy-waiting one; the reviewer column is what shows a run that quietly
degraded from `copilot` to `local` halfway through, which the outcomes alone look identical
under.

## Running it on a schedule

`/loop` is the usual driver: it re-invokes this skill on an interval, and each invocation
does its own bounded pass. Keep the bound small when looping, so the two levels of
repetition do not multiply into an unattended run nobody has read the start of.

The permission allow-list `backlog:setup-backlog` offers is what lets an unattended run
proceed without a prompt on every `gh` call. Without it the loop stalls on the first
permission dialog and looks like a hang.
