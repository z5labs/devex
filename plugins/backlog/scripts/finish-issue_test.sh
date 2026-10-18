#!/usr/bin/env bash
#
# finish-issue_test.sh — the fixture corpus for finish-issue.sh's reading of the
# merge workflow's runs.
#
# Every `--classify-runs` case is what `gh run list --json
# databaseId,status,conclusion,headSha,createdAt,url` prints for the merge
# workflow, and `--classify-runs` is the whole of the judgment the wait makes
# about it: everything else is polling and a bound. Offline, no network.
#
# The case that matters is the one that used to be invisible. Zaba505/backup
# #13: the merge workflow's run failed, so auto merge was never armed and the
# pull request could not merge — and the wait, reading only the pull request's
# state, saw OPEN and asked to be re-run until its whole budget was gone. A
# failed run must classify as failed, never as "still waiting".
#
# Run: plugins/backlog/scripts/finish-issue_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/finish-issue.sh"
[ -x "$SUT" ] || { printf 'finish-issue.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

# check <name> <expected exit> <expected stdout, or ''> <json> [classify args...]
check() {
  local name=$1 want=$2 want_out=$3 json=$4 got got_out
  shift 4
  got_out=$(printf '%s' "$json" | "$SUT" --classify-runs "$@")
  got=$?
  if [ "$got" = "$want" ] && { [ -z "$want_out" ] || [ "$got_out" = "$want_out" ]; }; then
    pass=$((pass + 1))
    printf '  ok   %-62s [exit %s]\n' "$name" "$got"
  else
    fail=$((fail + 1))
    printf '  FAIL %-62s want exit %s got %s\n' "$name" "$want" "$got"
    [ -n "$want_out" ] && printf '         want %s\n' "$want_out"
    printf '%s\n' "$got_out" | sed 's/^/         got  /'
  fi
}

HEAD=dfbf5c3a00000000000000000000000000000000
OLD=8ba2f52400000000000000000000000000000000

# r <id> <status> <conclusion> <sha> <created> — one run, as `gh run list` prints it
r() {
  printf '{"databaseId":%s,"status":"%s","conclusion":"%s","headSha":"%s","createdAt":"%s","url":"https://example.test/runs/%s"}' \
    "$1" "$2" "$3" "$4" "$5" "$1"
}

printf '\nfailed — the merge will never happen, so this must not read as waiting\n'

check 'the merge run failed' 1 "failure  https://example.test/runs/1" \
"[$(r 1 completed failure "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

check 'the merge run was cancelled' 1 "cancelled  https://example.test/runs/1" \
"[$(r 1 completed cancelled "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

check 'the merge run timed out' 1 '' \
"[$(r 1 completed timed_out "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

check 'the merge run never started' 1 '' \
"[$(r 1 completed startup_failure "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

check 'the merge run needs an approval nobody will give' 1 '' \
"[$(r 1 completed action_required "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

# `gh run list` prints newest first, but the verdict must not depend on it.
check 'the newest run decides, whatever order they arrive in' 1 "failure  https://example.test/runs/2" \
"[$(r 1 completed success "$HEAD" 2026-10-18T01:00:00Z), $(r 2 completed failure "$HEAD" 2026-10-18T02:00:00Z)]" --sha "$HEAD"

# A run where every job was skipped is some other label's `labeled` event, not
# a merge attempt; it must not hide the failure before it.
check 'a later all-skipped run does not hide the failure' 1 "failure  https://example.test/runs/1" \
"[$(r 2 completed skipped "$HEAD" 2026-10-18T02:00:00Z), $(r 1 completed failure "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

printf '\narmed — the merge is queued, keep waiting for it\n'

check 'the merge run succeeded' 0 "success  https://example.test/runs/1" \
"[$(r 1 completed success "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

# The label re-applied after a fix: the newer run is the one that counts.
check 'a newer success after a failure' 0 '' \
"[$(r 2 completed success "$HEAD" 2026-10-18T02:00:00Z), $(r 1 completed failure "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

printf '\nrunning — no verdict yet\n'

check 'the merge run is still in progress' 2 "in_progress  https://example.test/runs/1" \
"[$(r 1 in_progress '' "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

check 'the merge run is queued' 2 '' \
"[$(r 1 queued '' "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

check 'a re-run in progress after a failure' 2 '' \
"[$(r 2 in_progress '' "$HEAD" 2026-10-18T02:00:00Z), $(r 1 completed failure "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

# Only a conclusion known to mean "this will not merge" stops the cycle. Anything
# GitHub adds later waits, and the re-run budget still bounds it.
check 'an unrecognised conclusion is not a failure' 2 '' \
"[$(r 1 completed something_new "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

printf '\nnothing to classify — keep waiting, never conclude\n'

# The run is queued a few seconds after the label lands, so "no run" is a state
# every wait passes through.
check 'no run yet' 3 '' '[]' --sha "$HEAD"
check 'only all-skipped runs' 3 '' \
"[$(r 1 completed skipped "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"
check 'gh printed nothing' 3 '' '' --sha "$HEAD"
check 'gh printed an error rather than JSON' 3 '' 'HTTP 404: Not Found' --sha "$HEAD"
check 'an object rather than an array' 3 '' '{"message":"Not Found"}' --sha "$HEAD"

printf '\nanother commit — never inherited\n'

# A run against an earlier head failed for that head, and the push since may be
# exactly what fixed it.
check "the previous commit's failure is not this commit's" 3 '' \
"[$(r 1 completed failure "$OLD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

check "the previous commit's failure beside this commit's success" 0 '' \
"[$(r 2 completed failure "$OLD" 2026-10-18T02:00:00Z), $(r 1 completed success "$HEAD" 2026-10-18T01:00:00Z)]" --sha "$HEAD"

printf '\nusage\n'

usage_check() { # <name> <expected exit> <args...>
  local name=$1 want=$2; shift 2
  "$SUT" "$@" >/dev/null 2>&1 </dev/null
  local got=$?
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '  ok   %-62s [exit %s]\n' "$name" "$got"
  else
    fail=$((fail + 1)); printf '  FAIL %-62s want exit %s got %s\n' "$name" "$want" "$got"
  fi
}

usage_check 'no arguments' 4
usage_check 'only an issue number' 4 12
usage_check '--classify-runs needs --sha' 4 --classify-runs
usage_check '--classify-runs --sha needs a value' 4 --classify-runs --sha
usage_check '--classify-runs takes no stray argument' 4 --classify-runs --sha "$HEAD" extra

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
