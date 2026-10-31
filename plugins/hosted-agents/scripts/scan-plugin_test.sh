#!/usr/bin/env bash
#
# scan-plugin_test.sh — the fixture corpus for scan-plugin.sh.
#
# Offline: every case scans a fixture plugin under testdata/, and delivery cases
# build their repository layouts in a temporary directory, so no fixture commits a
# .claude/ or .github/ directory that a real session could pick up. It pins:
#
#   - the profiles: they validate against assets/target.schema.json, and every
#     rule has a source, a basis and a checked date;
#   - each scanned rule's fixture, testdata/rules/<rule-id>, triggers that rule
#     and no other, in every target whose profile defines the rule;
#   - each delivery rule fires on the repository layout it describes, one per
#     target, and a plugin that cannot be delivered breaks every component;
#   - a clean fixture yields no breaks or needs-setup finding for any target;
#   - a command quoted in skill prose, a JSON block or a message is not a
#     finding, while the same command in a fenced shell block or a script is;
#   - a verdict set by an unconfirmed finding is marked unconfirmed;
#   - verdicts follow calls: a script a skill runs, a spawned agent, a named skill;
#   - review findings merge in as basis review, can only add, and must cite a
#     rule of the profile;
#   - exit codes 0 / 1 / 2, and no network call.
#
# Run: plugins/hosted-agents/scripts/scan-plugin_test.sh
# Exit 0 when every case passes, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/scan-plugin.sh"
DATA="$HERE/testdata"
PROFILES="$HERE/../assets/targets"
SCHEMA="$HERE/../assets/target.schema.json"
TARGETS=(claude-code-cloud copilot-cloud-sandbox copilot-actions)
[ -x "$SUT" ] || { printf 'scan-plugin.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

TMP=$(mktemp -d) || exit 1
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; shift; [ $# -gt 0 ] && printf '       %s\n' "$@"; }
# same <name> <want> <got>
same() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want $2" "got  $3"; fi; }

# scan <out-file> <args...> — run the scanner, keep its JSON, return its exit code
scan() { local out=$1; shift; "$SUT" "$@" >"$out" 2>"$out.err"; }

# ---------------------------------------------------------------------------
printf '\nprofiles\n'

for t in "${TARGETS[@]}"; do
  f="$PROFILES/$t.json"
  if [ -f "$f" ] && jq -e --arg t "$t" '.id == $t' "$f" >/dev/null 2>&1; then ok "$t.json is a profile with id $t"
  else bad "$t.json is a profile with id $t"; fi
done

if python3 -c 'import jsonschema' 2>/dev/null; then
  out=$(python3 - "$SCHEMA" "${TARGETS[@]/#/$PROFILES/}" <<'PY' 2>&1
import json, sys, jsonschema
schema = json.load(open(sys.argv[1]))
jsonschema.Draft202012Validator.check_schema(schema)
v = jsonschema.Draft202012Validator(schema)
bad = 0
for path in sys.argv[2:]:
    for e in v.iter_errors(json.load(open(path + ".json"))):
        bad += 1
        print(f"{path}: {e.message[:200]} at {list(e.absolute_path)}")
sys.exit(1 if bad else 0)
PY
  ) && ok "all three profiles validate against target.schema.json" \
    || bad "all three profiles validate against target.schema.json" "$out"
else
  bad "all three profiles validate against target.schema.json" "python3 with jsonschema is needed to run this case"
fi

missing=$(for t in "${TARGETS[@]}"; do jq -r '.id as $p | .rules[]
  | select((.sources | length) == 0 or (.basis | IN("docs", "observed", "unconfirmed") | not)
           or (.checked | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$") | not)
           or (.basis == "observed" and ((.observed // []) | length) == 0))
  | "\($p)/\(.id)"' "$PROFILES/$t.json"; done)
same "every rule has a source, a basis and a checked date; every observed rule says where" "" "$missing"

obs=$(jq -r '[.rules[] | select(.basis == "observed") | .id] | sort | join(",")' "$PROFILES/claude-code-cloud.json")
same "claude-code-cloud records the GraphQL, gh auth, repository-scope and settings observations" \
  "gh-auth-status-fails,github-graphql,repository-not-attached,repository-settings-ignored" "$obs"
same "the user-settings result links pull request #470" "true" \
  "$(jq '[.rules[] | .observed // [] | .[] | .url] | any(. == "https://github.com/z5labs/devex/pull/470")' "$PROFILES/claude-code-cloud.json")"

# ---------------------------------------------------------------------------
printf '\neach rule'"'"'s fixture triggers that rule and no other\n'

scanned_rules=$(for t in "${TARGETS[@]}"; do jq -r '.rules[] | select(.match.kind != "delivery" and .match.kind != "review") | .id' "$PROFILES/$t.json"; done | sort -u)
for id in $scanned_rules; do
  [ -d "$DATA/rules/$id" ] || { bad "$id has a fixture" "no $DATA/rules/$id"; continue; }
  for t in "${TARGETS[@]}"; do
    jq -e --arg id "$id" 'any(.rules[]; .id == $id)' "$PROFILES/$t.json" >/dev/null || continue
    scan "$TMP/r.json" "$DATA/rules/$id" --no-repo --target "$t"
    same "$id on $t" "$id" "$(jq -r '[.targets[0].findings[].rule] | unique | join(",")' "$TMP/r.json" 2>/dev/null)"
  done
done
for d in "$DATA"/rules/*/; do
  id=$(basename "$d")
  grep -qx -- "$id" <<<"$scanned_rules" || bad "fixture rules/$id names a scanned rule" "no profile scans for $id"
done

# ---------------------------------------------------------------------------
printf '\na clean plugin\n'

scan "$TMP/clean.json" "$DATA/clean" --no-repo; rc=$?
same "clean, no repository: exit 0" 0 "$rc"
same "clean, no repository: no breaks or needs-setup finding on any target" "" \
  "$(jq -r '.targets[] | .id as $t | .findings[] | select(.severity == "breaks" or .severity == "needs-setup") | "\($t)/\(.rule)"' "$TMP/clean.json")"
same "clean, no repository: every component works on every target" "works" \
  "$(jq -r '[.targets[].components[].verdict] | unique | join(",")' "$TMP/clean.json")"
same "clean, no repository: delivery is not assessed" "not assessed" \
  "$(jq -r '[.targets[].delivery.verdict] | unique | join(",")' "$TMP/clean.json")"

# A repository that commits the plugin's components where each target loads them.
vend="$TMP/repo-vendored"
mkdir -p "$vend/.claude/skills/hello" "$vend/.claude/agents" "$vend/.github/agents"
cp "$DATA/clean/skills/hello/SKILL.md" "$vend/.claude/skills/hello/SKILL.md"
cp "$DATA/clean/agents/helper.md" "$vend/.claude/agents/helper.md"
cp "$DATA/clean/agents/helper.md" "$vend/.github/agents/helper.agent.md"
scan "$TMP/vend.json" "$DATA/clean" --repo "$vend"; rc=$?
same "clean, delivered by committed components: exit 0" 0 "$rc"
same "clean, delivered by committed components: no breaks or needs-setup finding on any target" "" \
  "$(jq -r '.targets[] | .id as $t | .findings[] | select(.severity == "breaks" or .severity == "needs-setup") | "\($t)/\(.rule)"' "$TMP/vend.json")"
same "committed components: the delivery rule each target reports" \
  "claude-code-cloud:committed-components copilot-actions:repository-components-unconfirmed copilot-cloud-sandbox:repository-components" \
  "$(jq -r '[.targets[] | .id as $t | .findings[] | select(.component == "delivery") | "\($t):\(.rule)"] | sort | join(" ")' "$TMP/vend.json")"

# ---------------------------------------------------------------------------
printf '\ndelivery\n'

# delivery_case <name> <repo-dir> <expected "target:rule ..." sorted>
delivery_case() {
  scan "$TMP/d.json" "$DATA/clean" --repo "$2"
  same "$1" "$3" "$(jq -r '[.targets[] | .id as $t | .findings[] | select(.component == "delivery") | "\($t):\(.rule)"] | sort | join(" ")' "$TMP/d.json")"
}
settings() { # <file> — enable clean@fixtures there
  mkdir -p "$(dirname "$1")"
  printf '{\n  "extraKnownMarketplaces": { "fixtures": { "source": { "source": "directory", "path": "." } } },\n  "enabledPlugins": {\n    "clean@fixtures": true\n  }\n}\n' >"$1"
}
mkdir -p "$TMP/repo-empty"
delivery_case "a repository that delivers nothing" "$TMP/repo-empty" \
  "claude-code-cloud:plugin-not-delivered copilot-actions:no-plugins-in-actions copilot-cloud-sandbox:no-plugin-settings"
settings "$TMP/repo-claude/.claude/settings.json"
delivery_case "enabled in .claude/settings.json only" "$TMP/repo-claude" \
  "claude-code-cloud:repository-settings-ignored copilot-actions:no-plugins-in-actions copilot-cloud-sandbox:no-plugin-settings"
same "the settings finding points at the line that enables it" ".claude/settings.json:4" \
  "$(jq -r '.targets[] | select(.id == "claude-code-cloud") | .findings[] | select(.component == "delivery") | "\(.file):\(.line)"' "$TMP/d.json")"
settings "$TMP/repo-copilot/.github/copilot/settings.json"
delivery_case "enabled in .github/copilot/settings.json only" "$TMP/repo-copilot" \
  "claude-code-cloud:plugin-not-delivered copilot-actions:no-plugins-in-actions copilot-cloud-sandbox:plugin-settings-unconfirmed"

delivery_rules=$(for t in "${TARGETS[@]}"; do jq -r '.id as $t | .rules[] | select(.match.kind == "delivery") | "\($t):\(.id)"' "$PROFILES/$t.json"; done | sort)
seen=$({ jq -r '.targets[] | .id as $t | .findings[] | select(.component == "delivery") | "\($t):\(.rule)"' "$TMP/vend.json"
  for r in repo-empty repo-claude repo-copilot; do scan "$TMP/s.json" "$DATA/clean" --repo "$TMP/$r"; jq -r '.targets[] | .id as $t | .findings[] | select(.component == "delivery") | "\($t):\(.rule)"' "$TMP/s.json"; done; } | sort -u)
same "every delivery rule is reached by one of these layouts" "$delivery_rules" "$seen"

scan "$TMP/und.json" "$DATA/clean" --repo "$TMP/repo-empty" --target copilot-actions; rc=$?
same "undeliverable: exit 1" 1 "$rc"
same "undeliverable: every component breaks" "breaks" "$(jq -r '[.targets[0].components[].verdict] | unique | join(",")' "$TMP/und.json")"
same "undeliverable: the verdict names the delivery rule" "no-plugins-in-actions" \
  "$(jq -r '[.targets[0].components[].set_by[]] | unique | join(",")' "$TMP/und.json")"
md=$("$SUT" "$DATA/clean" --repo "$TMP/repo-empty" --target copilot-actions --format markdown 2>/dev/null)
first_table=$(grep -n '^| Component | Verdict' <<<"$md" | head -1 | cut -d: -f1)
says=$(grep -n 'cannot be delivered' <<<"$md" | head -1 | cut -d: -f1)
if [ -n "$says" ] && [ -n "$first_table" ] && [ "$says" -lt "$first_table" ]; then ok "undeliverable: the report says so before the component table"
else bad "undeliverable: the report says so before the component table" "line $says vs table at $first_table"; fi

scan "$TMP/sand.json" "$DATA/clean" --repo "$TMP/repo-empty" --target copilot-cloud-sandbox
same "a delivery verdict set by an unconfirmed rule is unconfirmed" "needs-setup true" \
  "$(jq -r '.targets[0].delivery | "\(.verdict) \(.unconfirmed)"' "$TMP/sand.json")"

# ---------------------------------------------------------------------------
printf '\nprose is not a command\n'

skill="$DATA/prose-vs-fence/skills/merge/SKILL.md"
fence_line=$(grep -n '^gh pr merge --squash "\$PR"$' "$skill" | cut -d: -f1)
script_line=$(grep -n '^gh pr merge --squash "\$PR" ||' "$DATA/prose-vs-fence/scripts/merge.sh" | cut -d: -f1)
scan "$TMP/pvf.json" "$DATA/prose-vs-fence" --no-repo --target claude-code-cloud
same "only the fenced shell block and the script's command are findings" \
  "prose-vs-fence/scripts/merge.sh:$script_line prose-vs-fence/skills/merge/SKILL.md:$fence_line" \
  "$(jq -r '[.targets[0].findings[] | select(.rule == "github-graphql") | "\(.file):\(.line)"] | sort | join(" ")' "$TMP/pvf.json")"
same "the fence only degrades; the script breaks" "fence:degrades script:breaks" \
  "$(jq -r '[.targets[0].findings[] | select(.rule == "github-graphql") | "\(.context):\(.severity)"] | sort | join(" ")' "$TMP/pvf.json")"
scan "$TMP/clean-ccc.json" "$DATA/clean" --no-repo --target claude-code-cloud
same "prose naming gh pr merge, git push --tags and a blocked host is not a finding" "" \
  "$(jq -r '[.targets[0].findings[] | select(.severity != "info") | .rule] | join(",")' "$TMP/clean-ccc.json")"

# ---------------------------------------------------------------------------
printf '\nunconfirmed verdicts\n'

scan "$TMP/u.json" "$DATA/rules/plugin-root-undocumented" --no-repo --target copilot-cloud-sandbox
same "a verdict set only by an unconfirmed finding is unconfirmed" "breaks true" \
  "$(jq -r '.targets[0].components[0] | "\(.verdict) \(.unconfirmed)"' "$TMP/u.json")"
same "and so is the finding" "true unconfirmed" \
  "$(jq -r '.targets[0].findings[0] | "\(.unconfirmed) \(.basis)"' "$TMP/u.json")"
scan "$TMP/m.json" "$DATA/mixed-confirmation" --no-repo --target copilot-cloud-sandbox
same "a confirmed finding at the same severity makes the verdict confirmed" "breaks false agent-cannot-git-push,plugin-root-undocumented" \
  "$(jq -r '.targets[0].components[0] | "\(.verdict) \(.unconfirmed) \(.set_by | join(","))"' "$TMP/m.json")"
md=$("$SUT" "$DATA/rules/plugin-root-undocumented" --no-repo --target copilot-cloud-sandbox --format markdown 2>/dev/null)
grep -q '| \*\*breaks\*\*, unconfirmed |' <<<"$md" && ok "the report marks it unconfirmed" || bad "the report marks it unconfirmed"

# ---------------------------------------------------------------------------
printf '\nverdicts follow calls\n'

scan "$TMP/calls.json" "$DATA/calls" --no-repo --target claude-code-cloud
same "edges: a script the skill runs, a script that script runs, a spawned agent, a named skill" \
  "calls:driver>calls:step calls:driver>calls:worker calls:driver>scripts/fetch.sh scripts/fetch.sh>scripts/list.sh" \
  "$(jq -r '[.edges[] | "\(.from)>\(.to)"] | sort | join(" ")' "$TMP/calls.json")"
same "the driver breaks through the script two calls down" "breaks github-graphql scripts/fetch.sh → scripts/list.sh" \
  "$(jq -r '.targets[0].components[] | select(.component == "calls:driver") | "\(.verdict) \(.set_by | join(",")) \(.rules[0].via | join(";"))"' "$TMP/calls.json")"
same "what the driver calls is judged on its own" "calls:step:works calls:worker:works scripts/fetch.sh:breaks scripts/list.sh:breaks" \
  "$(jq -r '[.targets[0].components[] | select(.component != "calls:driver") | "\(.component):\(.verdict)"] | sort | join(" ")' "$TMP/calls.json")"

# ---------------------------------------------------------------------------
printf '\nreview findings\n'

review() { printf '%s\n' "$1" >"$TMP/review.json"; }
review '[{"target": "copilot-cloud-sandbox", "rule": "waits-for-user", "severity": "breaks", "component": "clean:hello",
  "file": "clean/skills/hello/SKILL.md", "line": 8, "evidence": "Greet the user, then wait for their answer"}]'
scan "$TMP/rv.json" "$DATA/clean" --no-repo --target copilot-cloud-sandbox --review "$TMP/review.json"; rc=$?
same "a review finding merges in, and its breaks sets the exit code" 1 "$rc"
same "it is basis review, unconfirmed because its rule is, and takes its rule's sources" "review true true" \
  "$(jq -r '.targets[0].findings[] | select(.rule == "waits-for-user") | "\(.basis) \(.unconfirmed) \(.sources | length > 0)"' "$TMP/rv.json")"
same "it sets the component's verdict" "breaks true" \
  "$(jq -r '.targets[0].components[] | select(.component == "clean:hello") | "\(.verdict) \(.unconfirmed)"' "$TMP/rv.json")"

review '[{"target": "claude-code-cloud", "rule": "github-graphql", "severity": "info", "component": "scripts/list.sh",
  "file": "rules/github-graphql/scripts/list.sh", "line": 2, "evidence": "only a listing"}]'
scan "$TMP/rv2.json" "$DATA/rules/github-graphql" --no-repo --target claude-code-cloud --review "$TMP/review.json"
same "a review finding cannot downgrade or drop a scanned one" "breaks 2" \
  "$(jq -r '.targets[0] | "\(.components[0].verdict) \(.findings | length)"' "$TMP/rv2.json")"

review_rejected() { # <name> <json>
  review "$2"
  scan "$TMP/rj.json" "$DATA/clean" --no-repo --target copilot-cloud-sandbox --review "$TMP/review.json"
  same "$1: exit 2" 2 "$?"
}
review_rejected "a review finding citing a rule the profile lacks" \
  '[{"target": "copilot-cloud-sandbox", "rule": "made-up", "severity": "breaks", "component": "clean:hello", "file": "x", "line": 1, "evidence": "e"}]'
review_rejected "a review finding claiming basis docs" \
  '[{"target": "copilot-cloud-sandbox", "rule": "waits-for-user", "severity": "breaks", "basis": "docs", "component": "clean:hello", "file": "x", "line": 1, "evidence": "e"}]'
review_rejected "a review finding on a component the plugin lacks" \
  '[{"target": "copilot-cloud-sandbox", "rule": "waits-for-user", "severity": "breaks", "component": "clean:nope", "file": "x", "line": 1, "evidence": "e"}]'
review_rejected "a review finding for a target not scanned" \
  '[{"target": "copilot-actions", "rule": "waits-for-user", "severity": "breaks", "component": "clean:hello", "file": "x", "line": 1, "evidence": "e"}]'
review_rejected "a review file that is not an array" '{"rule": "waits-for-user"}'

# ---------------------------------------------------------------------------
printf '\noutput\n'

keys=$(jq -r '[.targets[].findings[] | keys] | add | unique | join(",")' "$TMP/pvf.json")
for k in target rule severity component file line evidence fix_in remedy sources basis; do
  [[ ",$keys," == *",$k,"* ]] || { bad "every finding has target, rule, severity, component, file, line, evidence, fix_in, remedy, sources and basis" "missing $k"; k=missing; break; }
done
[ "${k:-}" = missing ] || ok "every finding has target, rule, severity, component, file, line, evidence, fix_in, remedy, sources and basis"
same "one JSON document, every target, a verdict per component" "3 true" \
  "$(jq -r '"\(.targets | length) \([.targets[].components[] | has("verdict")] | all)"' "$TMP/clean.json")"
md=$("$SUT" "$DATA/calls" --no-repo --format markdown 2>/dev/null)
for t in "${TARGETS[@]}"; do
  grep -q "^## .* — \`$t\`$" <<<"$md" && grep -q "targets/$t.json\`, checked \*\*[0-9-]*\*\*" <<<"$md" \
    && ok "the report has a $t section with its profile's checked date" \
    || bad "the report has a $t section with its profile's checked date"
done

# ---------------------------------------------------------------------------
printf '\nexit codes\n'

exit_case() { # <name> <want> <args...>
  local name=$1 want=$2; shift 2
  "$SUT" "$@" >/dev/null 2>&1
  same "$name" "$want" "$?"
}
exit_case "no finding breaks: 0" 0 "$DATA/clean" --no-repo
exit_case "a finding breaks: 1" 1 "$DATA/rules/github-graphql" --no-repo --target claude-code-cloud
exit_case "only needs-setup: 0" 0 "$DATA/rules/credential-from-environment" --no-repo --target claude-code-cloud
exit_case "a directory without .claude-plugin/plugin.json: 2" 2 "$DATA/not-a-plugin" --no-repo
exit_case "a path that does not exist: 2" 2 "$DATA/no-such-plugin" --no-repo
exit_case "no plugin argument: 2" 2
exit_case "an unknown option: 2" 2 "$DATA/clean" --bogus
exit_case "a target with no profile: 2" 2 "$DATA/clean" --no-repo --target no-such-target

mkdir -p "$TMP/profiles-bad-json" "$TMP/profiles-bad-pattern" "$TMP/profiles-no-sources" "$TMP/profiles-wrong-id"
printf '{ "id": "broken", \n' >"$TMP/profiles-bad-json/broken.json"
jq '.rules[0] = (.rules[] | select(.match.kind == "command") | .match.pattern = "(unclosed")' \
  "$PROFILES/claude-code-cloud.json" >"$TMP/profiles-bad-pattern/claude-code-cloud.json"
jq '.rules[0].sources = []' "$PROFILES/claude-code-cloud.json" >"$TMP/profiles-no-sources/claude-code-cloud.json"
jq '.id = "something-else"' "$PROFILES/claude-code-cloud.json" >"$TMP/profiles-wrong-id/claude-code-cloud.json"
exit_case "a profile that is not JSON: 2" 2 "$DATA/clean" --no-repo --targets-dir "$TMP/profiles-bad-json"
exit_case "a profile whose pattern does not compile: 2" 2 "$DATA/clean" --no-repo --targets-dir "$TMP/profiles-bad-pattern"
exit_case "a profile rule with no source: 2" 2 "$DATA/clean" --no-repo --targets-dir "$TMP/profiles-no-sources"
exit_case "a profile whose id is not its file name: 2" 2 "$DATA/clean" --no-repo --targets-dir "$TMP/profiles-wrong-id"

# ---------------------------------------------------------------------------
printf '\nno network\n'

# Every command that could reach the network is shadowed by a stub that records
# the call. The scan reads a plugin with scripts, fences, edges and a repository.
mkdir -p "$TMP/stubs"
for c in curl wget gh git nc ssh http https python3 node; do
  printf '#!/bin/sh\nprintf "%%s %%s\\n" "%s" "$*" >>"%s/calls.log"\nexit 1\n' "$c" "$TMP" >"$TMP/stubs/$c"
  chmod +x "$TMP/stubs/$c"
done
: >"$TMP/calls.log"
PATH="$TMP/stubs:$PATH" HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 \
  "$SUT" "$DATA/calls" --repo "$TMP/repo-claude" >/dev/null 2>&1
same "scanning calls no network tool" "" "$(cat "$TMP/calls.log")"
PATH="$TMP/stubs:$PATH" "$SUT" "$DATA/calls" --format markdown >/dev/null 2>&1
same "finding the repository with git is local: rev-parse only" "" \
  "$(grep -v '^git -C .* rev-parse --show-toplevel$' "$TMP/calls.log")"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
