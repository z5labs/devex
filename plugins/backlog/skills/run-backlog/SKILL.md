---
name: run-backlog
description: Drive a repository's story backlog unattended, one issue at a time, by spawning a fresh `issue-worker` subagent per iteration and halting on `BACKLOG EMPTY`, on `BLOCKED`, or at a bounded iteration count. Use this whenever the user wants the backlog worked continuously rather than a single issue — "run the backlog", "work through the issues", "keep taking stories until you run out", "drain the milestone", or a `/loop` that repeats the cycle. Takes an optional integer argument setting the maximum number of iterations. Skip this when the user wants exactly one issue (`backlog:next-issue`) or wants the repository bootstrapped first (`backlog:setup-backlog`).
allowed-tools: Agent, Bash, Read, Glob, Grep, TaskCreate, TaskUpdate, ScheduleWakeup
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
   opaque failure.

Also settle the **bound** now. It is the skill's argument when one was given; otherwise
default to **10** iterations. Say the number in your first message. An unbounded loop
against a large backlog is not the user asking for autonomy, it is the user losing the
chance to look at the first result before the twentieth lands on top of it.

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
| `BACKLOG EMPTY` | **Halt.** The backlog holds no matching open issues. This is success, not failure. |
| `BLOCKED …` | **Halt.** Report the worker's reason verbatim. |
| anything else | Record a one-line outcome and continue to the next iteration. |

Also halt if:

- The worker returns nothing, errors, or is skipped. A silent worker is not an empty
  backlog, and treating it as one hides a broken plugin install behind a clean finish.
- Two consecutive iterations report the same issue number. That means the issue did not
  close — the cycle would re-implement merged work on every remaining iteration. Report it;
  the cause is usually a merge that did not land, or the explicit issue close being skipped.
- The bound is reached. Say so plainly: a loop that stopped at its bound with work left is a
  different outcome from a drained backlog, and the user's next move differs.

Record per iteration: issue number and title, pull request number, and merged / blocked.
Nothing else. Keep the running list short enough that you can still see all of it at the
end.

## 2. What you never do yourself

- **Never run `gh pr merge`**, and never work an issue yourself when a worker fails. Beyond
  the permission classifier refusing an agent-driven merge, a subagent that merges emits a
  security banner into its caller's context which blocks the caller's next agent spawn.
  That is your context and your next spawn: the loop dies on the *following* iteration, with
  nothing at that point pointing back at the merge. If a worker's report suggests it merged
  by hand, halt and say so — the next spawn is likely to fail regardless.
- **Never implement the issue in your own context** when the worker reports `BLOCKED`. The
  block is a request for a human, which is the one thing a loop cannot supply.
- **Never widen the bound mid-run** because the backlog turned out to be longer. Finish,
  report, and let the user re-run.

## 3. Report

Close with the iteration table, the reason the loop stopped in its own words
(`BACKLOG EMPTY`, `BLOCKED`, bound reached, worker failure), and — if anything merged
without a review because `review.required` is `false` — that fact, once, for the whole run.

## Running it on a schedule

`/loop` is the usual driver: it re-invokes this skill on an interval, and each invocation
does its own bounded pass. Keep the bound small when looping, so the two levels of
repetition do not multiply into an unattended run nobody has read the start of.

The permission allow-list `backlog:setup-backlog` offers is what lets an unattended run
proceed without a prompt on every `gh` call. Without it the loop stalls on the first
permission dialog and looks like a hang.
