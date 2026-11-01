#!/usr/bin/env bash
#
# await-checks.sh — wait for a pull request's checks to settle, bounded.
#
# Usage: await-checks.sh <pr-number> [<head-sha>]
#        await-checks.sh --classify [--sha <sha>] [--require <name>]...
#                                          # classify a checks JSON array on stdin
#        await-checks.sh --normalize       # GitHub check runs + statuses on stdin
#                                          # -> the array --classify reads
#
# <head-sha> is the commit just pushed, and defaults to `git rev-parse HEAD` in
# the current directory — so run it from the worktree that pushed.
#
# Why this is a script and not `gh pr checks --watch`.
#
# `--watch` blocks for as long as CI takes and has no bound of its own, which
# left the cycle two bad options. Run it in the foreground and the call can die
# mid-wait — and a wait that never finished looks exactly like a pull request
# whose checks failed. Hand it to a background wait instead and the result
# arrives after the turn that armed it, which is the failure that deadlocked the
# loop: the worker of `backlog:run-backlog` ends its turn by RETURNING to its
# caller, so a wait reporting after the turn reports to nobody, and the resumed
# worker holds for an event that can never arrive.
#
# So the wait is bounded here instead, and "not finished yet" is an exit code
# rather than a killed process. Re-running resumes it — the state lives on
# GitHub and this only reads it — so a caller loops on exit 2 within one turn
# instead of waiting across turns.
#
# The classification is fail-fast for the same reason `--fail-fast` was: a red
# check does not turn green, and a red one sitting beside a pending one must not
# read as "still waiting". `--classify` exposes exactly that decision on stdin,
# with no network, which is what `await-checks_test.sh` exercises.
#
# Why the checks are read per commit, not per pull request.
#
# The wait used to read `gh pr checks <pr>`, which names no commit. Right after
# a push there is a window where nothing has registered for the new head, and in
# that window the read returned the PREVIOUS head's settled runs: z5labs/humus
# #676 exited 0 straight after its review fix was pushed, on the checks of the
# commit the fix replaced. A stale green and a real green looked identical, and
# the cycle pushes after review on its normal path, so this was not an edge.
#
# So the wait is tied to one SHA. It confirms the pull request's head IS that
# SHA, reads the check runs and commit statuses of that commit, and classifies
# only checks that say they ran on it. Nothing registered yet is "not reported
# yet", never a pass inherited from an earlier commit.
#
# The neighbouring window — some of the new head's checks registered, not all,
# and the fast ones green — is closed by the default branch's required status
# checks (rulesets and classic protection, whichever are readable). That is the
# set GitHub itself waits for before the merge label can merge anything, so a
# required check that has not registered is pending. A check that is neither
# required nor registered yet stays invisible; the previous head's check set
# was the other candidate, and it was rejected because it carries event-bound
# runs — a review bot, a `labeled` job — that a new push never recreates, which
# would make every post-review wait run out its budget.
#
# Exit codes:
#   0  every check on the commit has settled and none failed; stdout lists them
#   1  a check failed or was cancelled; stdout names it, with its link
#   2  checks are still running, or a required one has not registered, after
#      the bound — re-run to keep waiting; stderr names what is outstanding
#   3  no checks were ever reported on the commit and none is required: nothing
#      gates this pull request
#   4  usage or precondition failure
#   5  the pull request's head never became <head-sha> within the bound: it was
#      not pushed, was pushed elsewhere, or the branch has moved on since

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

POLL_COUNT=${BACKLOG_CHECKS_POLL_COUNT:-20}
POLL_SECONDS=${BACKLOG_CHECKS_POLL_SECONDS:-15}

fail() { local code=$1; shift; printf 'await-checks: %s\n' "$*" >&2; exit "$code"; }

command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

# ------------------------------------------------------------ classifying -----
# One array of checks in, one verdict out. Prints the checks it is reporting on.
#
# With a SHA, only checks whose `.sha` is that SHA exist; a check from any other
# commit — or one naming no commit — is dropped before anything is judged, so it
# can neither pass nor fail this one. Each required name must appear among what
# is left; one that does not is outstanding, exactly like a pending check.
#
#   0  settled, none failed, every required check present
#   1  at least one failed or was cancelled — printed, and nothing else is
#   2  at least one pending or required-but-missing, none failed — printed
#   3  nothing to classify: no checks in the input and none required, or the
#      input is not an array
classify() { # <checks json> [<sha>] [<required names, as a JSON array>]
  local json=$1 sha=${2:-} required=${3:-[]} bad outstanding

  printf '%s' "$json" | jq -e 'type == "array"' >/dev/null 2>&1 || return 3

  if [ -n "$sha" ]; then
    json=$(printf '%s' "$json" | jq -c --arg sha "$sha" '[.[] | select(.sha == $sha)]')
  fi

  bad=$(printf '%s' "$json" \
    | jq -r '.[] | select(.bucket == "fail" or .bucket == "cancel") | "\(.bucket)  \(.name)  \(.link // "")"')
  if [ -n "$bad" ]; then
    printf '%s\n' "$bad"
    return 1
  fi

  outstanding=$(printf '%s' "$json" | jq -r --argjson req "$required" '
    (.[] | select(.bucket == "pending") | "pending  \(.name)  \(.link // "")"),
    (($req - [.[].name]) | .[] | "expected  \(.)  (required; not registered on this commit yet)")')
  if [ -n "$outstanding" ]; then
    printf '%s\n' "$outstanding"
    return 2
  fi

  printf '%s' "$json" | jq -e 'length > 0' >/dev/null 2>&1 || return 3

  printf '%s' "$json" | jq -r '.[] | "\(.bucket)  \(.name)  \(.link // "")"'
  return 0
}

# ------------------------------------------------------------ normalizing -----
# `{"check_runs": [...], "status": {...}}` in — the REST check runs of a commit
# and its combined status — and one `{name,bucket,state,link,sha}` array out.
# Every check names the commit GitHub says it ran on (`head_sha`, or the
# combined status's `sha`), never the one that was asked for.
#
# The buckets follow `gh pr checks`, except that `startup_failure` is a failure
# rather than falling through to pending. Anything unrecognised — `stale`
# included — is pending: a wait that runs out is recoverable, a pass is not.
normalize() {
  jq -c '
    def run_bucket:
      if .status != "completed" then "pending"
      elif .conclusion == "success" then "pass"
      elif .conclusion == "neutral" or .conclusion == "skipped" then "skipping"
      elif .conclusion == "cancelled" then "cancel"
      elif (.conclusion | IN("failure", "timed_out", "action_required", "startup_failure")) then "fail"
      else "pending" end;
    def status_bucket:
      if .state == "success" then "pass"
      elif .state == "failure" or .state == "error" then "fail"
      else "pending" end;
    [ (.check_runs // [])[]
      | { name, bucket: run_bucket,
          state: ((if .status == "completed" then .conclusion else .status end) // "" | ascii_upcase),
          link: (.html_url // ""), sha: .head_sha } ]
    + [ .status.sha as $sha | (.status.statuses // [])[]
      | { name: .context, bucket: status_bucket, state: (.state // "" | ascii_upcase),
          link: (.target_url // ""), sha: $sha } ]' 2>/dev/null
}

if [ "${1:-}" = --classify ]; then
  shift
  sha='' required='[]'
  while [ $# -gt 0 ]; do
    case $1 in
      --sha)     [ $# -ge 2 ] || fail 4 "--sha needs a value"; sha=$2; shift 2 ;;
      --require) [ $# -ge 2 ] || fail 4 "--require needs a value"
                 required=$(jq -c --arg n "$2" '. + [$n]' <<<"$required"); shift 2 ;;
      *) fail 4 "usage: await-checks.sh --classify [--sha <sha>] [--require <name>]...  (JSON on stdin)" ;;
    esac
  done
  classify "$(cat)" "$sha" "$required"
  exit $?
fi

if [ "${1:-}" = --normalize ]; then
  [ $# -eq 1 ] || fail 4 "usage: await-checks.sh --normalize  (JSON on stdin)"
  normalize || exit 3
  exit 0
fi

# ---------------------------------------------------------------- waiting -----
{ [ $# -eq 1 ] || [ $# -eq 2 ]; } || fail 4 "usage: await-checks.sh <pr-number> [<head-sha>]"
PR=$1
case "$PR" in ''|*[!0-9]*) fail 4 "pull request number must be an integer (got '$PR')" ;; esac

if [ $# -eq 2 ]; then
  SHA=$2
else
  SHA=$(git rev-parse HEAD 2>/dev/null) \
    || fail 4 "no <head-sha> given and the current directory is not a git checkout; pass the SHA you pushed"
fi
# Full SHAs only: the comparison with the pull request's head is an equality,
# and an abbreviation would never be equal to anything.
[[ $SHA =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] \
  || fail 4 "head SHA must be a full lowercase hex SHA (got '$SHA')"

command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"

# From the origin remote, confirmed over REST. Every read below is REST against
# `repos/{owner}/{repo}/...`, which is what a Claude Code cloud session's GitHub
# proxy serves; `gh pr view` and `gh repo view` are GraphQL-backed, and it
# refuses those.
REPO=$("$HERE/repo.sh") || fail 4 "cannot resolve the repository; see the repo: line above"
REPO=${REPO%%$'\t'*}
[ -n "$REPO" ] || fail 4 "cannot resolve the repository; repo.sh printed no slug"

# Read the pull request once before polling. Without this a mistyped number
# polls for the whole bound and then reports exit 3 — "nothing gates this pull
# request" — which is a very wrong answer to give about a pull request that does
# not exist.
BASE=$(gh api "repos/$REPO/pulls/$PR" --jq '.base.ref // ""' 2>/dev/null) \
  || fail 4 "cannot read PR #$PR in $REPO"
[ -n "$BASE" ] || fail 4 "cannot read PR #$PR in $REPO"

# The required checks of the branch the pull request merges into, read once.
# Rulesets need no admin rights; classic protection does, and a 403 or a 404
# there means only that it adds nothing. Either read failing leaves its half
# empty, which falls back to judging what has registered — never to a hang.
required=$(
  {
    gh api "repos/$REPO/rules/branches/$BASE?per_page=100" \
      --jq '[.[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context]' 2>/dev/null
    gh api "repos/$REPO/branches/$BASE/protection/required_status_checks" --jq '.contexts' 2>/dev/null
  } | jq -sc 'map(select(type == "array")) | add // [] | unique' 2>/dev/null
)
[ -n "$required" ] || required='[]'

# The pull request's head right now. GitHub updates it shortly after a push, so
# a mismatch is first read as "not there yet" and only becomes exit 5 at the
# bound.
head_sha() {
  gh api "repos/$REPO/pulls/$PR" --jq '.head.sha // ""' 2>/dev/null
}

# The commit's own check runs and statuses — not the pull request's rollup. A
# failed read yields no array, which classify() treats as "not reported yet".
checks() {
  local runs status
  runs=$(gh api --paginate "repos/$REPO/commits/$SHA/check-runs?per_page=100" --jq '.check_runs[]' 2>/dev/null \
    | jq -sc .) || return
  status=$(gh api "repos/$REPO/commits/$SHA/status?per_page=100" 2>/dev/null) || return
  printf '{"check_runs":%s,"status":%s}' "$runs" "$status" | normalize
}

seen=0 head='' report=''
for i in $(seq 1 "$POLL_COUNT"); do
  head=$(head_sha)
  if [ "$head" = "$SHA" ]; then
    report=$(classify "$(checks)" "$SHA" "$required")
    case $? in
      0) printf '%s\n' "$report"; exit 0 ;;
      1) printf '%s\n' "$report"; fail 1 "a check on PR #$PR at ${SHA:0:12} did not pass" ;;
      2) seen=1 ;;
    esac
  fi

  [ "$i" -lt "$POLL_COUNT" ] && sleep "$POLL_SECONDS"
done

BOUND=$((POLL_COUNT * POLL_SECONDS))

[ "$head" = "$SHA" ] \
  || fail 5 "PR #$PR's head is ${head:-unreadable}, not $SHA, after ${BOUND}s; push that commit, or pass the SHA that was pushed"

[ "$seen" -eq 1 ] \
  || fail 3 "no checks reported on PR #$PR at ${SHA:0:12} after ${BOUND}s, and none is required; nothing gates this pull request"

printf '%s\n' "$report" >&2
fail 2 "checks on PR #$PR at ${SHA:0:12} are still outstanding after ${BOUND}s; re-run to keep waiting"
