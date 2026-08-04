#!/usr/bin/env bash
#
# finish-issue.sh — everything after the merge label, as one call.
#
# Usage: finish-issue.sh <issue-number> <pr-number> [branch]
#        branch defaults to issue-<issue-number>
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
# Exit codes:
#   0  merged and the issue is closed (cleanup warnings, if any, go to stderr)
#   1  the pull request closed without merging
#   2  timed out waiting for the merge
#   3  the issue would not close
#   4  usage or precondition failure

set -uo pipefail

POLL_COUNT=${BACKLOG_MERGE_POLL_COUNT:-40}
POLL_SECONDS=${BACKLOG_MERGE_POLL_SECONDS:-15}

fail() { local code=$1; shift; printf 'finish-issue: %s\n' "$*" >&2; exit "$code"; }
warn() { printf 'finish-issue: WARN %s\n' "$*" >&2; }

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
# point this at a repository it no longer describes.
read -r REPO DEFAULT_BRANCH <<<"$(gh repo view --json nameWithOwner,defaultBranchRef \
  --jq '"\(.nameWithOwner) \(.defaultBranchRef.name)"' 2>/dev/null)"
[ -n "${REPO:-}" ] && [ -n "${DEFAULT_BRANCH:-}" ] \
  || fail 4 "cannot resolve the repository slug and default branch from gh"

# ---------------------------------------------------------------- wait --------
# The merge is asynchronous: labelling queues it and GitHub completes it when the
# required checks finish. Both failure paths exit non-zero, so a timeout or an
# unmerged close cannot be mistaken for success by a caller reading the code
# rather than the text.
state=""
for _ in $(seq 1 "$POLL_COUNT"); do
  state=$(gh pr view "$PR" --repo "$REPO" --json state --jq .state 2>/dev/null || printf '')
  case "$state" in
    MERGED) break ;;
    CLOSED) fail 1 "PR #$PR closed without merging" ;;
  esac
  sleep "$POLL_SECONDS"
done
[ "$state" = MERGED ] \
  || fail 2 "PR #$PR is ${state:-unreadable} after $((POLL_COUNT * POLL_SECONDS))s; re-run to keep waiting"

SHA=$(gh pr view "$PR" --repo "$REPO" --json mergeCommit --jq '.mergeCommit.oid // ""' 2>/dev/null || printf '')

# --------------------------------------------------------------- close --------
# The assertion this whole script exists for. A `Closes #N` line in the pull
# request body does register a closing reference — that part works — but nothing
# acts on it when a workflow's GITHUB_TOKEN performs the merge, so the issue is
# still open here and closing it is not a formality.
istate=$(gh issue view "$ISSUE" --repo "$REPO" --json state --jq .state 2>/dev/null || printf '')
[ -n "$istate" ] || fail 3 "cannot read the state of issue #$ISSUE"

if [ "$istate" = OPEN ]; then
  comment="Implemented in #$PR"
  [ -n "$SHA" ] && comment="$comment, merged as $SHA"
  gh issue close "$ISSUE" --repo "$REPO" --comment "$comment." >/dev/null \
    || fail 3 "gh issue close failed for #$ISSUE"
fi

# Re-read rather than trusting the close. This is the check that turns a silent
# no-op into a non-zero exit.
istate=$(gh issue view "$ISSUE" --repo "$REPO" --json state --jq .state 2>/dev/null || printf '')
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
