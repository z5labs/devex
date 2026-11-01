#!/usr/bin/env bash
#
# finish-issue_test.sh — the fixture corpus for finish-issue.sh's reading of the
# merge workflow's runs, and the finish itself against a `gh` that answers from
# files.
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

printf '\nthe finish, against a gh that answers from files\n'

# The whole script, run against a scratch checkout and a `gh` that answers from
# files. Every GitHub call is REST under `repos/{owner}/{repo}/...` — the pull
# request from `GET /pulls/{n}`, the issue read, commented on and closed through
# `/issues/{n}` and `/issues/{n}/comments` — plus `gh run list`, which is
# REST-backed already. That is what a Claude Code cloud session's GitHub proxy
# serves. The stub sources the guard that refuses everything GraphQL-backed and
# logs it, and the last case here asserts nothing was refused.
SCRATCH=$(mktemp -d) || { printf 'cannot create a scratch directory\n' >&2; exit 1; }
trap 'rm -rf "$SCRATCH"' EXIT
mkdir -p "$SCRATCH/bin" "$SCRATCH/fx"
export GH_GUARD="$HERE/testdata/gh-guard.sh"
export GH_GUARD_LOG="$SCRATCH/graphql-calls"
: >"$GH_GUARD_LOG"
FX="$SCRATCH/fx"

# The main checkout is left on a branch that is not the default one, so the
# script's fast-forward is skipped with a WARN and never reaches the network.
REPO_DIR="$SCRATCH/repo"
git init -q "$REPO_DIR" >/dev/null 2>&1 || { printf 'cannot init a scratch repository\n' >&2; exit 1; }
git -C "$REPO_DIR" remote add origin https://github.com/z5labs/devex.git
mkdir -p "$REPO_DIR/.claude"
printf '{"merge":{"label":"auto-merge","workflow":"auto-merge.yaml"}}\n' >"$REPO_DIR/.claude/backlog.json"
git -C "$REPO_DIR" -c user.name=t -c user.email=t@example.test commit -q --allow-empty -m init
git -C "$REPO_DIR" checkout -q -b elsewhere

# `pull-7` is the pull request; `issue-state` the issue's state, which a PATCH
# that closes it rewrites unless `close-refused` exists; `comment-refused` makes
# the comment POST fail; `runs` is what `gh run list` prints. Writes are logged
# to `writes`, one line per call, with their fields.
cat >"$SCRATCH/bin/gh" <<'STUB'
#!/usr/bin/env bash
. "$GH_GUARD"
if [ "$1 $2" = 'run list' ]; then cat "$FX/runs" 2>/dev/null || printf '[]\n'; exit 0; fi
q="" path="" method=GET prev="" fields=""
for a in "$@"; do
  [ "$prev" = --jq ] && q=$a
  [ "$prev" = --method ] && method=$a
  [ "$prev" = -f ] && fields="$fields $a"
  case "$a" in repos/*) path=$a ;; esac
  prev=$a
done
[ "$1" = api ] && [ -n "$path" ] || { printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1; }
out() { if [ -n "$q" ]; then jq -r "$q"; else cat; fi; }
case "$method $path" in
  'GET repos/z5labs/devex')        printf '{"full_name":"z5labs/devex","default_branch":"main"}\n' | out ;;
  'GET repos/z5labs/devex/pulls/7') out <"$FX/pull-7" ;;
  'GET repos/z5labs/devex/issues/12')
    jq -n --arg s "$(cat "$FX/issue-state")" '{number: 12, state: $s}' | out ;;
  'POST repos/z5labs/devex/issues/12/comments')
    printf 'comment%s\n' "$fields" >>"$FX/writes"
    [ -f "$FX/comment-refused" ] && { printf 'gh: Resource not accessible (HTTP 403)\n' >&2; exit 1; }
    printf '{"id":1}\n' ;;
  'PATCH repos/z5labs/devex/issues/12')
    printf 'patch%s\n' "$fields" >>"$FX/writes"
    [ -f "$FX/close-refused" ] && { printf 'gh: Resource not accessible (HTTP 403)\n' >&2; exit 1; }
    printf 'closed\n' >"$FX/issue-state"
    printf '{"number":12,"state":"closed"}\n' ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$SCRATCH/bin/gh"

MERGED_AS=b20a9d66e8d1e449080327219bf864dca8ed6a1d

# pr <state> <merged true|false> — the pull request as `GET /pulls/{n}` answers it
pr() {
  local mc=null at=null
  [ "$2" = true ] && { mc="\"$MERGED_AS\""; at='"2026-11-01T16:47:32Z"'; }
  printf '{"number":7,"state":"%s","merged":%s,"merged_at":%s,"head":{"sha":"%s"},"merge_commit_sha":%s}\n' \
    "$1" "$2" "$at" "$HEAD" "$mc" >"$FX/pull-7"
}

fresh() { rm -f "$FX"/*; printf 'open\n' >"$FX/issue-state"; : >"$FX/writes"; }

# finish <name> <expected exit> [<substring the output must carry>]
finish() {
  local name=$1 want=$2 msg=${3:-} out rc
  out=$(cd "$REPO_DIR" && PATH="$SCRATCH/bin:$PATH" FX="$FX" \
    BACKLOG_MERGE_POLL_COUNT=1 BACKLOG_MERGE_POLL_SECONDS=0 "$SUT" 12 7 2>&1)
  rc=$?
  FIN_OUT=$out
  if [ "$rc" != "$want" ]; then
    fail=$((fail + 1)); printf '  FAIL %-62s want exit %s got %s: %s\n' "$name" "$want" "$rc" "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
  elif [ -n "$msg" ] && ! printf '%s' "$out" | grep -qF -- "$msg"; then
    fail=$((fail + 1)); printf '  FAIL %-62s exit %s but the output never says [%s]: %s\n' "$name" "$rc" "$msg" "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
  else
    pass=$((pass + 1)); printf '  ok   %-62s [exit %s]\n' "$name" "$rc"
  fi
}

# wrote <name> <extended regex the writes log must match>
wrote() {
  if grep -qE -- "$2" "$FX/writes"; then
    pass=$((pass + 1)); printf '  ok   %-62s [%s]\n' "$1" 'written'
  else
    fail=$((fail + 1)); printf '  FAIL %-62s writes were: %s\n' "$1" "$(tr '\n' ';' <"$FX/writes")"
  fi
}

fresh; pr closed true
finish 'merged, and the open issue is closed' 0 "MERGED as $MERGED_AS, issue #12 CLOSED"
wrote 'closed with state_reason completed' '^patch state=closed state_reason=completed$'
wrote 'and commented with the merge commit' "^comment body=Implemented in #7, merged as $MERGED_AS\.$"

# Re-running after a success is the guarded path: nothing is written twice.
pr closed true
: >"$FX/writes"
finish 'an issue already closed is not closed again' 0
if [ -s "$FX/writes" ]; then
  fail=$((fail + 1)); printf '  FAIL %-62s %s\n' 'and nothing is written' "$(tr '\n' ';' <"$FX/writes")"
else
  pass=$((pass + 1)); printf '  ok   %-62s [%s]\n' 'and nothing is written' 'no write'
fi

# `closed` with `merged: false` is an abandoned pull request, not a merge.
fresh; pr closed false
finish 'a pull request closed without merging' 1 'closed without merging'

# The comment is a courtesy. One that will not post must not leave the issue
# open, which is the outcome the whole close exists to rule out.
fresh; pr closed true; : >"$FX/comment-refused"
finish 'a comment that will not post still closes the issue' 0 'WARN could not comment on issue #12'
wrote 'and the close was still sent' '^patch state=closed'

fresh; pr closed true; : >"$FX/close-refused"
finish 'a close that is refused is exit 3' 3 'close of issue #12 was refused'

# Open, with the merge workflow's run on the head failed: auto merge was never
# armed, and waiting longer cannot change that.
fresh; pr open false
printf '[{"databaseId":9,"status":"completed","conclusion":"failure","headSha":"%s","createdAt":"2026-11-01T16:47:20Z","url":"https://example.test/runs/9"}]\n' "$HEAD" >"$FX/runs"
finish 'a failed merge run on the head is exit 5' 5 'auto merge was never armed'

fresh; pr open false
finish 'open with no merge run yet is a wait, not a verdict' 2 'is the merge label on it?'

printf '\nGraphQL\n'
if [ -s "$GH_GUARD_LOG" ]; then
  fail=$((fail + 1)); printf '  FAIL %-62s %s\n' 'no GraphQL-backed gh call was made' "$(tr '\n' ';' <"$GH_GUARD_LOG")"
else
  pass=$((pass + 1)); printf '  ok   %-62s [%s]\n' 'no GraphQL-backed gh call was made' 'none'
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
