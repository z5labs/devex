#!/usr/bin/env bash
#
# auto-merge_test.sh — the fixture corpus for the two merge-path steps in
# assets/auto-merge.yaml.
#
# The steps are not copied here. Each case pulls the `run:` script of a named
# step out of the asset and runs it under `bash -e`, the shell GitHub uses for a
# step with no `shell:`. `gh`, `date` and `sleep` are fakes on PATH, and the
# clock moves only when the script sleeps. So what passes here is the text an
# installed workflow runs, with no network and no waiting.
#
# The case that matters is the one that used to be impossible. Zaba505/backup is
# a private repository on GitHub Free: auto merge cannot be turned on there, so
# `gh pr merge --auto` failed and the operator had to fork the merge step by
# hand. A repository that reports `autoMergeAllowed: false` must take the gated
# path, and that path must merge only once every other check has passed.
#
# Needs python3 with PyYAML to read the asset, and jq.
#
# Run: plugins/backlog/scripts/auto-merge_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ASSET="$HERE/../assets/auto-merge.yaml"
[ -f "$ASSET" ] || { printf 'no asset at %s\n' "$ASSET" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { printf 'jq is not on PATH\n' >&2; exit 1; }
python3 -c 'import yaml' 2>/dev/null || { printf 'python3 with PyYAML is required to read the asset\n' >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# yq <python expression over `d`, the parsed asset> — prints the result
yq() {
  python3 -c '
import sys, yaml
d = yaml.safe_load(open(sys.argv[1]))
v = eval(sys.argv[2])
print(v if isinstance(v, str) else yaml.safe_dump(v, default_flow_style=True).strip())
' "$ASSET" "$1"
}

step() { # <step name> — the step as a dict expression
  printf '[s for s in d["jobs"]["enable-auto-merge"]["steps"] if s.get("name") == "%s"][0]' "$1"
}

MODE_SCRIPT="$WORK/mode.sh"
GATE_SCRIPT="$WORK/gate.sh"
yq "$(step 'Read whether native auto merge is allowed')[\"run\"]" >"$MODE_SCRIPT"
yq "$(step 'Merge once checks pass')[\"run\"]" >"$GATE_SCRIPT"

# ------------------------------------------------------------------ fakes -----
# Everything a fake answers comes from files under $FAKE, and every gh call is
# logged to $FAKE/calls as `<token> <args>`, so a case can assert which token
# read and which one merged.
BIN="$WORK/bin"
mkdir -p "$BIN"

cat >"$BIN/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s %s\n' "${GH_TOKEN:-}" "$*" >>"$FAKE/calls"
case "$1 $2" in
  'api graphql')
    cat "$FAKE/graphql" 2>/dev/null
    exit "$(cat "$FAKE/graphql.exit" 2>/dev/null || echo 0)" ;;
  'pr view')
    cat "$FAKE/head"; echo; exit 0 ;;
  'pr checks')
    # One fixture per read, the last one repeating: checks.1, checks.2, ...
    n=$(( $(cat "$FAKE/checks.n" 2>/dev/null || echo 0) + 1 ))
    echo "$n" >"$FAKE/checks.n"
    while [ "$n" -gt 1 ] && [ ! -e "$FAKE/checks.$n" ]; do n=$((n - 1)); done
    cat "$FAKE/checks.$n" 2>/dev/null
    cat "$FAKE/checks.$n.err" >&2 2>/dev/null
    exit "$(cat "$FAKE/checks.$n.exit" 2>/dev/null || echo 0)" ;;
  'pr merge')
    printf '%s\n' "$*" >>"$FAKE/merged"
    exit "$(cat "$FAKE/merge.exit" 2>/dev/null || echo 0)" ;;
esac
echo "fake gh: unexpected call: $*" >&2
exit 99
EOF

cat >"$BIN/date" <<'EOF'
#!/usr/bin/env bash
cat "$FAKE/clock"
EOF

cat >"$BIN/sleep" <<'EOF'
#!/usr/bin/env bash
echo $(( $(cat "$FAKE/clock") + $1 )) >"$FAKE/clock"
echo "$1" >>"$FAKE/sleeps"
EOF
chmod +x "$BIN/gh" "$BIN/date" "$BIN/sleep"

HEAD=dfbf5c3a00000000000000000000000000000000
NEW=8ba2f52400000000000000000000000000000000

fresh() { # a clean fake world for one case
  FAKE=$(mktemp -d "$WORK/case.XXXX")
  export FAKE
  echo 1000 >"$FAKE/clock"
  echo "$HEAD" >"$FAKE/head"
  : >"$FAKE/calls"
}

# c <bucket> <workflow> <name> — one check, as `gh pr checks --json workflow,name,bucket` prints it
c() { printf '{"bucket":"%s","workflow":"%s","name":"%s"}' "$1" "$2" "$3"; }

checks() { # <read number> <json> [stderr] [exit]
  printf '%s' "$2" >"$FAKE/checks.$1"
  [ -n "${3:-}" ] && printf '%s\n' "$3" >"$FAKE/checks.$1.err"
  [ -n "${4:-}" ] && echo "$4" >"$FAKE/checks.$1.exit"
  return 0
}

run_mode() {
  : >"$FAKE/output"
  OUT=$(env PATH="$BIN:$PATH" GITHUB_OUTPUT="$FAKE/output" \
    GH_TOKEN=read-token REPO=Zaba505/backup \
    bash -e "$MODE_SCRIPT" 2>&1)
  CODE=$?
}

run_gate() {
  OUT=$(env PATH="$BIN:$PATH" \
    GH_TOKEN=app-token READ_TOKEN=read-token PR=13 REPO=Zaba505/backup \
    HEAD_SHA="$HEAD" SELF='Auto merge' \
    WAIT_MINUTES=15 GRACE_SECONDS=60 POLL_SECONDS=20 \
    bash -e "$GATE_SCRIPT" 2>&1)
  CODE=$?
}

pass=0
fail=0
CASE=''
ok=1

begin() { CASE=$1; ok=1; WHY=''; }
expect() { # <description> <test command...>
  local what=$1; shift
  if ! "$@"; then ok=0; WHY="${WHY}         expected: $what\n"; fi
}
end() {
  if [ "$ok" = 1 ]; then
    pass=$((pass + 1)); printf '  ok   %s\n' "$CASE"
  else
    fail=$((fail + 1)); printf '  FAIL %s\n' "$CASE"
    printf '%b' "$WHY"
    printf '%s\n' "$OUT" | sed 's/^/         out  /'
  fi
}

exit_is() { [ "$CODE" = "$1" ]; }
exit_not() { [ "$CODE" != "$1" ]; }
out_has() { grep -qF -- "$1" <<<"$OUT"; }
output_is() { [ "$(cat "$FAKE/output")" = "$1" ]; }
merged_once() { [ -f "$FAKE/merged" ] && [ "$(wc -l <"$FAKE/merged")" -eq 1 ]; }
not_merged() { [ ! -f "$FAKE/merged" ]; }
merge_args_are() { [ "$(cat "$FAKE/merged")" = "$1" ]; }
sleeps_are() { [ "$(cat "$FAKE/sleeps" 2>/dev/null | wc -l)" -eq "$1" ]; }
merged_with() { grep -q "^$1 pr merge" "$FAKE/calls"; }
reads_with() { ! grep -E ' pr (view|checks)' "$FAKE/calls" | grep -qv "^$1 "; }

printf '\nwhich path — the setting decides, and nothing else does\n'

fresh; echo true >"$FAKE/graphql"; run_mode
begin 'auto merge allowed takes the native path'
expect 'exit 0' exit_is 0
expect 'allowed=true written' output_is 'allowed=true'
end

fresh; echo false >"$FAKE/graphql"; run_mode
begin 'auto merge not allowed (private, GitHub Free) takes the gated path'
expect 'exit 0' exit_is 0
expect 'allowed=false written' output_is 'allowed=false'
expect 'a notice saying the run gates itself' out_has '::notice title=No native auto merge::'
end

fresh; echo null >"$FAKE/graphql"; run_mode
begin 'a null setting fails rather than guessing a path'
expect 'non-zero exit' exit_not 0
expect 'no path written' output_is ''
expect 'an error naming what it read' out_has "read as 'null'"
end

fresh; echo 'gh: HTTP 502' >"$FAKE/graphql"; echo 1 >"$FAKE/graphql.exit"; run_mode
begin 'a failed read fails rather than guessing a path'
expect 'non-zero exit' exit_not 0
expect 'no path written' output_is ''
expect 'the error annotation' out_has '::error title=Cannot read the auto merge setting::'
end

fresh; echo true >"$FAKE/graphql"; run_mode
begin 'the owner and name are passed as GraphQL variables'
expect 'owner=Zaba505 name=backup' grep -q -- '-f owner=Zaba505 -f name=backup' "$FAKE/calls"
end

printf '\nthe gate merges — and only then\n'

fresh; checks 1 "[$(c pass CI 'CI Gate'), $(c pass CI 'Checks / .')]"; run_gate
begin 'every check passed on the first read'
expect 'exit 0' exit_is 0
expect 'merged once' merged_once
expect 'squash, pinned to the labelled head' merge_args_are "pr merge 13 --repo Zaba505/backup --squash --match-head-commit $HEAD"
expect 'no sleep' sleeps_are 0
expect 'the merge uses the App token, so the closed jobs fire' merged_with app-token
expect 'every read uses GITHUB_TOKEN' reads_with read-token
end

fresh; checks 1 "[$(c pending 'Auto merge' 'enable auto merge'), $(c skipping 'Auto merge' 'delete merged branch'), $(c pass CI 'CI Gate')]"; run_gate
begin "this workflow's own pending run is not waited on"
expect 'exit 0' exit_is 0
expect 'merged once' merged_once
expect 'no sleep' sleeps_are 0
end

fresh; checks 1 "[$(c pass CI 'CI Gate'), $(c skipping CI 'Planner tests')]"; run_gate
begin 'a skipped check counts as settled'
expect 'exit 0' exit_is 0
expect 'merged once' merged_once
end

fresh
checks 1 "[$(c pending CI 'CI Gate')]"
checks 2 "[$(c pending CI 'CI Gate')]"
checks 3 "[$(c pass CI 'CI Gate')]"
run_gate
begin 'a running check is waited for, then merged'
expect 'exit 0' exit_is 0
expect 'merged once' merged_once
expect 'two sleeps' sleeps_are 2
end

fresh
checks 1 "[$(c pending '' 'ci/jenkins')]"
checks 2 "[$(c pass '' 'ci/jenkins')]"
run_gate
begin 'a commit status (no workflow) is waited for, not taken for this workflow'
expect 'exit 0' exit_is 0
expect 'merged once' merged_once
expect 'one sleep' sleeps_are 1
end

printf '\nthe gate refuses — and leaves the pull request open\n'

fresh; checks 1 "[$(c fail CI 'CI Gate'), $(c pending CI 'Checks / .')]"; run_gate
begin 'a failed check fails the run, even beside a pending one'
expect 'non-zero exit' exit_not 0
expect 'not merged' not_merged
expect 'the failure named' out_has '::error title=Checks failed::not merging #13: CI / CI Gate'
end

fresh; checks 1 "[$(c cancel CI 'CI Gate')]"; run_gate
begin 'a cancelled check fails the run'
expect 'non-zero exit' exit_not 0
expect 'not merged' not_merged
expect 'the cancelled check named' out_has 'not merging #13: CI / CI Gate'
end

fresh; checks 1 "[$(c fail '' 'ci/jenkins')]"; run_gate
begin 'a failed commit status is named without a workflow'
expect 'non-zero exit' exit_not 0
expect 'named by its context' out_has 'not merging #13: ci/jenkins'
end

fresh; checks 1 "[$(c pending CI 'CI Gate'), $(c pass CI 'Checks / .')]"; run_gate
begin 'a check still running at the deadline fails the run'
expect 'non-zero exit' exit_not 0
expect 'not merged' not_merged
expect 'the pending check named' out_has 'still pending after 15 minutes: CI / CI Gate'
expect 'bounded: 45 polls of 20s' sleeps_are 45
end

fresh; checks 1 "[$(c waiting CI 'CI Gate')]"; run_gate
begin 'a bucket gh does not name is pending, never a pass'
expect 'non-zero exit' exit_not 0
expect 'not merged' not_merged
expect 'named as pending' out_has 'still pending after 15 minutes: CI / CI Gate'
end

fresh; checks 1 '' 'Resource not accessible by integration (repository.pullRequest.statusCheckRollup...checkSuite.workflowRun)' 1; run_gate
begin 'checks that cannot be read fail the run, not "no checks yet"'
expect 'non-zero exit' exit_not 0
expect 'not merged' not_merged
expect 'the gh error quoted' out_has '::error title=Cannot read checks::gh pr checks failed for #13: Resource not accessible by integration'
end

fresh; checks 1 "[$(c pass CI 'CI Gate')]"; echo "$NEW" >"$FAKE/head"; run_gate
begin 'a head that moved since the label fails the run'
expect 'non-zero exit' exit_not 0
expect 'not merged' not_merged
expect 'both commits named' out_has "labelled at $HEAD and its head is now $NEW"
end

fresh; checks 1 "[$(c pass CI 'CI Gate')]"; echo 1 >"$FAKE/merge.exit"; run_gate
begin 'a merge GitHub refuses fails the run'
expect 'non-zero exit' exit_not 0
expect 'the merge was attempted' merged_once
end

printf '\nno checks at all — merged, as native auto merge would, after the grace\n'

fresh; checks 1 '' 'no checks reported on the '"'issue-12'"' branch' 1; run_gate
begin 'no checks reported merges after the grace, with a warning'
expect 'exit 0' exit_is 0
expect 'merged once' merged_once
expect 'the warning' out_has '::warning title=No checks reported::'
expect 'three sleeps: 60s of grace' sleeps_are 3
end

fresh
checks 1 "[$(c pending 'Auto merge' 'enable auto merge')]"
checks 2 "[$(c pending 'Auto merge' 'enable auto merge'), $(c pending CI 'CI Gate')]"
checks 3 "[$(c pending 'Auto merge' 'enable auto merge'), $(c pass CI 'CI Gate')]"
run_gate
begin 'a check that registers during the grace is waited for'
expect 'exit 0' exit_is 0
expect 'merged once' merged_once
expect 'no warning' eval '! out_has "No checks reported"'
end

printf '\nthe workflow around the steps\n'

begin 'only one merge step can run: native on true, gated on false'
OUT="$(yq "$(step 'Enable auto merge')[\"if\"]") | $(yq "$(step 'Merge once checks pass')[\"if\"]")"
expect 'native step gated on true' out_has "steps.auto-merge.outputs.allowed == 'true' |"
expect 'gate step gated on false' out_has "| steps.auto-merge.outputs.allowed == 'false'"
end

begin 'GITHUB_TOKEN can read checks: checks, statuses and actions at read'
OUT=$(yq 'd["permissions"]')
expect 'checks: read' out_has 'checks: read'
expect 'statuses: read' out_has 'statuses: read'
expect 'actions: read (gh pr checks resolves checkSuite.workflowRun)' out_has 'actions: read'
end

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
