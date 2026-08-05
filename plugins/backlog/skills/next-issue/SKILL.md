---
name: next-issue
description: Take exactly one eligible story issue from the backlog through the full development cycle — select it, branch a worktree, implement the acceptance criteria, run the repository's verify commands, open a pull request, wait for checks, get a Copilot review and answer it, then label the pull request for GitHub to auto merge, close the issue and clean up. Use this whenever the user asks to "work the backlog", "take the next issue", "do the next story", "pick up the next ticket", or runs the backlog loop; it is also what the `issue-worker` agent invokes on every iteration of `backlog:run-backlog`. Everything repository-specific comes from `.claude/backlog.json`. Skip this when the user names a specific issue to explore rather than to land, or when they want the backlog bootstrapped rather than worked — that is `backlog:setup-backlog`.
allowed-tools: Bash, Read, Write, Edit, Glob, Grep, Monitor, TaskCreate, TaskUpdate, EnterWorktree, ExitWorktree
---

# next-issue

Take **exactly one** issue from backlog to a merged pull request, then stop. Do not start a
second issue in the same invocation — the loop re-invokes this skill for the next one.

You never run `gh pr merge`. You label the pull request and GitHub merges it, gated by the
default branch's protection rules; see step 9.

Nothing about the repository is written into this skill. The repository slug and default
branch are read from the repository itself (step 0); the label, milestone, optional project
scope, dependency convention, verify commands, merge label and worktree directory are read
from `.claude/backlog.json`.

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
| `select.label`, `select.milestone`, `select.limit`, `select.project` | step 1 — read by `select-issue.sh`, not by you |
| `dependencies.style` | step 1 — same |
| `verify` | step 4, the commands that gate the pull request |
| `merge.label`, `merge.workflow` | step 9, handing the merge to GitHub |
| `review.required` | steps 7 and 8, whether Copilot gates the merge |
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
| 4 | a message naming the problem | `BLOCKED`. The config or the environment is wrong, not the backlog — usually `.claude/backlog.json` is missing, unparseable, carries an unknown `dependencies.style`, or has an empty `verify`. A requested project scope that could not be resolved lands here too; see below. Point at `backlog:setup-backlog`. |

The script walks candidates in ascending number order and takes the first whose every
declared dependency is `CLOSED`. Its per-candidate reasoning goes to stderr, so a selection
you did not expect can be explained without re-running anything.

### Scoping the run to one project field value

Some repositories group work by a single-select field on a GitHub project — `Module`, `Area`,
`Component` — rather than by labels or milestones. Where `.claude/backlog.json` carries
`select.project`, one run can be restricted to a single value of that field:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/select-issue.sh" --project-value <value>
```

Pass the flag when, and only when, you were asked for a scoped run — the user named one, or
`backlog:run-backlog` put one in your prompt. Otherwise call the script with no arguments and
let `select.project.value` decide, which is normally `null` and means the whole backlog.
`--no-project-filter` is the other direction: run unscoped even though the config pins a
value.

The scope is applied before the dependency walk, so the walk sees only in-scope issues and a
blocker outside the scope cannot hold up the one you were asked to work.

Two things to know when it fails:

- It needs the **`read:project`** token scope, which `repo` does not include.
  `gh auth refresh -s read:project` grants it.
- Every project failure is exit 4 — a token without that scope, an unknown project, a field
  that does not exist or is not a single-select, a value that is not one of the field's
  options, `--project-value` with no `select.project` in the config. **Do not retry without
  the flag.** Selecting from the whole backlog when a scope was asked for gets work done in
  the wrong order, which is worse than selecting nothing; report `BLOCKED` with the message.

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
declaration. A bare `#N` means an issue in this repository. Under all three declaring styles,
a cross-repository `owner/repo#N` is not modelled: it makes *that issue* ineligible and is
named in the report, rather than being skipped and the issue called eligible — but the walk
continues, so one unresolvable reference does not starve a backlog whose other issues are
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

## 6. Wait for checks

Pass this as the `command` of a `Monitor` call:

```
gh pr checks <pr> --repo <repo> --watch --fail-fast
```

**Not to Bash.** `--watch` blocks until every check finishes, which on a repository with real
CI outlasts the Bash tool — the call is killed mid-wait, and a wait that never finished looks
exactly like a pull request whose checks failed. Every wait in this cycle goes through
`Monitor` for that reason; step 7 and step 10 are the same.

Set `timeout_ms` to cover the monitored command's own wait. It defaults to five minutes,
while the scripts steps 7 and 10 monitor wait up to ten, so `660000` covers every wait in
this cycle with room to spare. A monitor killed halfway through a wait that would have
succeeded reads as a failed wait.

### Waiting is not something you do

This is the first of the cycle's three waits, and the rule is the same for all of them.

A `Monitor` call is the wait. Its result comes back to you on its own — the command's output
arrives as notifications and its exit is reported when it ends — so there is nothing to
collect, nothing to check, and nothing you can do that makes it arrive sooner. Once the wait
is armed, **your turn is over.** The next thing you do is read what came back.

Two improvisations have been measured on real runs, and both are forbidden by name:

- **No no-op `sleep` Bash calls to pass the time.** `sleep 0.5; echo ok` and its variants
  advance nothing — the monitored command is a separate process and it is not waiting on you
  — while each one costs a full assistant turn. One measured iteration spent 106 turns, about
  a quarter of its entire token cost, on that loop to buy under a minute of real waiting; the
  two iterations either side of it spent 0 and 2.
- **No peeking.** Do not `Read`, `cat` or `tail` a wait's output file, log or task record to
  see how a `Monitor` that has not reported yet is getting on, and do not re-run the
  underlying command by hand to check on it. Its output reaches you in full when it ends.

The `Bash` tool's own description states the same rule from the other side: *foreground
`sleep` is blocked; use `Monitor` with an until-loop to wait on a condition.*

And do **not** reach for Bash `run_in_background` when a wait feels unreliable. It is not a
more observable `Monitor`; it is a wait that has been observed exiting immediately without
ever polling, reporting whatever the first sample happened to be as though the wait had
completed. `Monitor` is the tool that actually waits. Where a wait needs a precondition, the
precondition goes inside the monitored command — see `no checks reported` below — never into
a turn of your own.

- Exit 0 → green, continue.
- Failure → read the logs (`gh run view <id> --log-failed`), fix, push, and re-watch. After
  **three** failed attempts on the same root cause, stop and report instead of looping.
- `no checks reported` → most often the workflows have not been created yet; runs queue for a
  few seconds after a push. Do not re-check by hand and do not sleep between checks. Put the
  precondition inside the `Monitor` command, so one call waits for the checks to appear and
  then watches them:

  ```
  until gh pr checks <pr> --repo <repo> >/dev/null 2>&1; do sleep 5; done; gh pr checks <pr> --repo <repo> --watch --fail-fast
  ```

  `until <precondition>; do sleep 5; done; <blocking command>` is the sanctioned shape for
  every "wait for X to exist, then wait for X to finish" in this cycle. The `sleep` is inside
  the monitored command rather than in a turn of yours, and the whole wait costs one call.
  `timeout_ms` bounds it, so a precondition that never becomes true ends the monitor instead
  of hanging the cycle.

  If that call ends with the checks still unreported, nothing gates this pull request. Do not
  silently treat it as a pass: record `no checks reported` in the report and continue. This is
  the same gap `backlog:setup-backlog` reports when the default branch has no required status
  check, seen from the other side — with no required check, the label in step 9 merges the
  pull request the instant it lands.

## 7. The Copilot review — one call

**If `review.required` is `false`, skip this step and step 8 entirely** and go to step 9.
Nothing here is optional when it is `true`.

Requesting the review, waiting for it, and deciding whether what landed *is* a review are one
call. Pass it as the `command` of a `Monitor` call with `timeout_ms` of `660000` — the wait
runs up to ten minutes, past both the Bash tool's ceiling and `Monitor`'s default — and not to
Bash with `run_in_background`. This is the longest wait in the cycle and the one that has
attracted busy-waiting; step 6's rule holds unchanged here. Arm it, end your turn, and read
the exit code when it arrives:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/await-review.sh" <pr>
```

| exit | meaning | what you do |
| --- | --- | --- |
| 0 | a review **completed** — it left comments, or reported it generated none. stdout is its body | continue to step 8 |
| 1 | the most recent review **declined** the work; stdout is why | `BLOCKED`, and do **not** label. If it is the 300-file limit, say so and suggest how the work could be split |
| 2 | timed out waiting | re-run it; it resumes. If it times out again, `BLOCKED` |
| 3 | the review could not be requested — Copilot code review is probably not enabled for this organisation | `BLOCKED`, naming that |
| 4 | usage or precondition failure | `BLOCKED`; the plugin or the environment is wrong, not the pull request |

**Only exit 0 lets you label the pull request in step 9.**

### Why this is a script

"Did Copilot review this?" looks like one question and is two, and the cycle used to answer
them in places far enough apart that the second could be skipped. The wait was a polling loop
that exited on the first `reviewed` event; whether that review had *declined* the work was a
separate command sixty lines further down, under its own heading. Copilot posts a review
whose body declines — most often `"Copilot wasn't able to review this pull request because it
exceeds the maximum number of files (300)"` — and that decline satisfies any `length > 0`
test. It has been missed in the wild: a vendored test suite pushed a pull request past the
limit, Copilot declined, the check passed, and the cycle merged with no review at all.

The label in step 9 **is** the assertion that a review completed. Making it an exit code is
what stops that assertion depending on an agent remembering a second question at the very end
of the longest part of the cycle.

Four findings live inside the script rather than in prose here, each of which cost a
debugging session:

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
- A case-insensitive `copilot` login filter, without which the repository owner glancing at
  the pull request ends the wait and step 9 decides on a review that never arrived.

Re-running is safe: a Copilot review already on the pull request is classified and returned
without requesting another, and the newest review wins, so an old decline sitting beside a
newer completed review is not misread.

Every finding above is a reason the wait is *inside* a script rather than in your hands. None
of them is a reason to look in on the script while it runs — a review that has not landed yet
reads exactly like one that never will, from your side, and the script is the thing that can
tell those apart. Re-run it after an exit 2; do not check on it before one.

## 8. Address the review

**Skipped entirely when `review.required` is `false`.**

Step 7 already printed the summary body on stdout. What it does not carry is the inline
comments, which is what you are here for:

```
gh api repos/<repo>/pulls/<pr>/comments --jq '.[] | "[\(.id)] \(.path):\(.line)\n\(.body)"'
```

An empty result is normal — a review whose body says it `generated no comments` still counts
as having reviewed, and step 7 exited 0 on it. If you want the summary again, take it from
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
2. **either** `review.required` is `false`, **or** step 7 exited **0** — a review actually
   completed, and every comment it left is now addressed or answered.

This is not a formality to route around: the label **is** the assertion that you verified
both conditions, and adding it without having done so is the same failure as merging
unreviewed work by hand. Condition 1 is enforced by branch protection whatever you do;
condition 2 is enforced by nothing but you.

Keeping the merge in a workflow puts the policy somewhere it can be read and changed — the
label gate plus the branch protection rule — rather than in a decision made mid-cycle and
visible only in a transcript. It is also what lets the loop run unattended: an agent merging
on its own is blocked, and labelling is not.

When `review.required` is `true`, any exit from step 7 other than 0 — declined (1), timed out
(2), never requested (3) — is **not** a completed review. Do **not** label the pull request. Leave it open, leave the worktree in place, and stop with a report beginning
`BLOCKED` that names the pull request and why the review is missing, so the user can tell a
pull request that needs a human look from one that merged on its own. Sending unreviewed
work to the default branch is the one step of this cycle that is not yours to take
unilaterally.

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

If it has not completed, waiting for it is a `Monitor` call in the form step 6 gives, not a
sequence of Bash calls of your own:

```
until [ "$(gh run list --repo <repo> --workflow <merge.workflow> --branch issue-<n> --limit 1 --json status --jq '.[0].status // ""')" = completed ]; do sleep 5; done
gh run list --repo <repo> --workflow <merge.workflow> --branch issue-<n> --limit 1 --json conclusion --jq '.[0].conclusion'
```

A `success` conclusion with the pull
request still open means auto merge is armed and waiting on a check. A `failure` conclusion
means the workflow itself is broken — report it, with the output of
`gh run view <id> --log-failed`, rather than falling back to a manual merge, which is what
this whole step exists to avoid.

## 10. Finish — one call

Everything after the label is mechanism, not judgment: wait for the merge, close the issue,
verify it closed, drop the worktree, delete the local branch. Run it as one script rather
than as five steps. Pass it as the `command` of a `Monitor` call with `timeout_ms` of
`660000` — the wait runs up to ten minutes, past both the Bash tool's ceiling and `Monitor`'s
default — and not to Bash with `run_in_background`. Step 6's rule holds here too: arm it, end
your turn, and read the exit code when it arrives. Do not sleep, and do not read the pull
request or the worktree to see how far it has got.

```
"${CLAUDE_PLUGIN_ROOT}/scripts/finish-issue.sh" <n> <pr> issue-<n>
```

Read its exit code. It is the assertion, and each value means one thing:

| exit | meaning | what you do |
| --- | --- | --- |
| 0 | merged, issue confirmed `CLOSED`, local cleanup done | continue to the report |
| 1 | the pull request closed without merging | `BLOCKED`, naming the pull request |
| 2 | timed out waiting for the merge | re-run it; it resumes. If it times out again with nothing running, `BLOCKED` |
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
# 1. wait for MERGED (Monitor); CLOSED without merging and a timeout are both failures
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
public API. If the run was scoped with `--project-value`, name the scope — an outcome from a
scoped backlog says nothing about the rest of it.

- When `review.required` is `true`: whether Copilot reviewed and what it flagged.
- When `review.required` is `false`: say plainly that **the pull request merged without a
  review**, because `review.required` is `false`. Do not leave that to be inferred from the
  absence of a review line.

If you stopped early, say exactly where and why, beginning the report with `BLOCKED`.

## Stop conditions

Stop and report — do not push through — if any of these happen:

- `select-issue.sh` exits 4 — `.claude/backlog.json` is missing, does not parse, carries an
  unknown `dependencies.style`, has an empty `verify`, or a requested project scope could not
  be resolved. Never re-run it without `--project-value` to get past the last of those.
- The same CI failure survives three fix attempts.
- Acceptance criteria are ambiguous enough that two readings produce materially different
  public APIs.
- Landing the pull request would require a force-push, a branch-protection override, or
  discarding someone else's commits.
- `git status` in the main checkout is dirty with changes you did not make.
- `review.required` is `true` and `await-review.sh` exited non-zero — the review declined,
  timed out, or could not be requested.

When you stop for one of these, begin the report with `BLOCKED` so the caller can tell a
halted cycle from a finished one at a glance. `backlog:run-backlog` halts the loop on that
word.
