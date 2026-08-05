#!/usr/bin/env bash
#
# await-checks.sh — wait for a pull request's checks to settle, bounded.
#
# Usage: await-checks.sh <pr-number>
#        await-checks.sh --classify        # classify a checks JSON array on stdin
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
# Exit codes:
#   0  every check has settled and none failed; stdout lists them
#   1  a check failed or was cancelled; stdout names it, with its link
#   2  checks are still running after the bound — re-run to keep waiting
#   3  no checks were ever reported: nothing gates this pull request
#   4  usage or precondition failure

set -uo pipefail

POLL_COUNT=${BACKLOG_CHECKS_POLL_COUNT:-20}
POLL_SECONDS=${BACKLOG_CHECKS_POLL_SECONDS:-15}

fail() { local code=$1; shift; printf 'await-checks: %s\n' "$*" >&2; exit "$code"; }

command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

# ------------------------------------------------------------ classifying -----
# One array of checks in, one verdict out. Prints the checks it is reporting on.
#
#   0  settled, none failed
#   1  at least one failed or was cancelled — printed, and nothing else is
#   2  at least one still pending, none failed
#   3  nothing to classify: no checks in the input, or it is not an array
classify() { # <checks json>
  local json=$1 bad

  printf '%s' "$json" | jq -e 'type == "array" and length > 0' >/dev/null 2>&1 || return 3

  bad=$(printf '%s' "$json" \
    | jq -r '.[] | select(.bucket == "fail" or .bucket == "cancel") | "\(.bucket)  \(.name)  \(.link // "")"')
  if [ -n "$bad" ]; then
    printf '%s\n' "$bad"
    return 1
  fi

  printf '%s' "$json" | jq -e 'all(.[]; .bucket != "pending")' >/dev/null 2>&1 || return 2

  printf '%s' "$json" | jq -r '.[] | "\(.bucket)  \(.name)  \(.link // "")"'
  return 0
}

if [ "${1:-}" = --classify ]; then
  [ $# -eq 1 ] || fail 4 "usage: await-checks.sh --classify  (JSON on stdin)"
  classify "$(cat)"
  exit $?
fi

# ---------------------------------------------------------------- waiting -----
[ $# -eq 1 ] || fail 4 "usage: await-checks.sh <pr-number>"
PR=$1
case "$PR" in ''|*[!0-9]*) fail 4 "pull request number must be an integer (got '$PR')" ;; esac

command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"

REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) \
  || fail 4 "cannot resolve the repository slug from gh"
[ -n "$REPO" ] || fail 4 "cannot resolve the repository slug from gh"

# Read the pull request once before polling. Without this a mistyped number
# polls for the whole bound and then reports exit 3 — "nothing gates this pull
# request" — which is a very wrong answer to give about a pull request that does
# not exist.
gh pr view "$PR" --repo "$REPO" --json number >/dev/null 2>&1 \
  || fail 4 "cannot read PR #$PR in $REPO"

# `gh pr checks` exits 8 while checks are pending and 1 when one has failed, so
# its exit code says nothing about whether the READ succeeded. The JSON is the
# thing to test, and classify() treats an empty or unparseable body as "not
# reported yet" rather than as an answer.
checks() {
  gh pr checks "$PR" --repo "$REPO" --json name,bucket,state,link 2>/dev/null
}

seen=0
for i in $(seq 1 "$POLL_COUNT"); do
  report=$(classify "$(checks)")
  case $? in
    0) printf '%s\n' "$report"; exit 0 ;;
    1) printf '%s\n' "$report"; fail 1 "a check on PR #$PR did not pass" ;;
    2) seen=1 ;;
  esac

  [ "$i" -lt "$POLL_COUNT" ] && sleep "$POLL_SECONDS"
done

[ "$seen" -eq 1 ] \
  || fail 3 "no checks reported on PR #$PR after $((POLL_COUNT * POLL_SECONDS))s; nothing gates this pull request"

fail 2 "checks on PR #$PR are still running after $((POLL_COUNT * POLL_SECONDS))s; re-run to keep waiting"
