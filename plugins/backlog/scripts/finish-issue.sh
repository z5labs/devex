#!/usr/bin/env bash
#
# finish-issue.sh — everything after the merge label, as one call.
#
# Usage: finish-issue.sh <issue-number> <pr-number> [branch]
#        branch defaults to issue-<issue-number>
#        finish-issue.sh --classify-runs --sha <sha>
#                          # classify the merge workflow's runs (JSON on stdin)
#
# Why this is a script and not five numbered steps.
#
# The tail of a cycle — wait for the merge, close the issue, verify it closed,
# drop the worktree, delete the local branch — carries no judgment at all. It is
# also the part that runs ten-plus minutes after the interesting work, at the
# point where an agent's context is fullest, and it is five actions that can be
# half-done without anything looking wrong. The close in particular has been
# rediscovered from first principles on cycle after cycle, because GitHub does
# not auto-close a linked issue when the merge is performed by a workflow's
# GITHUB_TOKEN — the closing reference registers, and nothing acts on it. An
# issue left open is one the next iteration selects again, so the loop
# re-implements work it has already merged.
#
# One call with an exit code is the fix. It cannot be forgotten, it cannot be
# partially completed, and it asserts the close rather than trusting it.
#
# Every step is guarded, so re-running after a timeout is safe and cheap: an
# already-merged pull request skips the wait, an already-closed issue skips the
# close, an absent worktree or branch skips its removal.
#
# It deletes nothing remote. The merge workflow's delete-merged-branch job owns
# the remote branch, and `git push --delete` is denied by the operator's
# settings — a script working around that rule would be the wrong shape whether
# or not any single deletion was harmless.
#
# Why the wait reads the merge workflow's runs, not only the pull request.
#
# The label does not merge anything by itself: it starts the merge workflow,
# and that run arms auto merge. If the run fails — auto merge disabled on the
# repository, squash not allowed, a token missing a permission, a secret's name
# mistyped — nothing is armed and the pull request stays OPEN for ever. Read
# only the pull request's state and that looks exactly like a merge queued
# behind a slow check, so the wait asked to be re-run until its budget was
# spent: Zaba505/backup #13 waited a quarter of an hour on a run that had
# already failed.
#
# So each poll also reads the merge workflow's runs on the pull request's head
# commit (`merge.workflow` in `.claude/backlog.json`), and the newest one that
# is a merge attempt decides. A run every job of which was skipped is another
# label's `labeled` event, not a merge attempt, and is passed over. Failed,
# cancelled or otherwise dead is exit 5, at once, naming the run. Running, or
# succeeded with the pull request still open, keeps waiting; so does no run at
# all, or one that cannot be read — the run is queued a few seconds after the
# label lands, and an unreadable list is not a verdict. A run on an earlier head
# is not this head's, in either direction. `--classify-runs` exposes exactly
# that decision on stdin, with no network, which is what `finish-issue_test.sh`
# exercises.
#
# Exit codes:
#   0  merged and the issue is closed (cleanup warnings, if any, go to stderr)
#   1  the pull request closed without merging
#   2  timed out waiting for the merge
#   3  the issue would not close
#   4  usage or precondition failure
#   5  the merge workflow's run on the pull request's head failed or was
#      cancelled, so auto merge was never armed; stderr names the run

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

# Five minutes per call, deliberately: the caller waits by BLOCKING on this
# script, so the bound has to fit inside a single foreground call rather than
# span turns. A merge queued behind a slow check takes longer; exit 2 is the
# answer, and the caller runs it again in the same turn.
POLL_COUNT=${BACKLOG_MERGE_POLL_COUNT:-20}
POLL_SECONDS=${BACKLOG_MERGE_POLL_SECONDS:-15}

fail() { local code=$1; shift; printf 'finish-issue: %s\n' "$*" >&2; exit "$code"; }
warn() { printf 'finish-issue: WARN %s\n' "$*" >&2; }

command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

# ------------------------------------------------------------ classifying -----
# `gh run list --json databaseId,status,conclusion,headSha,createdAt,url` for
# the merge workflow in, one verdict out. Prints the run it is reporting on, as
# `<conclusion, or status while it runs>  <url>`.
#
# Only runs on <sha> exist, and of those, runs whose every job was skipped are
# not merge attempts. The newest of what is left decides:
#
#   0  it succeeded: auto merge is armed, and the merge waits on GitHub
#   1  it failed, was cancelled, timed out, never started or wants an approval:
#      nothing is armed, so the pull request will not merge
#   2  it is still running, or ended in a conclusion not known to be fatal
#   3  nothing to classify: no run on <sha> yet, or the input is not an array
classify_runs() { # <runs json> <sha>
  local json=$1 sha=$2 latest verdict
  printf '%s' "$json" | jq -e 'type == "array"' >/dev/null 2>&1 || return 3

  latest=$(printf '%s' "$json" | jq -c --arg sha "$sha" '
    [ .[] | select(.headSha == $sha)
          | select((.status == "completed" and (.conclusion | IN("skipped", "neutral"))) | not) ]
    | sort_by(.createdAt) | last // empty')
  [ -n "$latest" ] || return 3

  verdict=$(printf '%s' "$latest" | jq -r '
    (if .status == "completed" then .conclusion else .status end) as $s
    | "\($s // "unknown")  \(.url // "")"')
  printf '%s\n' "$verdict"

  printf '%s' "$latest" | jq -e '.status == "completed"' >/dev/null || return 2
  printf '%s' "$latest" | jq -e '.conclusion == "success"' >/dev/null && return 0
  printf '%s' "$latest" | jq -e '.conclusion
    | IN("failure", "cancelled", "timed_out", "startup_failure", "action_required")' >/dev/null && return 1
  return 2
}

if [ "${1:-}" = --classify-runs ]; then
  shift
  sha=''
  while [ $# -gt 0 ]; do
    case $1 in
      --sha) [ $# -ge 2 ] || fail 4 "--sha needs a value"; sha=$2; shift 2 ;;
      *) fail 4 "usage: finish-issue.sh --classify-runs --sha <sha>  (JSON on stdin)" ;;
    esac
  done
  [ -n "$sha" ] || fail 4 "usage: finish-issue.sh --classify-runs --sha <sha>  (JSON on stdin)"
  classify_runs "$(cat)" "$sha"
  exit $?
fi

# -------------------------------------------------------------- finishing -----
[ $# -ge 2 ] || fail 4 "usage: finish-issue.sh <issue-number> <pr-number> [branch]"
ISSUE=$1
PR=$2
BRANCH=${3:-issue-$1}

command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"

# Move to the main checkout before doing anything else. This script removes the
# worktree it may well have been called from, and a process whose working
# directory has been deleted fails later, somewhere unrelated. `git worktree
# list` reports the main checkout first, so this needs no configured path.
MAIN=$(git worktree list --porcelain 2>/dev/null | awk '/^worktree /{print substr($0, 10); exit}')
[ -n "$MAIN" ] || fail 4 "not inside a git repository"
cd "$MAIN" || fail 4 "cannot enter the main checkout at $MAIN"

# Resolved from GitHub, never configured: a fork or a rename must not be able to
# point this at a repository it no longer describes. The origin remote names it
# and `GET /repos/{owner}/{repo}` confirms it — REST, like every call below,
# because a Claude Code cloud session's GitHub proxy refuses the GraphQL that
# `gh repo view`, `gh pr view` and `gh issue` are built on.
RESOLVED=$("$HERE/repo.sh") || fail 4 "cannot resolve the repository; see the repo: line above"
IFS=$'\t' read -r REPO DEFAULT_BRANCH <<<"$RESOLVED"
[ -n "${REPO:-}" ] && [ -n "${DEFAULT_BRANCH:-}" ] \
  || fail 4 "cannot resolve the repository slug and default branch"

# The merge workflow, read from the main checkout's config. Without it the wait
# still works — it just cannot tell a failed merge run from a slow merge, which
# is the silence this check exists to end, so say so rather than skip quietly.
WORKFLOW=$(jq -r '.merge.workflow // ""' .claude/backlog.json 2>/dev/null || printf '')
[ -n "$WORKFLOW" ] \
  || warn "no merge.workflow in $MAIN/.claude/backlog.json; a failed merge run will read as a slow merge"

# ---------------------------------------------------------------- wait --------
# The merge is asynchronous: labelling queues it and GitHub completes it when the
# required checks finish. Every failure path exits non-zero, so a timeout, an
# unmerged close or a dead merge run cannot be mistaken for success by a caller
# reading the code rather than the text.
#
# The run list is filtered by workflow and commit, not by event: a locally
# adapted merge workflow may not trigger on `pull_request_target`, and any run
# of the merge workflow on this head is a merge attempt or one of the jobs that
# follow a merge — which never get here, because MERGED is read first.
#
# `GET /pulls/{n}` says `closed` for a merged pull request and an abandoned one
# alike; `merged` is what tells them apart, so the state below is folded from
# the two into the three words the rest of this script reads.
state="" run=""
for i in $(seq 1 "$POLL_COUNT"); do
  read -r state head <<<"$(gh api "repos/$REPO/pulls/$PR" \
    --jq '"\(if .merged == true or .merged_at != null then "MERGED" elif .state == "closed" then "CLOSED" else "OPEN" end) \(.head.sha // "")"' \
    2>/dev/null || printf '')"
  case "$state" in
    MERGED) break ;;
    CLOSED) fail 1 "PR #$PR closed without merging" ;;
  esac

  if [ -n "$WORKFLOW" ] && [ -n "${head:-}" ]; then
    run=$(classify_runs "$(gh run list --repo "$REPO" --workflow "$WORKFLOW" --commit "$head" --limit 20 \
      --json databaseId,status,conclusion,headSha,createdAt,url 2>/dev/null)" "$head")
    [ $? -eq 1 ] \
      && fail 5 "the $WORKFLOW run on PR #$PR at ${head:0:12} ended '${run%%  *}', so auto merge was never armed and the pull request will not merge: ${run#*  } — read it with \`gh run view ${run##*/} --repo $REPO --log-failed\`"
  fi

  [ "$i" -lt "$POLL_COUNT" ] && sleep "$POLL_SECONDS"
done

if [ "$state" != MERGED ]; then
  # Say what the merge run looked like at the bound, so the re-run that follows
  # is not blind: armed and waiting on a check, still running, or no run at all.
  if [ -z "$WORKFLOW" ] || [ -z "${head:-}" ]; then
    note=''
  elif [ -n "$run" ]; then
    note="; the latest $WORKFLOW run on ${head:0:12} is '${run%%  *}' (${run#*  })"
  else
    note="; no $WORKFLOW run on ${head:0:12} has been reported — is the merge label on it?"
  fi
  fail 2 "PR #$PR is ${state:-unreadable} after $((POLL_COUNT * POLL_SECONDS))s$note; re-run to keep waiting"
fi

# Read once MERGED, and only then: before the merge, `merge_commit_sha` is the
# test merge GitHub keeps for an open pull request, which is not a commit that
# landed anywhere.
SHA=$(gh api "repos/$REPO/pulls/$PR" --jq '.merge_commit_sha // ""' 2>/dev/null || printf '')

# --------------------------------------------------------------- close --------
# The assertion this whole script exists for. A `Closes #N` line in the pull
# request body does register a closing reference — that part works — but nothing
# acts on it when a workflow's GITHUB_TOKEN performs the merge, so the issue is
# still open here and closing it is not a formality.
#
# The close is `PATCH /issues/{n}` with `state_reason: completed`, which is what
# `gh issue close` sent, and the comment is `POST /issues/{n}/comments`, made
# first so it reads above the close on the timeline as it always has. A comment
# that will not post is a WARN and the close still happens: the comment is a
# courtesy, and an issue left open is the one outcome this block exists to rule
# out.
issue_state() {
  gh api "repos/$REPO/issues/$ISSUE" --jq '.state // "" | ascii_upcase' 2>/dev/null || printf ''
}

istate=$(issue_state)
[ -n "$istate" ] || fail 3 "cannot read the state of issue #$ISSUE"

if [ "$istate" = OPEN ]; then
  comment="Implemented in #$PR"
  [ -n "$SHA" ] && comment="$comment, merged as $SHA"
  gh api --method POST "repos/$REPO/issues/$ISSUE/comments" -f body="$comment." >/dev/null 2>&1 \
    || warn "could not comment on issue #$ISSUE; closing it without the comment"
  gh api --method PATCH "repos/$REPO/issues/$ISSUE" \
    -f state=closed -f state_reason=completed >/dev/null \
    || fail 3 "the close of issue #$ISSUE was refused (PATCH repos/$REPO/issues/$ISSUE)"
fi

# Re-read rather than trusting the close. This is the check that turns a silent
# no-op into a non-zero exit.
istate=$(issue_state)
[ "$istate" = CLOSED ] || fail 3 "issue #$ISSUE is ${istate:-unreadable} after the close attempt"

# ------------------------------------------------------------- clean up -------
# Past this point the cycle has succeeded. Anything that fails below is untidy
# rather than broken, so it warns and the script still exits 0 — conflating a
# leftover directory with an issue that would not close would make the exit code
# useless for the one thing it is read for.
WT=$(git worktree list --porcelain | awk -v want="branch refs/heads/$BRANCH" '
  /^worktree /{ w = substr($0, 10) }
  $0 == want   { print w; exit }')

if [ -n "$WT" ]; then
  # Deliberately not --force. A dirty worktree at this point means work that
  # never reached the merged pull request, and discarding it silently is worse
  # than leaving a directory behind.
  git worktree remove "$WT" 2>/dev/null \
    || warn "could not remove the worktree at $WT; it may have uncommitted changes"
fi

# Only ever fast-forward a checkout that is ALREADY on the default branch. This
# script cleans up what the cycle created; the branch the operator left their
# main checkout on is not that, and silently moving it is a surprise waiting at
# the end of an unattended run. Nothing depends on the local ref anyway — step 2
# fetches and branches from origin/<default-branch> — so declining here costs
# nothing.
CURRENT=$(git symbolic-ref --quiet --short HEAD 2>/dev/null || printf '')
if [ "$CURRENT" != "$DEFAULT_BRANCH" ]; then
  warn "main checkout at $MAIN is on '${CURRENT:-a detached HEAD}', not $DEFAULT_BRANCH; leaving it alone"
elif [ -n "$(git status --porcelain)" ]; then
  warn "main checkout at $MAIN is dirty; skipping the update of $DEFAULT_BRANCH"
else
  git pull --quiet --ff-only 2>/dev/null \
    || warn "could not fast-forward $DEFAULT_BRANCH"
fi

# A stale *local* branch is what breaks a retry: `git worktree add -b <branch>`
# fails against an existing branch, and the retry then reports a name collision
# rather than anything real.
if git show-ref --verify --quiet "refs/heads/$BRANCH"; then
  git branch -D "$BRANCH" >/dev/null 2>&1 \
    || warn "could not delete the local branch $BRANCH"
fi

printf 'finish-issue: OK — PR #%s MERGED%s, issue #%s CLOSED, local cleanup done\n' \
  "$PR" "${SHA:+ as $SHA}" "$ISSUE"
