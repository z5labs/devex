#!/usr/bin/env bash
#
# await-checks_test.sh — the fixture corpus for await-checks.sh's classification.
#
# Every case is a `gh pr checks --json name,bucket,state,link` response, fed to
# `--classify`, which is the whole of the script's judgment: everything else is
# polling and a bound. Offline, no network.
#
# The cases that matter are the ones where a wrong verdict is invisible. A red
# check sitting beside a pending one must classify as FAILED, not as "still
# waiting" — the cycle labels a pull request for auto merge on the strength of
# this exit code, so a failure read as pending is a wait that eventually times
# out and gets re-run, and a failure read as settled is a merge.
#
# Run: plugins/backlog/scripts/await-checks_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/await-checks.sh"
[ -x "$SUT" ] || { printf 'await-checks.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

# check <name> <expected exit> <expected stdout lines, or ''> <json>
check() {
  local name=$1 want=$2 want_out=$3 json=$4 got got_out
  got_out=$(printf '%s' "$json" | "$SUT" --classify)
  got=$?
  local lines
  lines=$(printf '%s\n' "$got_out" | grep -c .)
  if [ "$got" = "$want" ] && { [ -z "$want_out" ] || [ "$lines" = "$want_out" ]; }; then
    pass=$((pass + 1))
    printf '  ok   %-58s [exit %s]\n' "$name" "$got"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want exit %s got %s\n' "$name" "$want" "$got"
    printf '%s\n' "$got_out" | sed 's/^/         /'
  fi
}

printf '\nsettled\n'

check 'every check passed' 0 1 \
'[{"name":"CI Gate","bucket":"pass","state":"SUCCESS","link":"l1"}]'

# `skipping` is settled, not pending. The auto-merge workflow's own jobs are
# routinely skipped, and a run where one of them is skipped is a green run.
check 'a skipped check is settled' 0 2 \
'[{"name":"CI Gate","bucket":"pass","state":"SUCCESS","link":"l1"},
  {"name":"enable auto merge","bucket":"skipping","state":"","link":"l2"}]'

printf '\nfailed — these must not read as pending or as settled\n'

check 'a failed check' 1 1 \
'[{"name":"CI Gate","bucket":"fail","state":"FAILURE","link":"l1"}]'

# The one that would be invisible: `--fail-fast` existed because a red check
# does not turn green, and waiting out its pending siblings buys nothing.
check 'a failed check beside a pending one' 1 1 \
'[{"name":"CI Gate","bucket":"fail","state":"FAILURE","link":"l1"},
  {"name":"tests","bucket":"pending","state":"","link":"l2"}]'

check 'a cancelled check counts as failed' 1 1 \
'[{"name":"CI Gate","bucket":"cancel","state":"CANCELLED","link":"l1"},
  {"name":"tests","bucket":"pass","state":"SUCCESS","link":"l2"}]'

check 'only the failures are printed' 1 1 \
'[{"name":"a","bucket":"pass","state":"SUCCESS","link":"l1"},
  {"name":"b","bucket":"fail","state":"FAILURE","link":"l2"},
  {"name":"c","bucket":"pass","state":"SUCCESS","link":"l3"}]'

printf '\npending\n'

check 'everything still running' 2 '' \
'[{"name":"CI Gate","bucket":"pending","state":"","link":"l1"}]'

check 'one still running among green ones' 2 '' \
'[{"name":"a","bucket":"pass","state":"SUCCESS","link":"l1"},
  {"name":"b","bucket":"pending","state":"","link":"l2"}]'

printf '\nnothing to classify — the caller keeps polling, it does not conclude\n'

# `gh pr checks` prints nothing at all on a branch with no workflows, and the
# runs queue for a few seconds after a push, so "no checks" is a state the poll
# loop passes through on the way to a perfectly ordinary green run. It becomes
# exit 3 only when the bound is up.
check 'no checks reported' 3 '' '[]'
check 'gh printed nothing' 3 '' ''
check 'gh printed an error rather than JSON' 3 '' 'no checks reported on the branch'
check 'an object rather than an array' 3 '' '{"message":"Not Found"}'

printf '\nusage\n'

usage_check() { # <name> <expected exit> <args...>
  local name=$1 want=$2; shift 2
  "$SUT" "$@" >/dev/null 2>&1
  local got=$?
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '  ok   %-58s [exit %s]\n' "$name" "$got"
  else
    fail=$((fail + 1)); printf '  FAIL %-58s want exit %s got %s\n' "$name" "$want" "$got"
  fi
}

usage_check 'no arguments' 4
usage_check 'a pull request number that is not a number' 4 not-a-number
usage_check '--classify takes no other argument' 4 --classify 12 </dev/null

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
