---
name: next-issue
description: Take exactly one eligible story issue from the backlog through the full development cycle — select it, branch a worktree, implement the acceptance criteria, run the repository's verify commands, open a pull request, wait for checks, get a review from the configured roster and answer it, then label the pull request for GitHub to auto merge, close the issue and clean up. Use this whenever the user asks to "work the backlog", "take the next issue", "do the next story", "pick up the next ticket", or runs the backlog loop; it is also what the `issue-worker` agent invokes on every iteration of `backlog:run-backlog`. Everything repository-specific comes from `.claude/backlog.json`. Skip this when the user names a specific issue to explore rather than to land, or when they want the backlog bootstrapped rather than worked — that is `backlog:setup-backlog`.
allowed-tools: Agent, Bash, Read, Write, Edit, Glob, Grep, TaskCreate, TaskUpdate, EnterWorktree, ExitWorktree
---

# next-issue

Take **exactly one** issue from backlog to a merged pull request, then stop. Do not start a
second issue in the same invocation — the loop re-invokes this skill for the next one.

You never run `gh pr merge`. You label the pull request and GitHub merges it, gated by the
default branch's protection rules; see step 9.

Nothing about the repository is written into this skill. The repository slug and default
branch are read from the repository itself (step 0); the label, milestone, optional project
scope, dependency convention, verify commands, merge label and worktree directory are read
from `.claude/backlog.json` — where every selector among them is a **default** that an
argument to step 1 can replace for this run alone.

## 0. Load the configuration

Two reads, in this order. Both must succeed before anything else happens.

**The config.** `.claude/backlog.json`, relative to the repository root:

```
jq . .claude/backlog.json
```

If the file is absent, or `jq` exits non-zero on it, **stop**. Report, beginning with
`BLOCKED`:

> `BLOCKED` — `.claude/backlog.json` is missing (or does not parse). Run
> `backlog:setup-backlog` to create it.

Do not invent defaults and carry on. A guessed label selects the wrong issues and a guessed
verify list opens red pull requests, and both failures look like the repository's fault
rather than the config's.

What each key is used for:

| key | used at |
| --- | --- |
| `select.label`, `select.milestone`, `select.limit`, `select.project` | step 1 — read by `select-issue.sh`, not by you, and each one a **default** a flag on that call can override |
| `dependencies.style` | step 1 — same, minus the override |
| `verify` | step 4, the commands that gate the pull request |
| `merge.label`, `merge.workflow` | step 9, handing the merge to GitHub |
| `review.reviewers` | steps 7 and 8, the ordered roster of reviewers that gates the merge |
| `worktreeDir` | steps 2 and 10, where the worktree lives |

Read the file for the four keys you use directly. The first three rows belong to step 1's
script, which re-reads and validates them itself — including rejecting an empty `verify`,
which parses fine and would otherwise surface as a pull request nothing local ever checked.

`${CLAUDE_PLUGIN_ROOT}/assets/backlog.schema.json` is the schema; read it if a key looks
wrong.

**The repository.** Slug and default branch come from GitHub, not from the config — a fork
or a rename must not be able to leave a stale slug pointing the cycle at someone else's
repository:

```
read -r REPO DEFAULT_BRANCH <<<"$(gh repo view --json nameWithOwner,defaultBranchRef \
  --jq '"\(.nameWithOwner) \(.defaultBranchRef.name)"')"
```

Shell state does not survive between Bash calls, so those two variables are gone by your
next command. Note the values down and substitute them literally into the commands below,
or re-derive them inside the same command. Every `<repo>` and `<default-branch>` in this
document is one of those two values.

## 1. Pick the issue — one call

Selection carries no judgment: given the label, the milestone, the limit and the dependency
convention, the answer is a function of the backlog. So it is one call, and the answer is an
exit code:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/select-issue.sh"
```

| exit | stdout | what you do |
| --- | --- | --- |
| 0 | `{"number":N,"title":"…"}` | that is your issue — continue to step 2 |
| 10 | `BACKLOG EMPTY` | print that line and stop. Do nothing else. |
| 11 | `BLOCKED — …`, then a line per issue naming what holds it | print it and stop |
| 4 | a message naming the problem | `BLOCKED`. The config or the environment is wrong, not the backlog — usually `.claude/backlog.json` is missing, unparseable, carries an unknown `dependencies.style`, or has an empty `verify`. A requested project scope that could not be resolved lands here too, as does a milestone that does not exist; see below. Point at `backlog:setup-backlog`. |

The script walks candidates in ascending number order and takes the first whose every
declared dependency is `CLOSED`. Its per-candidate reasoning goes to stderr, so a selection
you did not expect can be explained without re-running anything.

### Every selector is a runtime decision

`.claude/backlog.json` holds the **default** for each selector and nothing more. Every one of
them can be replaced for a single run by an argument to the call above:

| argument | overrides | when you pass it |
| --- | --- | --- |
| `--label <name>` | `select.label` | you were asked to work a different label this run — `bug` on a repository whose backlog is normally `story` |
| `--milestone <title>` | `select.milestone` | you were asked to drain a named milestone |
| `--no-milestone-filter` | same, to nothing | you were asked to ignore the configured milestone |
| `--project-value`, `--project-field`, `--project-owner`, `--project-number`, `--no-project-filter` | `select.project.*` | see the next section |
| `--all` | the milestone **and** the project scope, both to nothing | you were asked for the whole labelled backlog, whatever narrowings this repository happens to configure |
| `--issue <n>` | selection itself | you were asked to work one named issue rather than the next one |

Pass an argument when, and only when, you were asked for what it expresses — the user named
it, or `backlog:run-backlog` put it in your prompt. **Never infer one.** With nothing asked
for, call the script with no arguments at all and let the config decide.

Two rules the script enforces, both worth knowing before you are refused:

- **A clearing argument never combines with a narrowing one.** `--milestone X
  --no-milestone-filter` is exit 4, as `--no-project-filter` beside any `--project-*` flag
  already was, and so is `--all` beside any of them: each pair would have to discard one of
  the two silently, and either discard is a run that is not the run you were asked for.
  `--all --label bug` is fine, because the label defines the backlog rather than narrowing
  it.
- **`--issue <n>` combines with nothing.** It bypasses the label, the milestone and the
  project scope, and its stderr says so, one line per narrowing — a run that selected an
  out-of-backlog issue has to read differently from one that searched and found it at the
  top. The dependency walk still runs against it: an issue whose blockers are open comes back
  exit 11 `BLOCKED` naming them, never selected.

A **milestone that does not exist** is exit 4 naming the ones that do, whether it came from
the flag or from the config. That check is there because `gh issue list --milestone` answers
`[]` with exit 0 for a milestone that was deleted or renamed, and an empty candidate list
prints `BACKLOG EMPTY` — which halts the loop as a success over a backlog that is fully
workable. If that fires, the answer is a `--milestone` (or `--no-milestone-filter`) argument
on this run and an issue against the repository whose config went stale; it is **not** a
reason to edit `.claude/backlog.json` yourself.

### Scoping the run to one project field value

Some repositories group work by a single-select field on a GitHub project — `Module`, `Area`,
`Component`, `Status` — rather than by labels or milestones. One run can be restricted to a
single value of one such field:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/select-issue.sh" --project-value <value>
```

A scope is four things, and each one has a flag that overrides `.claude/backlog.json` for
this run only:

| flag | overrides | when you pass it |
| --- | --- | --- |
| `--project-value <value>` | `select.project.value` | you were asked for a scoped run |
| `--project-field <name>` | `select.project.field` | you were asked to scope by an axis other than the configured one — `Status` where the config says `Module` |
| `--project-owner <login>` | `select.project.owner` | the config has no `select.project` at all, or names a different board |
| `--project-number <n>` | `select.project.number` | same |

The usual scoped call is `--project-value` alone: the config already names the board and the
axis, and only the value is a property of this run. `--project-field` is the next most common,
because a board routinely carries more than one single-select worth scoping by. `--project-owner`
and `--project-number` are for the repository whose config carries no `select.project` — with
them a complete scope can be assembled on the command line, so scoping never requires editing
a tracked file.

Pass a flag when, and only when, you were asked for that scope — the user named it, or
`backlog:run-backlog` put it in your prompt. **Never infer one from the issues you can see.**
With nothing asked for, call the script with no arguments and let the config decide, which
normally means `value` is `null` and the whole backlog is in play. `--no-project-filter` is
the other direction: run unscoped even though the config pins a value. It cannot be combined
with any of the four flags above — "unscoped" and "scoped like this" is not a request the
script will guess at.

The scope is applied before the dependency walk, so the walk sees only in-scope issues and a
blocker outside the scope cannot hold up the one you were asked to work.

Three things to know when it fails:

- It needs the **`read:project`** token scope, which `repo` does not include, and it needs it
  however the scope was assembled — a scope built entirely from flags reads the same API.
  `gh auth refresh -s read:project` grants it.
- Every project failure is exit 4 — a token without that scope, an unknown project, a field
  that does not exist or is not a single-select, a value that is not one of the field's
  options, an axis named with no value to match on it. **Do not retry without the flag.**
  Selecting from the whole backlog when a scope was asked for gets work done in the wrong
  order, which is worse than selecting nothing; report `BLOCKED` with the message.
- A scope that cannot be assembled names the piece that is missing and both places it could
  have come from — `missing: field (select.project.field or --project-field)`. Supply it on
  the command line. Do not edit `.claude/backlog.json` to get past it: the config describes
  the repository's backlog, and a run is not entitled to rewrite that to describe itself.

Exit 10 under a scope is not a failure — the scope is genuinely drained. Say which scope in
the report, so a finished module reads differently from a finished backlog.

**Do not reconstruct any of this by hand**, and do not fall back to `gh issue list` plus your
own body parsing if the call fails. That fallback is where this step's history lives: the
extraction used to be three `awk`/`sed`/`grep` pipelines written out here for you to retype,
and every one of them was wrong — a sentence reading `this issue is not blocked by anything`
opened a dependency list, a fenced code block quoting the convention counted as a
declaration, GitHub's `- [ ] #12` task-list form extracted nothing, and a cross-repository
`owner/repo#N` silently terminated the list, dropping the real dependencies below it.
`scripts/select-issue_test.sh` is the fixture corpus those came from; a change to the rules
belongs there, not here.

### The conventions it reads

Useful when you are *writing* an issue body, and what `backlog:setup-backlog` infers
`dependencies.style` from. Two of the four styles read the body, `native` reads GitHub's
typed dependencies instead, and `none` reads neither — which one applies is whatever
`.claude/backlog.json` declares. **There is no fallback between them in either direction**;
see `native` below for why.

**`blocked-by`** — a line that *opens* with `Blocked by:` (a heading, bold, or plain), then
`- #N` list items. The phrase has to start the line and end at a colon or the line end, which
is what tells a declaration apart from prose that merely mentions being blocked. References
on the label line itself count too, so `Blocked by: #12, #14` works. The list ends at the
first non-blank line that is not a list item — a blank line does not end it, because Markdown
calls a bullet list with blank lines between its items a single loose list. On each item the
**first** reference is the dependency and the rest is a gloss, so `- #12 — needs #34 first`
contributes 12 and not 34.

**`depends-on`** — `Depends on #N` written inline, conventionally under a `Related Issues`
heading. The reference has to follow the phrase immediately, so `Depends on the parser
landing, see #99` is not a dependency on 99. A phrase negated in the same clause — `no longer
depends on #17` — is a note that a dependency was removed, not a live one.

**`native`** — GitHub's own typed issue dependencies, the `blockedBy` edges you write with
`gh issue edit <n> --add-blocked-by <n>`. The body is not read at all under this style. A
typed edge cannot be written ambiguously, it renders as a real relationship on the issue, and
it survives a rewording of the body — so where a repository has populated them, they are
strictly better than anything parsed out of prose.

The reason this is **declared and never fallen back to**: an unpopulated edge set and an
unblocked issue are the same response. A repository that writes its ordering in prose and has
never touched typed dependencies would come back "nothing blocks this" for every issue —
which reads as an unblocked backlog and is the one wrong answer that gets work done in the
wrong order rather than not at all. So a body parse finding nothing never escalates to
`native`, and `native` coming back empty never falls back to reading the body. Each style
answers wrongly for a repository using the other, and the config is what says which. When
you *write* an issue for a `native` repository, add the edge with `gh issue edit`; a
`Blocked by:` line in the body is decoration there and blocks nothing.

**`none`** — the backlog declares no ordering. Bodies are not parsed at all; every open issue
carrying the label is eligible and the lowest-numbered one wins.

Under both parsing styles, text inside a ``` or `~~~` fence is example text, not a
declaration. A bare `#N` means an issue in this repository and `owner/repo#N` one anywhere
else; under all three declaring styles both are resolved the same way, against the repository
the reference names, and both are satisfied when that issue is CLOSED. A cross-repository
dependency therefore decays when its blocker lands, exactly like one here — which is what
makes a story in one repository able to wait on work in another.

A dependency whose state cannot be *read* — a private repository, a token without the scope,
a deleted issue — is `UNREADABLE` and stays a blocker. That makes *that issue* ineligible and
is named in the report, rather than being skipped and the issue called eligible, but the walk
continues, so one unreadable reference does not starve a backlog whose other issues are
workable. Under `native` a dependency query that fails outright is treated the same way: that
issue is ineligible with the error recorded, never eligible-by-default.

### Read the issue

Read the whole issue body. The **Acceptance Criteria** checklist is the spec — every box
must be genuinely satisfied before you open the pull request.

## 2. Worktree

Branch a fresh worktree from the remote default branch:

```
git fetch origin
git worktree add -b issue-<n> <worktreeDir>/issue-<n> origin/<default-branch>
```

Then work by absolute path inside that directory. Confirm with
`git -C <worktreeDir>/issue-<n> rev-parse --abbrev-ref HEAD` and
`git -C <worktreeDir>/issue-<n> log --oneline -3`.

Branch from `origin/<default-branch>`, not from the local one: the main checkout is
routinely behind, because earlier iterations merge through GitHub rather than locally, and
branching from a stale local ref reimplements work that already landed or conflicts with
it.

Never commit, branch, or open a pull request from the main checkout — the directory holding
`<worktreeDir>/`, which `git worktree list` reports first.

Avoid bare `git stash` and `git stash pop`. The stash stack is shared across every worktree
of the repository, so a pop can restore work belonging to another session entirely. Set
work aside with a temporary commit instead.

> Running this interactively, outside the loop, `EnterWorktree(name: "issue-<n>")` and
> `ExitWorktree(action: "remove")` do the same job with less typing. They are unavailable
> to a subagent running with a working-directory override, which is exactly how
> `backlog:run-backlog` invokes this skill — so the git commands above are the path that
> actually runs, and the tools are the convenience.

If the repository root has a `CLAUDE.md`, read it now, along with any `CLAUDE.md` in the
package you are about to touch. It holds the implementation conventions this repository's
issues assume.

## 3. Implement

Work through the acceptance criteria in order. Follow the conventions already established
in the repository — the package layout, naming and test style of code that has already
landed — rather than inventing new ones. Where the issue names a structural model, mirror
that.

Write tests alongside the implementation, in the style the repository already uses.

## 4. Verify locally

Run every command in `verify`, in order, from inside the worktree. All must pass before you
go further. This is the same set CI runs, so a local failure is a guaranteed red pull
request:

```
<verify[0]>
<verify[1]>
...
```

Some verify commands report a problem by printing rather than by exiting non-zero — a
formatter listing the files it would rewrite is the usual case. For those, **non-empty
output is the failure.** Fix what it lists rather than reading the exit code and moving on.

If a test fails, fix the code — never weaken the test to make it pass. If the acceptance
criteria and a passing test genuinely conflict, stop and report rather than guessing.

A verify command that fails because the *tool* is stale rather than because the code is
wrong is not a code defect; fix the tool and say so in the report rather than editing
around it.

## 5. Commit and open the pull request

```
git add -A
git commit -m "<type>(<scope>): <summary>"   # match the issue title's prefix
git push -u origin HEAD
gh pr create --title "<issue title>" --body "<body>"
```

The pull request body must include `Closes #<n>`, the acceptance criteria checked off one by
one, a note on any judgment call that shaped the public API, and how it was verified. Keep
the standard attribution your harness adds to commits and pull request bodies.

Then assert that the closing reference actually registered:

```
gh pr view <pr> --repo <repo> --json closingIssuesReferences \
  --jq '[.closingIssuesReferences[].number]'
```

It must contain `<n>`. An empty array means GitHub did not parse the keyword — usually
because the line is missing, misspelled, or names a cross-repository issue — and the link is
established at creation time, so fixing the body later is the moment to re-check.

This check costs one call and it is diagnostic, not preventive. It is expected to *pass* and
for the issue to stay open anyway after the merge: the reference registers correctly and
nothing acts on it when a workflow's `GITHUB_TOKEN` performs the merge, which is why step 10
closes the issue explicitly. Running it here is what separates those two failures — a link
that never formed from a link nothing honoured — instead of leaving them to look identical
ten minutes later.

## 6. Wait for checks — one call, and then another if it is not finished

The wait is an ordinary **blocking `Bash` call**. Its result comes back in that same call:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/await-checks.sh" <pr>
```

Give the Bash tool a `timeout` of `330000`. The script bounds itself at five minutes, so that
number only has to outlive it.

| exit | meaning | what you do |
| --- | --- | --- |
| 0 | every check has settled and none failed | continue to step 7 |
| 1 | a check failed or was cancelled; stdout names it with its link | read the logs (`gh run view <id> --log-failed`), fix, push, and wait again. After **three** failed attempts on the same root cause, stop and report instead of looping |
| 2 | still running when the five minutes were up | **run the same command again.** Nothing failed and nothing was lost — the script only reads GitHub. Each re-run is one more tool call in the turn you are already in. After six consecutive 2s — half an hour of checks that will not settle — stop and report `BLOCKED` naming the pull request |
| 3 | no checks were ever reported | nothing gates this pull request. Record `no checks reported` in the report and continue — do not silently treat it as a pass. This is the same gap `backlog:setup-backlog` reports when the default branch has no required status check, seen from the other side: with no required check, the label in step 9 merges the pull request the instant it lands |
| 4 | usage or precondition failure | `BLOCKED`; the plugin or the environment is wrong, not the pull request |

Exit 2 is the normal case on a repository with real CI, and it is **not** a failure to be
reported, retried differently, or worked around. It is the wait saying "ask again".

The verdict the script gives is not one you should second-guess by reading the checks
yourself: a failed check beside a still-pending one is exit 1, not exit 2, and a `skipping`
check is settled rather than outstanding. `scripts/await-checks_test.sh` is the offline
fixture corpus for those rules; a change to them belongs there, not here.

### How to wait — and the three ways not to

This is the first of the cycle's three long waits; step 7 and step 10 have the same shape and
the same exit-2 contract. One rule covers all three: **the wait's result must come back inside
the call that started it.** Everything below follows from that.

- **Not `Monitor`.** A `Monitor` call reports through notifications that arrive *after* the
  turn that armed it. Run interactively, that works. Run the way this cycle almost always runs
  — as `backlog:run-backlog`'s `issue-worker` subagent — and it deadlocks: your next message
  is your **return value**, so ending the turn with a wait armed ends the agent, and the wait
  dies with it. You are then resumed believing a wait is live, hold for an event that can never
  arrive, and do nothing. Measured on a real run: one iteration needed thirteen resume nudges
  at 178–192k tokens each — about 2.4M tokens against a 300–375k band — and one of those
  resumes made zero tool calls in 6.5 seconds while CI had been green for some time. `Monitor`
  is not in this skill's `allowed-tools`, and that is the fix rather than a restriction to
  route around.
- **Not `run_in_background`.** Same defect — the result lands after the turn — plus one of its
  own: it has been observed exiting immediately without ever polling, reporting whatever the
  first sample happened to be as though the wait had completed.
- **Not turns of your own.** No no-op `sleep` Bash calls to pass the time, and no peeking:
  do not `Read`, `cat` or `tail` a log, output file or task record to see how a wait is
  getting on, and do not re-run the underlying `gh` command by hand alongside one. A blocking
  call leaves nothing to peek at while it runs, so what this forbids is the temptation *after*
  an exit 2 — polling by hand instead of simply calling the wait again. One measured iteration
  spent 106 turns, about a quarter of its entire token cost, on that loop to buy under a
  minute of real waiting; the two iterations either side of it spent 0 and 2.

Where a wait needs a precondition, the precondition goes **inside** the waiting command, never
into a turn of yours — `until <precondition>; do sleep 5; done; <blocking command>` is the
sanctioned shape, and the three scripts already do it for you. That is also why the `Bash`
tool's ban on foreground `sleep` is not violated here: the sleeps are inside a bounded command
that is doing the waiting, not calls of yours that pass the time.

### If you are resumed mid-cycle

You should never hand control back with a gate unresolved — the blocking calls above are what
make that avoidable. If it happens anyway and you find yourself resumed partway through the
cycle, then **nothing is running on your behalf.** Any wait from an earlier turn has already
returned or died with that turn; there is no event still coming, and no amount of holding will
produce one.

So do not hold, and do not report that you are waiting. Make **one** direct status query for
the gate you had reached —

```
"${CLAUDE_PLUGIN_ROOT}/scripts/await-checks.sh" <pr>          # step 6
"${CLAUDE_PLUGIN_ROOT}/scripts/await-review.sh" <pr> <rung>   # step 7 — idempotent
gh pr view <pr> --repo <repo> --json state,autoMergeRequest   # step 9
"${CLAUDE_PLUGIN_ROOT}/scripts/finish-issue.sh" <n> <pr> issue-<n>   # step 10 — guarded
```

The three scripts are the query as well as the wait: each reads the current state first and
returns immediately when the gate has already settled, so re-entering one costs a moment, not
five minutes.

— act on what it says, and if the gate is genuinely still pending, start the wait again with
the blocking call. The no-polling rule above is about not polling *alongside* a live wait;
with no wait running, that single query is exactly the right move, and doing nothing is the
one response that cannot be right.

## 7. The review — walk the roster

`review.reviewers` is an **ordered roster**, tried in order and failing over on availability:
`["copilot"]`, `["copilot", "local"]`, `["copilot", "local", "none"]`. `select-issue.sh`
already validated it at step 1, so by the time you are here the rungs are known and `none`,
if present, is last.

Two things can override the configured roster, and both come from your prompt rather than
from the repository:

- `--reviewers <a,b,c>` replaces the roster for this run. `backlog:run-backlog` passes it when
  a month's Copilot allowance has run out and editing a tracked file in every repository is
  not the remedy. It is not yours to widen, reorder or drop.
- A list of rungs **already found UNAVAILABLE earlier in this run**. Skip those without
  probing them: the driver only carries forward rungs that reported themselves unavailable
  synchronously, and re-probing one costs a few seconds per iteration while re-waiting on one
  costs twenty minutes.

**If the first rung you have left is `none`, skip this step and step 8 entirely** and go to
step 9. Nothing here is optional for any other rung.

### The distinction the whole roster turns on

**Unavailability advances the roster. Refusal does not.**

A rung that reviewed the pull request and *refused* it — Copilot's `"wasn't able to review
this pull request because it exceeds the maximum number of files (300)"` — has said something
true about the work. Advancing past that to a reviewer with different limits is exactly how
unreviewable work reaches the default branch, which is the failure the decline check was
added for: a vendored test suite pushed a pull request past the file limit, Copilot declined,
a naive `length > 0` check passed, and the cycle merged with no review at all. So a refusal is
`BLOCKED` and the roster stops. This is the conservative reading, chosen deliberately.

### `copilot`

One blocking `Bash` call, the same shape as step 6's, with the Bash tool's `timeout` set to
`330000` and the script bounding itself at five minutes:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/await-review.sh" <pr> copilot
```

This is the longest wait in the cycle and the one that has attracted busy-waiting. Step 6's
rules hold here unchanged: not `Monitor`, not `run_in_background`, and not turns of your own.

### `local`

An adversarial review, produced by a fresh subagent that has never seen your implementation,
and posted to the pull request under the backlog App's identity. Four steps, in this order:

**1. Preflight, before anything is spawned.**

```
"${CLAUDE_PLUGIN_ROOT}/scripts/post-review.sh" --preflight
```

Exit 3 means the rung is unavailable — `BACKLOG_APP_ID`/`BACKLOG_APP_KEY` absent from the
environment, or `openssl` not on PATH. That is a fallthrough, not a crash, and it costs no
network call: advance the roster and record the rung. The preflight is first because
everything after it costs a fresh agent and a whole diff, and learning afterwards that the
App was never configured is the expensive way to learn it.

If the `Agent` tool is not available to you, the rung is unavailable too. Record it and
advance — **do not review your own diff.** A reviewer holding the context that produced the
code is not a second opinion, and a review it posts under an App identity would look exactly
like one.

**2. Spawn the reviewer.** A `general-purpose` subagent, given the diff and nothing else:

```
gh pr diff <pr> > /tmp/pr-<pr>.diff
```

Its prompt gives it the diff, the pull request title and the issue's acceptance criteria, and
says plainly that it must not read the repository for the code's rationale — the point of the
rung is a reader who was not there. Ask it to return **only** JSON:

```json
{"body": "<markdown summary>",
 "comments": [{"path": "a/b.go", "line": 42, "side": "RIGHT", "body": "<finding>"}]}
```

Tell it to look for correctness defects, missing test coverage of the acceptance criteria,
error paths that swallow failures, and public API it would find hard to use — and to say so
in the summary when it found nothing, rather than inventing a finding. And tell it that if it
cannot review the diff at all, its `body` must say **"I was not able to review this pull
request"** and why: that is the wording the gate classifies as a refusal, and a rung that
cannot say "I refuse" in a form the gate recognises is a rung whose refusals merge.

**3. Post it.**

```
"${CLAUDE_PLUGIN_ROOT}/scripts/post-review.sh" <pr> <body-file> <comments-file>
```

Exit 3 is unavailable (advance and record). Exit 1 is a post that failed for a reason that is
not availability — advance the roster, but do **not** record the rung as down; nothing about
the rung is broken.

**4. Wait on it through the same gate as every other rung.**

```
"${CLAUDE_PLUGIN_ROOT}/scripts/await-review.sh" <pr> local
```

That last call is not ceremony. A posted review is a `reviewed` event on the timeline, which
is what the gate polls, so **one exit code covers every rung** and "was this reviewed?" never
becomes something you assert at the end of the longest part of the cycle.

### The exit table — identical for both rungs

| exit | meaning | what you do |
| --- | --- | --- |
| 0 | a review **completed** — it left comments, or reported it generated none. stdout is its body | continue to step 8. Note which rung it was; your report names it |
| 1 | the most recent review **REFUSED** the work; stdout is why | `BLOCKED`, and do **not** label and do **not** advance the roster. If it is the 300-file limit, say so and suggest how the work could be split |
| 2 | the bound was up with no review yet | **run it again**, in the same turn. It resumes: a review already requested is not requested twice, and one already posted is classified immediately. After **four** re-runs — about twenty minutes with nothing posted — this rung is unavailable *for this pull request*: advance the roster. Do **not** report it as having refused, and do not tell the driver to retire it. Silence is exactly what a slow-but-working reviewer looks like |
| 3 | **UNAVAILABLE**, synchronously — the request was refused, or the rung's preconditions are absent | advance the roster, and **record the rung** with the reason. Your report names it, and the driver carries it into the next iteration so ten iterations do not each spend a minute rediscovering it |
| 4 | usage or precondition failure | `BLOCKED`; the plugin or the environment is wrong, not the pull request |

**Only exit 0 lets you label the pull request in step 9** — on some rung, or the roster
reaching `none`.

### Reaching the end of the roster

If every rung is exhausted and the last one was **not** `none`, that is `BLOCKED`. Name each
rung tried and why each was unavailable, so the reader can tell "Copilot's quota is gone" from
"the App is not installed here" without opening the run.

If the last rung **is** `none`, the pull request merges unreviewed. That is legal — the
operator wrote it into the roster — and it is not silent: step 9 labels, and your report says
plainly that the merge was unreviewed and names every rung that was tried and why each failed.

### Why the gate is a script

"Did the reviewer review this?" looks like one question and is two, and the cycle used to
answer them in places far enough apart that the second could be skipped. The wait was a
polling loop that exited on the first `reviewed` event; whether that review had *declined* the
work was a separate command sixty lines further down, under its own heading. Copilot posts a
review whose body declines, and that decline satisfies any `length > 0` test. It has been
missed in the wild, exactly as described above.

The label in step 9 **is** the assertion that a review completed. Making it an exit code is
what stops that assertion depending on an agent remembering a second question at the very end
of the longest part of the cycle.

Findings that live inside the script rather than in prose here, each of which cost a debugging
session:

- Copilot is a **Bot**, not a User. The GraphQL mutation takes `botIds`; a bare
  `reviewers[]=Copilot` on the REST endpoint returns 200 while doing nothing, and the wait
  then times out on a review that was never requested. REST works only with the full
  `copilot-pull-request-reviewer[bot]` login. The bot's node ID is looked up rather than
  hard-coded.
- The wait polls the **timeline**, not `pulls/<pr>/reviews`. The reviews endpoint has been
  seen empty for forty minutes after Copilot had in fact submitted, so polling it times out
  on a pull request that *was* reviewed. That is why the polling lives in the script and is
  aimed at the timeline; a check of your own, against whichever endpoint, is the mistake this
  finding is made of.
- `--paginate`, because the timeline returns thirty events per page and a pull request with a
  few pushes and check runs pushes the `reviewed` event off page one.
- A login filter per rung, without which the repository owner glancing at the pull request
  ends the wait and step 9 decides on a review that never arrived — and, since the `local`
  rung's login is the App's, without which Copilot's review would satisfy the local rung's
  wait and one review would count as two.
- The `local` rung's login is **discovered** from the App rather than written down. The App is
  the operator's and its slug is not this plugin's to know.

`scripts/await-review_test.sh` is the offline fixture corpus for the classification and for
the unavailable/refused/nothing-yet split; a change to those rules belongs there, not here.

Re-running the gate is safe on either rung: a review already on the pull request is classified
and returned without requesting another, and the newest review wins, so an old refusal sitting
beside a newer completed review is not misread.

None of those findings is a reason to look in on the script while it runs — a review that has
not landed yet reads exactly like one that never will, from your side, and the script is the
thing that can tell those apart. Re-run it after an exit 2; do not go looking at the review
endpoints in between.

## 8. Address the review

**Skipped entirely when the roster reached `none`.** The comments are addressed identically
whoever left them, so nothing below asks which rung reviewed.

Step 7 already printed the summary body on stdout. What it does not carry is the inline
comments, which is what you are here for:

```
gh api repos/<repo>/pulls/<pr>/comments --jq '.[] | "[\(.id)] \(.path):\(.line)\n\(.body)"'
```

An empty result is normal — a review whose body says it generated no comments still counts
as having reviewed, and step 7 exited 0 on it. The `local` rung's inline comments arrive here
too: `post-review.sh` posts them with the summary, and drops them and keeps the summary if
GitHub rejects a line that is not in the diff, so a review with findings in its body and none
inline is an ordinary outcome rather than a lost one. If you want the summary again, take it from
step 7's output rather than from `pulls/<pr>/reviews`, which lags (see step 7) and will
happily return an empty array for a review that landed.

Use judgment. Fix what is a real defect or a genuine improvement. Where a comment is wrong
or does not apply, reply on the thread explaining why rather than making the change — do not
silently ignore it, and do not change correct code just to clear a comment.

```
gh api --method POST repos/<repo>/pulls/<pr>/comments/<comment-id>/replies \
  -f body="<reply>" -q '.id'
```

The `pulls/<pr>/` segment belongs there. Copilot has claimed that the route is
`pulls/comments/<comment-id>/replies` and that the form above 404s; it is wrong, and the
reply rebutting it was posted with the command exactly as written. The shorter path is the
`GET`/`PATCH`/`DELETE` route for a single review comment, not the reply route.

If you push fixes, go back to step 6 and let checks re-run before labelling.

## 9. Label the pull request — never merge it yourself

**Do not run `gh pr merge`.** Not with `--auto`, not without. Two separate things break.
The permission classifier stops an agent merging to the default branch, so the command
fails. And a *subagent* that merges emits a security banner into its caller's context which
blocks the caller's next agent spawn — killing the loop on its second iteration, one
iteration after the mistake, where the cause is no longer visible.

Hand the merge to GitHub instead:

```
gh pr edit <pr> --add-label <merge.label>
```

`.github/workflows/<merge.workflow>` picks up the `labeled` event and enables native auto
merge. GitHub squash-merges the pull request once every required status check passes — or
immediately, if they have already passed — and leaves it open if one fails.

Apply the label only when **both** hold:

1. Checks are green, and
2. **either** a review completed on some rung — step 7 exited **0** — and every comment it
   left is now addressed or answered, **or** the roster reached `none`.

This is not a formality to route around: the label **is** the assertion that you verified
both conditions, and adding it without having done so is the same failure as merging
unreviewed work by hand. Condition 1 is enforced by branch protection whatever you do;
condition 2 is enforced by nothing but you.

Keeping the merge in a workflow puts the policy somewhere it can be read and changed — the
label gate plus the branch protection rule — rather than in a decision made mid-cycle and
visible only in a transcript. It is also what lets the loop run unattended: an agent merging
on its own is blocked, and labelling is not.

No exit from step 7 other than 0 is a completed review. A **refusal** (1) is `BLOCKED`
outright. Silence (2) and unavailability (3) send you to the next rung, and if the roster runs
out without reaching `none` they are `BLOCKED` too. In every one of those cases: do **not**
label the pull request. Leave it open, leave the worktree in place, and stop with a report
beginning `BLOCKED` that names the pull request, every rung tried and why each failed, so the
user can tell a pull request that needs a human look from one that merged on its own. Sending
unreviewed work to the default branch is the one step of this cycle that is not yours to take
unilaterally — unless the operator wrote `none` into the roster, which is the only way it
becomes theirs to have taken.

If the pull request is unreviewable because it exceeds the 300-file limit, say so in the
`BLOCKED` report and suggest how the work could be split; do not label it anyway.

### Confirm the label was acted on

There are **two** successful outcomes, and checking only for an armed auto merge request
will report a false failure on the more common one:

```
gh pr view <pr> --json state,autoMergeRequest -q '"\(.state) \(.autoMergeRequest.enabledAt // "not-armed")"'
```

- `MERGED …` — already merged. `gh pr merge --auto` merges immediately when the required
  checks have already passed, and since this cycle labels only after they pass, this is the
  usual result. `not-armed` beside `MERGED` is correct, not a fault.
- `OPEN <timestamp>` — auto merge is armed and waiting on a check still running.
- `OPEN not-armed` — **usually just means the workflow has not started yet.** The run is
  queued for a few seconds after the label lands, and reading the pull request immediately
  will show this every time. It is not evidence of a failure on its own.

Do not conclude anything from `OPEN not-armed` until the workflow run has actually finished.
The run list is what disambiguates:

```
gh run list --repo <repo> --workflow <merge.workflow> --branch issue-<n> --limit 1
```

`--branch` is what makes this your run. Without it, `--limit 1` is the most recent run of
that workflow in the whole repository — anyone labelling another pull request while yours is
queued hands you their result, and it reads as a verdict on yours.

If it has not completed, **do not build a wait of your own for it** — no sleeps, no polling
loop in turns of yours. Step 10 is a bounded wait for the merge itself, which is the outcome
this run is only a proxy for: go there and read its exit code. Come back here if it exits 2
more than twice — then read the run list once more, and its conclusion is the answer:

```
gh run list --repo <repo> --workflow <merge.workflow> --branch issue-<n> --limit 1 --json status,conclusion
```

A `success` conclusion with the pull request still open means auto merge is armed and waiting
on a check. A `failure` conclusion means the workflow itself is broken — report it, with the
output of `gh run view <id> --log-failed`, rather than falling back to a manual merge, which
is what this whole step exists to avoid.

## 10. Finish — one call

Everything after the label is mechanism, not judgment: wait for the merge, close the issue,
verify it closed, drop the worktree, delete the local branch. Run it as one script rather than
as five steps — a blocking `Bash` call with the tool's `timeout` set to `330000`, the script
bounding itself at five minutes. Step 6's rules hold here too: not `Monitor`, not
`run_in_background`, no sleeps of your own, and no reading the pull request or the worktree to
see how far it has got.

```
"${CLAUDE_PLUGIN_ROOT}/scripts/finish-issue.sh" <n> <pr> issue-<n>
```

Read its exit code. It is the assertion, and each value means one thing:

| exit | meaning | what you do |
| --- | --- | --- |
| 0 | merged, issue confirmed `CLOSED`, local cleanup done | continue to the report |
| 1 | the pull request closed without merging | `BLOCKED`, naming the pull request |
| 2 | the five minutes were up and the merge had not landed | **run it again**, in the same turn; every step is guarded, so it resumes. After three re-runs — a quarter of an hour with the pull request still open — go back to step 9's run list to find out why, and `BLOCKED` if nothing is running |
| 3 | the issue would not close | `BLOCKED` — this is the one that makes the next iteration redo merged work |
| 4 | usage or precondition failure | `BLOCKED`; the plugin or the environment is wrong, not the pull request |

Warnings on stderr (`WARN`) are untidiness, not failure — a worktree that would not remove,
a main checkout too dirty to update. Repeat them in the report and carry on.

Re-running after a timeout is safe: every step is guarded, so an already-merged pull request
skips the wait, an already-closed issue skips the close, and an absent worktree or branch
skips its removal.

### Why this is a script

Three of those steps have been rediscovered from first principles on cycle after cycle, and
the close is the expensive one. **A `Closes #<n>` line does not close the issue when a token
rather than a person performs the merge.** Across five consecutive merges the closing
reference registered correctly every time — step 5's assertion would have passed on all
five — and every issue stayed open until an agent noticed and closed it by hand.

Granting the merge workflow `issues: write` is the obvious explanation, and it was tested on
a real merge: the issue still did not close, and the loop closed it by hand eighteen seconds
later exactly as before. That explanation is **refuted**; the mechanism remains unknown.

So this close is not a backstop for something that usually works — for a long time it was
the *only* thing closing these issues, and an issue left open is one the next invocation of
this skill selects again, so the loop re-implements work it has already merged. Putting it
behind an exit code is what stops it depending on an agent remembering, at the very end of
the longest part of the cycle, a fact that is invisible unless you go looking for it.

The workflow this plugin installs now also closes linked issues explicitly, in a
`close-linked-issues` job. Both closes are idempotent — whichever arrives first wins and the
other reads `CLOSED` and does nothing — and the redundancy is deliberate, because that job
runs only when the repository is set up as below.

### The remote branch, and what it depends on

The script **deletes nothing remote.** `git push --delete` is denied by the operator's
settings, and neither an agent nor a script should work around that rule; the
`delete-merged-branch` job in `.github/workflows/<merge.workflow>` owns remote cleanup.

That job only fires if the merge was performed by a **GitHub App installation token**.
GitHub does not create workflow runs from events triggered by `GITHUB_TOKEN`, so a merge
performed with `GITHUB_TOKEN` emits a `closed` event that starts no run — and both the
branch deletion and the issue close above are skipped. Measured, not inferred: across ten
merges in two repositories every auto-merge run showed that job `skipped` and twenty-four
merged branches survived, while the one run where it did fire belonged to a pull request a
person merged by hand.

`backlog:setup-backlog` checks for the App secrets and reports when they are missing. If
they are, expect merged branches to accumulate; say so in the report rather than deleting
one, and note that `deleteBranchOnMerge` on the repository does not cover it either — that
fires for a merge a person performs.

### If the script is unavailable

Running interactively without the plugin root set, do the same five things by hand, in this
order, and check the state after the close rather than assuming it:

```
# 1. wait for MERGED, bounded and in the foreground; CLOSED without merging is a failure,
#    and a bound reached is "call it again", not "it failed"
# 2. gh issue view <n> --repo <repo> --json state -q .state
# 3. if OPEN: gh issue close <n> --repo <repo> --comment "Implemented in #<pr>, merged as <sha>."
# 4. re-read the state and confirm CLOSED
# 5. git worktree remove <worktreeDir>/issue-<n>
#    MAIN=$(git worktree list --porcelain | head -1 | cut -d' ' -f2-)
#    git -C "$MAIN" branch -D issue-<n>
#    # fast-forward the main checkout ONLY if it is already on <default-branch>;
#    # moving the branch the operator left it on is not this cycle's to do, and
#    # step 2 branches from origin/<default-branch> regardless
```

A stale *local* branch is what breaks a retry — `git worktree add -b issue-<n>` fails
against an existing branch, and the retry then reports `BLOCKED` on a name collision rather
than on anything real. The remote side needs nothing from you.

If a required check fails, auto merge stays armed and the pull request stays open; fix the
failure, push, and it merges when the rerun is green. If it stays `OPEN` with nothing
running, report it rather than merging by hand. If the pull request did not merge at all,
leave the worktree in place and report `BLOCKED` with the pull request number and the last
state you saw.

## Report

Finish with a short status: issue number and title, pull request number and URL, check
result, whether the pull request reached `MERGED`, and any judgment call that shaped the
public API. If the run was scoped, name the scope in full — the field as well as the value,
because the same value can exist on more than one of a board's fields. An outcome from a
scoped backlog says nothing about the rest of it.

- **Which rung reviewed**, by name, and what it flagged. `copilot` and `local` are different
  assurances and a report that says only "reviewed" hides which one was had.
- **Any rung you found UNAVAILABLE** — the gate's exit 3 — by name and with the reason:
  Copilot code review not enabled, the App credentials absent. Not a rung that **refused** the
  work; that one halts you at `BLOCKED` and never reaches a report like this. Your driver carries those forward into the next
  iteration, so this line is load bearing rather than decorative. Say it in a form it can
  read back:

  ```
  Reviewer: local (copilot unavailable: Copilot code review is not enabled for this organisation)
  ```

  A rung that merely went **silent** does not go on that line. Silence is what a slow reviewer
  looks like, and retiring one on it costs every remaining iteration its best reviewer.
- When the roster reached `none`: say plainly that **the pull request merged without a
  review**, and name every rung tried and why each was unavailable. Do not leave that to be
  inferred from the absence of a review line.

If you stopped early, say exactly where and why, beginning the report with `BLOCKED`.

### If you have to hand back control unfinished

Sometimes control goes back to your caller with the cycle still running — you ran out of room,
or something outside the cycle interrupted you. That is neither a block nor a finished
iteration, and it has its own first line:

> `IN FLIGHT` — issue #<n>, PR #<pr>, stopped at step <k> waiting for <the gate>.

Then say what state you left behind: the branch, the worktree, whether the pull request is
labelled. `backlog:run-backlog` reads that word and **resumes you** instead of starting a new
iteration over your open pull request.

Never leave a mid-cycle return unlabelled. A report that merely describes how far you got
reads to the driver as a completed iteration, and the next iteration is then spawned over an
unmerged pull request, with your worktree still on disk and the issue still open. And do not
use `IN FLIGHT` where `BLOCKED` belongs: `IN FLIGHT` says the cycle can be picked up exactly
where it stands, `BLOCKED` says it needs a person.

## Stop conditions

Stop and report — do not push through — if any of these happen:

- `select-issue.sh` exits 4 — `.claude/backlog.json` is missing, does not parse, carries an
  unknown `dependencies.style`, has an empty `verify`, carries a `review.reviewers` roster
  naming an unknown rung or putting `none` anywhere but last, still carries the retired
  `review.required`, names a milestone that does not exist, or a requested project scope could
  not be resolved. Never re-run it with an argument
  dropped, widened or changed to get past the last two, and never edit
  `.claude/backlog.json` to get past them either.
- The same CI failure survives three fix attempts.
- Acceptance criteria are ambiguous enough that two readings produce materially different
  public APIs.
- Landing the pull request would require a force-push, a branch-protection override, or
  discarding someone else's commits.
- `git status` in the main checkout is dirty with changes you did not make.
- `await-review.sh` exited 1 on any rung — a reviewer looked at the work and refused it. This
  one does **not** advance the roster: a reviewer with different limits is not a second
  opinion on work the first one could not read.
- The roster is exhausted without a completed review and without reaching `none`.
- A wait's exit 2 outlasts its re-run budget: six for `await-checks.sh`, four for
  `await-review.sh`, three for `finish-issue.sh`. An exit 2 on its own is not a stop
  condition — it is the wait asking to be called again, in the same turn — but a gate that
  will not settle inside those budgets is a pull request that needs a person.

When you stop for one of these, begin the report with `BLOCKED` so the caller can tell a
halted cycle from a finished one at a glance. `backlog:run-backlog` halts the loop on that
word.
