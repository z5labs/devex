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
branch are read from the repository itself (step 0); the label, milestone, dependency
convention, verify commands, merge label and worktree directory are read from
`.claude/backlog.json`.

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

The six keys, and what each is used for:

| key | used at |
| --- | --- |
| `select.label`, `select.milestone`, `select.limit` | step 1 — read by `select-issue.sh`, not by you |
| `dependencies.style` | step 1 — same |
| `verify` | step 4, the commands that gate the pull request |
| `merge.label`, `merge.workflow` | step 9, handing the merge to GitHub |
| `review.required` | steps 7 and 8, whether Copilot gates the merge |
| `worktreeDir` | steps 2 and 10, where the worktree lives |

Read the file for the four keys you use directly. The first four rows belong to step 1's
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
| 4 | a message naming the problem | `BLOCKED`. The config or the environment is wrong, not the backlog — usually `.claude/backlog.json` is missing, unparseable, carries an unknown `dependencies.style`, or has an empty `verify`. Point at `backlog:setup-backlog`. |

The script walks candidates in ascending number order and takes the first whose every
declared dependency is `CLOSED`. Its per-candidate reasoning goes to stderr, so a selection
you did not expect can be explained without re-running anything.

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
`dependencies.style` from. GitHub's own native blocked-by field is **not** consulted: it has
been observed empty on repositories whose issue bodies do declare ordering in prose, and an
empty native field reads as an unblocked backlog — the one wrong answer that gets work done
in the wrong order rather than not at all.

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

**`none`** — the backlog declares no ordering. Bodies are not parsed at all; every open issue
carrying the label is eligible and the lowest-numbered one wins.

Under both parsing styles, text inside a ``` or `~~~` fence is example text, not a
declaration. A bare `#N` means an issue in this repository. A cross-repository `owner/repo#N`
is not modelled: it makes *that issue* ineligible and is named in the report, rather than
being skipped and the issue called eligible — but the walk continues, so one unresolvable
reference does not starve a backlog whose other issues are workable.

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

Do not foreground a `sleep` loop. Either use `gh pr checks <pr> --watch --fail-fast`, or
poll with `Monitor`.

Do **not** use Bash `run_in_background` for a wait loop. It has been observed exiting
immediately without ever polling, which looks like a completed wait and reports whatever the
first sample happened to be. `Monitor` is the tool for every wait in this cycle.

- Exit 0 → green, continue.
- Failure → read the logs (`gh run view <id> --log-failed`), fix, push, and re-watch. After
  **three** failed attempts on the same root cause, stop and report instead of looping.
- `no checks reported` → nothing gates this pull request. Workflows queue for a few seconds
  after a push, so re-check once after a short wait before concluding anything. If there
  are still none, do not silently treat it as a pass: record `no checks reported` in the
  report and continue. This is the same gap `backlog:setup-backlog` reports when the default
  branch has no required status check, seen from the other side — with no required check,
  the label in step 9 merges the pull request the instant it lands.

## 7. Request the Copilot review

**If `review.required` is `false`, skip this step and step 8 entirely** and go to step 9.
Nothing here is optional when it is `true`.

Copilot is a **Bot**, not a User. Two things follow. The login is
`copilot-pull-request-reviewer[bot]`, and passing a bare `reviewers[]=Copilot` to the REST
endpoint `POST /pulls/<pr>/requested_reviewers` returns 200 while silently doing nothing —
`requested_reviewers` stays empty and the wait below then times out on a review that was
never requested.

The GraphQL mutation is the reliable path. It takes `botIds`, not `reviewers`:

```
PR_ID=$(gh pr view <pr> --json id --jq .id)
BOT_ID=$(gh api '/users/copilot-pull-request-reviewer[bot]' --jq .node_id)
gh api graphql -f query='
mutation($pr:ID!, $bot:ID!) {
  requestReviews(input: {pullRequestId: $pr, botIds: [$bot], union: true}) {
    pullRequest { reviewRequests(first:10) { nodes {
      requestedReviewer { __typename ... on Bot { login } } } } }
  }
}' -f pr="$PR_ID" -f bot="$BOT_ID"
```

The bot ID is looked up by login rather than hard-coded. Confirm the response lists
`copilot-pull-request-reviewer` under `reviewRequests` — an empty list means the request did
not take.

REST also works *provided the full bot login is used*, and is a reasonable fallback if the
mutation errors:

```
gh api --method POST repos/<repo>/pulls/<pr>/requested_reviewers \
  -f "reviewers[]=copilot-pull-request-reviewer[bot]" -q '.number'
```

Either way, confirm the login appears in `requested_reviewers` before you start waiting.

### Wait for the review to land

Pass this as the `command` of a `Monitor` call — not to Bash with `run_in_background`, for
the reason in step 6:

```
for i in $(seq 1 40); do
  n=$(gh api --paginate repos/<repo>/issues/<pr>/timeline \
        --jq '.[]|select(.event=="reviewed")
              |select(.user.login|test("copilot";"i"))|.id' 2>/dev/null | wc -l)
  if [ "$n" -gt 0 ]; then echo "copilot review landed"; exit 0; fi
  sleep 15
done
echo "copilot review timed out"; exit 1
```

Two filters, both load bearing.

`--paginate`, because the timeline returns thirty events per page by default, and a pull
request that saw several pushes, check runs and comments will push the `reviewed` event off
the first page — where an unpaginated read reports zero and the loop times out on a reviewed
pull request, which is the exact failure this step exists to avoid. Counting `.id` lines
rather than taking a `length` per page is what makes the count work across pages.

The `copilot` login filter, because a `reviewed` event from *anyone* would otherwise end the
wait. The repository owner reviewing a pull request while Copilot was still working would
satisfy this loop, and step 9 would then be deciding on a review that never arrived. The
gate is specifically that Copilot completed; the wait has to be specific in the same way.
Matching is case-insensitive on purpose — the timeline reports this reviewer as `Copilot`
while the `pulls/` endpoints report `copilot-pull-request-reviewer[bot]`.

The wait polls the **timeline**, not `pulls/<pr>/reviews`. The reviews endpoint — and the
equivalent GraphQL query — have been seen returning an empty array for as long as forty
minutes after Copilot had in fact submitted, while the timeline showed it immediately.
Polling the reviews endpoint therefore produces a timeout on a pull request that was
reviewed, and that reads as a missing review rather than as the API lagging.

**Never report `BLOCKED` for a missing review without checking the timeline first:**

```
gh api --paginate repos/<repo>/issues/<pr>/timeline \
  --jq '.[]|select(.event=="reviewed")|select(.user.login|test("copilot";"i"))' \
  | jq -s 'sort_by(.submitted_at)|last|{user:.user.login,state,body}'
```

Same two filters, for the same two reasons — without the login filter this returns whichever
review is newest, which after a human comment is not Copilot's. And `jq -s` because it
slurps the per-page objects back into one array before sorting: sorting inside `--jq` would
sort each page separately and give you the last review *of the last page*, not the last
review.

### A non-empty reviews array does not mean the pull request was reviewed

Copilot posts a review whose body **declines** the work — most often `"Copilot wasn't able
to review this pull request because it exceeds the maximum number of files (300)"` — and
that decline satisfies a naive `length > 0` test. Check the body before treating the review
as real.

Read the **most recent** Copilot review only. Reruns and pushed fixes leave older reviews in
the array, so an earlier decline sitting beside a later completed review — or the reverse —
is easy to misread:

```
gh api repos/<repo>/pulls/<pr>/reviews \
  --jq '[.[] | select(.user.login | test("copilot";"i"))]
        | sort_by(.submitted_at) | last | .body // "no copilot review"'
```

A body matching `wasn't able to review` is a **declined** review, not a completed one. This
has been seen in the wild: vendored test suites pushed a pull request past the 300-file
limit, Copilot declined, the `length > 0` check passed, and the cycle merged with no review
at all.

If the review is declined, times out, or the request itself errors (Copilot code review not
enabled for the organisation), the cycle does **not** label the pull request — see step 9.

## 8. Address the review

**Skipped entirely when `review.required` is `false`.**

Pull both the summary review and the inline comments — a review whose body says it
`generated no comments` still counts as having reviewed:

```
gh api repos/<repo>/pulls/<pr>/reviews  --jq '.[] | "\(.user.login) [\(.state)]\n\(.body)"'
gh api repos/<repo>/pulls/<pr>/comments --jq '.[] | "[\(.id)] \(.path):\(.line)\n\(.body)"'
```

If the reviews endpoint is still lagging (step 7), read the body from the timeline instead —
the `reviewed` events carry the same `state` and `body`. An empty result here is a stale
endpoint, not an absent review.

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
2. **either** `review.required` is `false`, **or** a Copilot review actually **completed** —
   it either left comments (every one now addressed or answered) or reported that it
   generated none.

This is not a formality to route around: the label **is** the assertion that you verified
both conditions, and adding it without having done so is the same failure as merging
unreviewed work by hand. Condition 1 is enforced by branch protection whatever you do;
condition 2 is enforced by nothing but you.

Keeping the merge in a workflow puts the policy somewhere it can be read and changed — the
label gate plus the branch protection rule — rather than in a decision made mid-cycle and
visible only in a transcript. It is also what lets the loop run unattended: an agent merging
on its own is blocked, and labelling is not.

When `review.required` is `true`, a review that was declined, never arrived, or was never
requested because the request errored is **not** a completed review. Do **not** label the
pull request. Leave it open, leave the worktree in place, and stop with a report beginning
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
gh run list --repo <repo> --workflow <merge.workflow> --limit 1
```

Wait for it to leave `queued` and `in_progress`. A `success` conclusion with the pull
request still open means auto merge is armed and waiting on a check. A `failure` conclusion
means the workflow itself is broken — report it, with the output of
`gh run view <id> --log-failed`, rather than falling back to a manual merge, which is what
this whole step exists to avoid.

## 10. Finish — one call

Everything after the label is mechanism, not judgment: wait for the merge, close the issue,
verify it closed, drop the worktree, delete the local branch. Run it as one script rather
than as five steps. Pass it as the `command` of a `Monitor` call — the wait runs up to ten
minutes, past the Bash tool's ceiling — and not to Bash with `run_in_background`:

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
#    git -C "$MAIN" checkout <default-branch> && git -C "$MAIN" pull --ff-only
#    git -C "$MAIN" branch -D issue-<n>
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
public API.

- When `review.required` is `true`: whether Copilot reviewed and what it flagged.
- When `review.required` is `false`: say plainly that **the pull request merged without a
  review**, because `review.required` is `false`. Do not leave that to be inferred from the
  absence of a review line.

If you stopped early, say exactly where and why, beginning the report with `BLOCKED`.

## Stop conditions

Stop and report — do not push through — if any of these happen:

- `select-issue.sh` exits 4 — `.claude/backlog.json` is missing, does not parse, carries an
  unknown `dependencies.style`, or has an empty `verify`.
- The same CI failure survives three fix attempts.
- Acceptance criteria are ambiguous enough that two readings produce materially different
  public APIs.
- Landing the pull request would require a force-push, a branch-protection override, or
  discarding someone else's commits.
- `git status` in the main checkout is dirty with changes you did not make.
- `review.required` is `true` and the Copilot review declined, timed out, or was never
  requested successfully.

When you stop for one of these, begin the report with `BLOCKED` so the caller can tell a
halted cycle from a finished one at a glance. `backlog:run-backlog` halts the loop on that
word.
