#!/usr/bin/env bash
#
# await-checks_test.sh — the fixture corpus for await-checks.sh's classification,
# and the wait itself against a `gh` that answers from files.
#
# Every `--classify` case is an array of `{name,bucket,state,link,sha}` checks —
# the shape `--normalize` turns GitHub's check runs and commit statuses into —
# and `--classify` is the whole of the script's judgment: everything else is
# polling and a bound. The `--normalize` cases pin how GitHub's statuses and
# conclusions map onto those buckets. Offline, no network.
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

# check <name> <expected exit> <expected stdout lines, or ''> <json> [classify args...]
check() {
  local name=$1 want=$2 want_out=$3 json=$4 got got_out
  shift 4
  got_out=$(printf '%s' "$json" | "$SUT" --classify "$@")
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

printf '\nanother commit — never inherited, in either direction\n'

# z5labs/humus #676: right after the review fix was pushed, nothing had
# registered for the new head yet, and the read returned the previous head's
# settled runs. A stale green and a real green looked identical, so the wait
# exited 0 on a commit nobody had checked. Checks are judged only against the
# SHA the caller pushed; a check from any other commit is not there at all.
OLD=8ba2f52400000000000000000000000000000000
NEW=dfbf5c3a00000000000000000000000000000000

# c <name> <bucket> <sha> — one normalized check, for the cases below
c() { printf '{"name":"%s","bucket":"%s","state":"","link":"l","sha":"%s"}' "$1" "$2" "$3"; }

check "the previous commit's green is not this commit's" 3 '' \
"[$(c 'CI Gate' pass "$OLD")]" --sha "$NEW"

check "the previous commit's green beside this commit's pending" 2 '' \
"[$(c 'CI Gate' pass "$OLD"), $(c 'CI Gate' pending "$NEW")]" --sha "$NEW"

# The other direction: the failure that the fix push was answering does not
# fail the fix.
check "the previous commit's failure does not fail this one" 0 1 \
"[$(c 'CI Gate' fail "$OLD"), $(c 'CI Gate' pass "$NEW")]" --sha "$NEW"

check 'a check that names no commit is not this commit'"'"'s' 3 '' \
'[{"name":"CI Gate","bucket":"pass","state":"SUCCESS","link":"l1"}]' --sha "$NEW"

printf '\nincomplete — a required check that has not registered is outstanding\n'

# The second window: the new head has some of its checks but not all. A fast
# job passes before the gate job — or a whole workflow — registers, and every
# check on the page is green. The branch's required checks are the set GitHub
# itself will wait for, so one of them missing is pending, not settled.
check 'only a fast check, the required one not registered yet' 2 '' \
"[$(c 'CI files changed' pass "$NEW")]" --sha "$NEW" --require 'CI Gate'

check 'nothing registered yet, but a check is required' 2 '' \
'[]' --sha "$NEW" --require 'CI Gate'

check 'the required check registered on the previous commit only' 2 '' \
"[$(c 'CI Gate' pass "$OLD"), $(c 'CI files changed' pass "$NEW")]" --sha "$NEW" --require 'CI Gate'

check 'every required check present and passed' 0 2 \
"[$(c 'CI files changed' pass "$NEW"), $(c 'CI Gate' pass "$NEW")]" --sha "$NEW" --require 'CI Gate'

check 'one of two required checks still missing' 2 '' \
"[$(c 'CI Gate' pass "$NEW")]" --sha "$NEW" --require 'CI Gate' --require 'CodeQL'

# GitHub counts a skipped required check as satisfied, and so does the wait.
check 'a skipped required check is present' 0 1 \
"[$(c 'CI Gate' skipping "$NEW")]" --sha "$NEW" --require 'CI Gate'

# Fail-fast still wins: a red check is red whatever has not registered yet.
check 'a failure while a required check is missing' 1 1 \
"[$(c tests fail "$NEW")]" --sha "$NEW" --require 'CI Gate'

printf '\nnormalizing — GitHub check runs and commit statuses onto buckets\n'

# normalize_check <name> <expected compact JSON> <jq projection> <json>
normalize_check() {
  local name=$1 want=$2 proj=$3 json=$4 got
  got=$(printf '%s' "$json" | "$SUT" --normalize 2>&1 | jq -c "$proj" 2>&1)
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '  ok   %s\n' "$name"
  else
    fail=$((fail + 1)); printf '  FAIL %s\n         want %s\n         got  %s\n' "$name" "$want" "$got"
  fi
}

# run <status> <conclusion as JSON> — one REST check run on $NEW
run() {
  printf '{"name":"r","status":"%s","conclusion":%s,"head_sha":"%s","html_url":"u"}' "$1" "$2" "$NEW"
}

# `stale` is GitHub giving up on a run that never finished. It is not a pass,
# and an unrecognised conclusion is not one either: both wait, never settle.
normalize_check 'every check-run status and conclusion maps to a bucket' \
'["pending","pending","pending","pending","pass","skipping","skipping","fail","fail","fail","fail","cancel","pending","pending"]' \
'[.[].bucket]' \
"{\"check_runs\":[$(run queued null),$(run in_progress null),$(run waiting null),$(run requested null),
  $(run completed '"success"'),$(run completed '"neutral"'),$(run completed '"skipped"'),
  $(run completed '"failure"'),$(run completed '"timed_out"'),$(run completed '"action_required"'),
  $(run completed '"startup_failure"'),$(run completed '"cancelled"'),$(run completed '"stale"'),
  $(run completed '"something_new"')],
  \"status\":{\"sha\":\"$NEW\",\"statuses\":[]}}"

normalize_check 'commit statuses map to buckets and carry the combined status sha' \
"[[\"pass\",\"$NEW\"],[\"fail\",\"$NEW\"],[\"fail\",\"$NEW\"],[\"pending\",\"$NEW\"]]" \
'[.[] | [.bucket, .sha]]' \
"{\"check_runs\":[],\"status\":{\"sha\":\"$NEW\",\"statuses\":[
  {\"context\":\"a\",\"state\":\"success\",\"target_url\":\"u\"},
  {\"context\":\"b\",\"state\":\"failure\",\"target_url\":\"u\"},
  {\"context\":\"c\",\"state\":\"error\",\"target_url\":null},
  {\"context\":\"d\",\"state\":\"pending\",\"target_url\":\"u\"}]}}"

# Each check names the commit GitHub says it ran on, not the one that was asked
# for — that is what lets `--classify --sha` refuse a check from anywhere else.
normalize_check 'a check carries its own commit, name and link' \
"[{\"name\":\"CI Gate\",\"bucket\":\"pass\",\"state\":\"SUCCESS\",\"link\":\"u1\",\"sha\":\"$OLD\"},{\"name\":\"ctx\",\"bucket\":\"pending\",\"state\":\"PENDING\",\"link\":\"\",\"sha\":\"$NEW\"}]" \
'.' \
"{\"check_runs\":[{\"name\":\"CI Gate\",\"status\":\"completed\",\"conclusion\":\"success\",\"head_sha\":\"$OLD\",\"html_url\":\"u1\"}],
  \"status\":{\"sha\":\"$NEW\",\"statuses\":[{\"context\":\"ctx\",\"state\":\"pending\",\"target_url\":null}]}}"

normalize_check 'nothing registered is an empty array' '[]' '.' \
"{\"check_runs\":[],\"status\":{\"sha\":\"$NEW\",\"statuses\":[]}}"

# A read that failed must not normalize to "nothing registered" either — it
# yields no array, which `--classify` already treats as not reported yet.
normalize_check 'an unreadable response is not an array' '' '.' 'Not Found'

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
usage_check '--classify takes no stray argument' 4 --classify 12 </dev/null
usage_check '--classify --sha needs a value' 4 --classify --sha </dev/null
usage_check '--classify --require needs a value' 4 --classify --require </dev/null
usage_check '--normalize takes no argument' 4 --normalize 12 </dev/null
usage_check 'a SHA that is not a full hex SHA' 4 12 dfbf5c3a
usage_check 'more than a pull request and a SHA' 4 12 "$NEW" extra

printf '\nthe wait, against a gh that answers from files\n'

# Everything above is the judgment; this is the plumbing that feeds it, run
# whole against a scratch checkout and a `gh` that answers from files. Every read
# is REST under `repos/{owner}/{repo}/...` — the pull request's base and head from
# `GET /pulls/{n}`, the required checks, the commit's check runs and statuses —
# because that is what a Claude Code cloud session's GitHub proxy serves. The
# stub sources the guard that refuses everything GraphQL-backed and logs it, and
# the last case here asserts nothing was refused.
SCRATCH=$(mktemp -d) || { printf 'cannot create a scratch directory\n' >&2; exit 1; }
trap 'rm -rf "$SCRATCH"' EXIT
mkdir -p "$SCRATCH/bin" "$SCRATCH/fx"
export GH_GUARD="$HERE/testdata/gh-guard.sh"
export GH_GUARD_LOG="$SCRATCH/graphql-calls"
: >"$GH_GUARD_LOG"

git init -q "$SCRATCH/repo" >/dev/null 2>&1 || { printf 'cannot init a scratch repository\n' >&2; exit 1; }
git -C "$SCRATCH/repo" remote add origin git@github.com:z5labs/devex.git

# Routes, each answered from a file in $FX; an absent file is GitHub's 404.
cat >"$SCRATCH/bin/gh" <<'STUB'
#!/usr/bin/env bash
. "$GH_GUARD"
q="" path="" prev=""
for a in "$@"; do
  [ "$prev" = --jq ] && q=$a
  case "$a" in repos/*) path=$a ;; esac
  prev=$a
done
[ "$1" = api ] && [ -n "$path" ] || { printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1; }
route=${path%%\?*}
case "$route" in
  repos/z5labs/devex)                         f=repo ;;
  repos/*/pulls/*)                            f="pull-${route##*/}" ;;
  repos/*/rules/branches/*)                   f=rules ;;
  repos/*/branches/*/protection/*)            f=protection ;;
  repos/*/commits/*/check-runs)               f=check-runs ;;
  repos/*/commits/*/status)                   f=status ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
[ -f "$FX/$f" ] || { printf '{"message":"Not Found"}'; printf 'gh: Not Found (HTTP 404)\n' >&2; exit 1; }
if [ -n "$q" ]; then jq -r "$q" "$FX/$f"; else cat "$FX/$f"; fi
STUB
chmod +x "$SCRATCH/bin/gh"

FX="$SCRATCH/fx"
printf '{"full_name":"z5labs/devex","default_branch":"main"}\n' >"$FX/repo"
printf '[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"CI Gate"}]}}]\n' >"$FX/rules"

# pull <number> <head sha> — the pull request as `GET /pulls/{n}` answers it
pull() { printf '{"number":%s,"state":"open","merged":false,"base":{"ref":"main"},"head":{"sha":"%s"}}\n' "$1" "$2" >"$FX/pull-$1"; }
# runs <check run json>... — the commit's check runs
runs() { printf '{"total_count":%s,"check_runs":[%s]}\n' "$#" "$(IFS=,; printf '%s' "$*")" >"$FX/check-runs"; }
# cr <name> <status> <conclusion|null> [sha] — one REST check run
cr() { printf '{"name":"%s","status":"%s","conclusion":%s,"head_sha":"%s","html_url":"u"}' "$1" "$2" "$3" "${4:-$NEW}"; }
printf '{"sha":"%s","statuses":[]}\n' "$NEW" >"$FX/status"

# checke <name> <expected exit> <args...>
checke() {
  local name=$1 want=$2 out rc; shift 2
  out=$(cd "$SCRATCH/repo" && PATH="$SCRATCH/bin:$PATH" FX="$FX" \
    BACKLOG_CHECKS_POLL_COUNT=1 BACKLOG_CHECKS_POLL_SECONDS=0 "$SUT" "$@" 2>&1)
  rc=$?
  if [ "$rc" = "$want" ]; then
    pass=$((pass + 1)); printf '  ok   %-58s [exit %s]\n' "$name" "$rc"
  else
    fail=$((fail + 1)); printf '  FAIL %-58s want exit %s got %s: %s\n' "$name" "$want" "$rc" "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
  fi
}

pull 12 "$NEW"
runs "$(cr 'CI Gate' completed '"success"')" "$(cr tests completed '"success"')"
checke 'every check on the pushed head passed' 0 12 "$NEW"

runs "$(cr 'CI Gate' completed '"failure"')"
checke 'a failed check on the pushed head' 1 12 "$NEW"

# The required check, read from `rules/branches`, has not registered yet.
runs "$(cr tests completed '"success"')"
checke 'a required check not registered yet is outstanding' 2 12 "$NEW"

# The pull request's head, from `GET /pulls/{n}`, is still the previous commit.
pull 12 "$OLD"
runs "$(cr 'CI Gate' completed '"success"' "$OLD")"
checke "the head is not the pushed commit yet" 5 12 "$NEW"

# A pull request that does not exist is a precondition failure, read once before
# any polling, not five minutes of "nothing gates this pull request".
checke 'a pull request that does not exist' 4 99 "$NEW"

printf '\nGraphQL\n'
if [ -s "$GH_GUARD_LOG" ]; then
  fail=$((fail + 1)); printf '  FAIL %-58s %s\n' 'no GraphQL-backed gh call was made' "$(tr '\n' ';' <"$GH_GUARD_LOG")"
else
  pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' 'no GraphQL-backed gh call was made' 'none'
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
