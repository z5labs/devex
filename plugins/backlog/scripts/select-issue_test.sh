#!/usr/bin/env bash
#
# select-issue_test.sh — the fixture corpus for select-issue.sh.
#
# Three halves, all offline.
#
# **Extraction.** Every case is an issue body that a real backlog can contain.
# The ones marked REGRESSION were extracted wrongly by the awk/sed/grep
# pipelines this script replaced, back when they lived in
# `skills/next-issue/SKILL.md` as prose for an agent to retype and check against
# a table by eye. Three of them produced a *silently eligible* issue, which is
# the failure that gets work done in the wrong order rather than not at all.
#
# **Native dependencies.** Every case is a GraphQL response the `blockedBy`
# query can return, fed to `--native-deps`. The style takes its edges from
# GitHub rather than from the body, so its corpus is responses rather than
# markdown — but it prints the same two reference shapes, which is what keeps
# the eligibility walk one code path across every style.
#
# **Project scoping.** Every case is a GraphQL response `gh api graphql
# --paginate` can return for the project query, fed to `--project-items`. The
# failures matter as much as the matches here: a scope that resolves to nothing
# and a scope that was never applied both look like an ordinary backlog from the
# outside, so each one has to be an exit 4 rather than an empty answer.
#
# **Runtime selectors.** Every case runs the *whole* script against a scratch
# repository and a `gh` that answers from files, asserting which issue came back
# rather than which message did. That is what covers the rules no seam can reach:
# flag over config for the label, the milestone and the project scope; the
# combinations that are refused rather than silently resolved; and an unknown
# milestone, which `gh issue list` answers with `[]` and exit 0 — the failure that
# reported a workable backlog as a drained one.
#
# Run: plugins/backlog/scripts/select-issue_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/select-issue.sh"
[ -x "$SUT" ] || { printf 'select-issue.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

# The review roster every harness below writes unless a case replaces it. It is
# a variable rather than a literal because the roster is now validated at
# selection, so an invalid one has to be expressible in a fixture.
REVIEW_BLOCK='{ "reviewers": ["copilot"] }'

# check <name> <style> <expected refs, space separated> <body>
check() {
  local name=$1 style=$2 want=$3 body=$4 got
  got=$(printf '%s' "$body" | "$SUT" --extract "$style" | tr '\n' ' ')
  got=${got% }
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [%s]\n' "$name" "${got:-<none>}"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want [%s] got [%s]\n' "$name" "$want" "$got"
  fi
}

printf '\nblocked-by\n'

# The fixture the skill has always documented. The three styles must give three
# different answers on this one body.
check 'the documented fixture' blocked-by '#12 #14' \
'### Related Issues

Blocked by:
- #12 — the parser this builds on
- #14

Depends on #17.

- #19 is related but not blocking.'

# REGRESSION: `tolower($0) ~ /blocked by/` was unanchored, so a sentence saying
# the issue is NOT blocked opened a dependency list and swallowed the next
# bullet. The issue was then blocked forever on a dependency not in its body.
check 'REGRESSION negated prose does not open a list' blocked-by '' \
'This issue is not blocked by anything.

- #77 see also'

# REGRESSION: same root cause, past tense, in a paragraph nowhere near the
# Related Issues section.
check 'REGRESSION incidental prose does not open a list' blocked-by '' \
'Background

The old parser was blocked by a flaky test.

- #99 unrelated tracking bullet'

# REGRESSION: an issue template quoted inside a fence is an example, not a
# declaration.
check 'REGRESSION fenced code block is not a declaration' blocked-by '' \
'Example config:

```
Blocked by:
- #123
```

real text'

# REGRESSION: the inline form extracted nothing at all, so the issue read as
# unblocked. Every reference on the label line counts — there is no gloss
# convention there, and taking only the first would drop #14.
check 'REGRESSION inline form on the label line' blocked-by '#12 #14' \
'Blocked by: #12, #14

Some prose.'

# REGRESSION: GitHub renders `- [ ] #12` as a task list. The old list-item
# regex required `#` immediately after the bullet, so a checklist of blockers
# extracted nothing.
check 'REGRESSION task-list checkboxes' blocked-by '#12 #14' \
'Blocked by:
- [ ] #12
- [x] #14'

# REGRESSION, and the worst of them. `- z5labs/other#42` was not a list item
# under the old regex, so it TERMINATED the list — the cross-repo reference was
# dropped AND the real in-repo dependency below it was never seen. Now both
# survive, and the caller reports BLOCKED on the unresolvable one.
check 'REGRESSION cross-repo item keeps the list open' blocked-by 'z5labs/other#42 #14' \
'Blocked by:
- z5labs/other#42
- #14'

check 'a markdown heading ends the list' blocked-by '#12' \
'Blocked by:
- #12

## Acceptance Criteria

- [ ] parser handles #500 style refs
- #501'

check 'prose ends the list' blocked-by '#12' \
'Blocked by:
- #12
Some following paragraph.
- #34'

check 'bold label' blocked-by '#12' \
'**Blocked by:**
- #12'

check 'heading label with no colon' blocked-by '#12' \
'### Blocked by

- #12'

# The first reference on an item is the dependency; the rest of the line is a
# gloss. This is the rule the documented fixture depends on.
check 'first ref per item, gloss ignored' blocked-by '#12' \
'Blocked by:
- #12 — needs #34 first'

# Markdown calls this one loose list, so both items are dependencies. Ending at
# the blank line instead would silently drop #19, and a missed dependency is
# worse than an extra one: the extra one halts the loop with a visible reason.
check 'a blank line does not end a loose list' blocked-by '#12 #19' \
'Blocked by:
- #12

- #19'

check 'duplicates collapse, order preserved' blocked-by '#14 #12' \
'Blocked by:
- #14
- #12
- #14'

check 'no label at all' blocked-by '' \
'Just an ordinary issue body with a reference to #5 in it.'

printf '\ndepends-on\n'

check 'the documented fixture' depends-on '#17' \
'### Related Issues

Blocked by:
- #12 — the parser this builds on
- #14

Depends on #17.

- #19 is related but not blocking.'

# REGRESSION: `grep -oiE '"'"'depends on[[:space:]]*#[0-9]+'"'"'` matched a
# cross-repo reference nowhere, so the issue came out eligible with an
# unresolved dependency.
check 'REGRESSION cross-repo reference is reported' depends-on 'z5labs/other#42' \
'Depends on z5labs/other#42'

# REGRESSION: a note that a dependency was removed is not a dependency.
check 'REGRESSION no longer depends on' depends-on '' \
'This no longer depends on #17 since #20 landed.'

check 'REGRESSION never depends on' depends-on '' \
'This never depends on #17.'

check 'REGRESSION fenced code block is not a declaration' depends-on '' \
'For example:

```
Depends on #123
```'

check 'colon form' depends-on '#17' 'Depends on: #17'

check 'case insensitive' depends-on '#17' 'depends on #17'

check 'several, deduped in order' depends-on '#17 #20' \
'Depends on #17 for the schema.

Also depends on #20, and depends on #17 again.'

# The reference has to follow the phrase. A sentence that merely mentions an
# issue later on is not declaring a dependency on it.
check 'non-adjacent reference is not a dependency' depends-on '' \
'Depends on the parser landing, see #99 for context.'

check 'blocked-by list items carry no depends-on phrase' depends-on '' \
'Blocked by:
- #12
- #14'

printf '\nnone\n'

check 'parses nothing' none '' \
'Blocked by:
- #12

Depends on #17.'

printf '\nnative dependencies\n'

# The `native` style takes its edges from GitHub's typed issue dependencies
# instead of the body, so its corpus is GraphQL responses rather than markdown —
# fixtured the same way, and read through the same `--native-deps` seam the walk
# uses, so nothing here needs the network.
#
# The failures carry the weight. Every one of them would otherwise resolve to an
# empty reference list, and an empty reference list is indistinguishable from an
# issue with no blockers — which is precisely the wrong answer this style exists
# to remove, arrived at from the other direction.

npage() { # <blockedBy nodes json array> [issue number]
  printf '{"data":{"repository":{"issue":{"number":%s,' "${2:-42}"
  printf '"blockedBy":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":%s}}}}}' "$1"
}

dep() { # <number> <repo>
  printf '{"number":%s,"repository":{"nameWithOwner":"%s"}}' "$1" "$2"
}

# checkn <name> <repo> <expected refs, space separated> <pages>
checkn() {
  local name=$1 repo=$2 want=$3 pages=$4 got rc
  got=$(printf '%s' "$pages" | "$SUT" --native-deps "$repo" 2>/dev/null)
  rc=$?
  got=$(printf '%s' "$got" | tr '\n' ' ')
  got=${got% }
  if [ "$rc" -eq 0 ] && [ "$got" = "$want" ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [%s]\n' "$name" "${got:-<none>}"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want [%s] exit 0, got [%s] exit %d\n' "$name" "$want" "$got" "$rc"
  fi
}

# checkn_fail <name> <repo> <substring of the message> <pages>
checkn_fail() {
  local name=$1 repo=$2 want=$3 pages=$4 err rc
  err=$(printf '%s' "$pages" | "$SUT" --native-deps "$repo" 2>&1 >/dev/null)
  rc=$?
  case "$err" in
    *"$want"*) ;;
    *) fail=$((fail + 1))
       printf '  FAIL %-58s message lacks [%s]: %s\n' "$name" "$want" "$err"
       return ;;
  esac
  if [ "$rc" -eq 4 ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [exit 4]\n' "$name"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want exit 4, got exit %d\n' "$name" "$rc"
  fi
}

# The same two references the documented body fixture yields under `blocked-by`,
# in the same printed form — that sameness is what lets the eligibility walk stay
# one code path across every style.
checkn 'the documented fixture, as typed edges' z5labs/devex '#12 #14' \
  "$(npage "[$(dep 12 z5labs/devex),$(dep 14 z5labs/devex)]")"

# The honest empty, and the reason the style has to be declared rather than
# fallen back to: on a repository that has not populated dependencies this is
# every issue's answer, and it reads as an unblocked backlog.
checkn 'no dependencies is an empty edge set, not an error' z5labs/devex '' \
  "$(npage '[]')"

# A cross-repository edge is reported in the form it was written, so the caller
# can tell "issue 14 here" from "something I cannot resolve" — the same
# distinction the body extractor draws, and the same one that makes the issue
# ineligible rather than silently eligible.
checkn 'a cross-repository edge is named, not dropped' z5labs/devex 'z5labs/other#42 #14' \
  "$(npage "[$(dep 42 z5labs/other),$(dep 14 z5labs/devex)]")"

# An issue number is only unique within its repository. Keyed on the number
# alone, `z5labs/other#12` would collapse into `#12` and be resolved against
# this repository's issue 12 — a dependency on the wrong issue entirely.
checkn 'the same number in two repositories stays two edges' z5labs/devex '#12 z5labs/other#12' \
  "$(npage "[$(dep 12 z5labs/devex),$(dep 12 z5labs/other)]")"

# `--paginate` prints one whole response per page, concatenated. Reading only the
# first would drop every dependency past 100 — an issue that looks less blocked
# than it is.
checkn 'every page contributes, deduped, order preserved' z5labs/devex '#12 #14 #19' \
  "$(npage "[$(dep 12 z5labs/devex),$(dep 14 z5labs/devex)]")
$(npage "[$(dep 14 z5labs/devex),$(dep 19 z5labs/devex)]")"

# The four failures, each of which would otherwise print nothing and exit 0.
checkn_fail 'an empty response is refused' z5labs/devex \
  'returned no response' ''

checkn_fail 'a missing issue is refused' z5labs/devex \
  'resolved to no issue' \
  '{"data":{"repository":{"issue":null}}}'

# An issue that exists but carries no `blockedBy` connection is a GitHub that
# does not serve typed dependencies at all. Reading it as "no blockers" is the
# whole failure mode.
checkn_fail 'an issue with no blockedBy connection is refused' z5labs/devex \
  'does not serve typed issue dependencies' \
  '{"data":{"repository":{"issue":{"number":42}}}}'

checkn_fail 'a node that cannot be named is refused, not filtered' z5labs/devex \
  'no number or repository' \
  "$(npage '[{"number":12,"repository":null}]')"

checkn_fail 'a response that is not JSON is refused' z5labs/devex \
  'does not parse as JSON' 'not json at all'

# `native` is a configured style but never a body extraction. Accepting it here
# would answer "no references" for a body that declares none, which is exactly
# the fallback the style is declared to avoid.
printf '\nnative is not a body style\n'
NATIVE_EXTRACT_ERR=$(printf 'Blocked by:\n- #12\n' | "$SUT" --extract native 2>&1 >/dev/null)
NATIVE_EXTRACT_RC=$?
case "$NATIVE_EXTRACT_ERR" in
  *'--native-deps'*)
    if [ "$NATIVE_EXTRACT_RC" -eq 4 ]; then
      pass=$((pass + 1)); printf '  ok   %-58s [exit 4]\n' '--extract native points at --native-deps'
    else
      fail=$((fail + 1)); printf '  FAIL %-58s want exit 4, got exit %d\n' '--extract native points at --native-deps' "$NATIVE_EXTRACT_RC"
    fi ;;
  *) fail=$((fail + 1))
     printf '  FAIL %-58s message lacks [--native-deps]: %s\n' '--extract native points at --native-deps' "$NATIVE_EXTRACT_ERR" ;;
esac

printf '\nproject scoping\n'

# One GraphQL page, built around the item list under test. The field list and
# the query root are the two things individual cases vary, so both are
# arguments. The shape is what `gh api graphql` really returns — the fields are
# read out of `fields` because `field(name:)` fails the whole query on a name
# that does not exist, taking the list of real names with it.
FIELDS='[{"__typename":"ProjectV2Field","name":"Title"},
         {"__typename":"ProjectV2SingleSelectField","name":"Status","options":[{"name":"Todo"},{"name":"Done"}]},
         {"__typename":"ProjectV2SingleSelectField","name":"Module","options":[{"name":"workspace-ci"},{"name":"pdf"}]}]'

page() { # <items json array> [fields json array] [organization|user]
  printf '{"data":{"%s":{"projectV2":{"title":"devex",' "${3:-organization}"
  printf '"fields":{"nodes":%s},' "${2:-$FIELDS}"
  printf '"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":%s}}}}}' "$1"
}

issue_item() { # <number> <repo> <field value, or - for none>
  local value='null'
  [ "$3" = - ] || value="{\"__typename\":\"ProjectV2ItemFieldSingleSelectValue\",\"name\":\"$3\"}"
  printf '{"content":{"__typename":"Issue","number":%s,"repository":{"nameWithOwner":"%s"}},"fieldValueByName":%s}' \
    "$1" "$2" "$value"
}

# checkp <name> <repo> <field> <value> <expected numbers, space separated> <pages>
checkp() {
  local name=$1 repo=$2 field=$3 value=$4 want=$5 pages=$6 got rc
  got=$(printf '%s' "$pages" | "$SUT" --project-items "$repo" "$field" "$value" 2>/dev/null)
  rc=$?
  got=$(printf '%s' "$got" | tr '\n' ' ')
  got=${got% }
  if [ "$rc" -eq 0 ] && [ "$got" = "$want" ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [%s]\n' "$name" "${got:-<none>}"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want [%s] exit 0, got [%s] exit %d\n' "$name" "$want" "$got" "$rc"
  fi
}

# checkp_fail <name> <repo> <field> <value> <substring of the message> <pages>
checkp_fail() {
  local name=$1 repo=$2 field=$3 value=$4 want=$5 pages=$6 err rc
  err=$(printf '%s' "$pages" | "$SUT" --project-items "$repo" "$field" "$value" 2>&1 >/dev/null)
  rc=$?
  case "$err" in
    *"$want"*) ;;
    *) fail=$((fail + 1))
       printf '  FAIL %-58s message lacks [%s]: %s\n' "$name" "$want" "$err"
       return ;;
  esac
  if [ "$rc" -eq 4 ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [exit 4]\n' "$name"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want exit 4, got exit %d\n' "$name" "$rc"
  fi
}

checkp 'the requested value, and only it' z5labs/devex Module workspace-ci '288 291' \
  "$(page "[$(issue_item 288 z5labs/devex workspace-ci),
            $(issue_item 302 z5labs/devex pdf),
            $(issue_item 291 z5labs/devex workspace-ci)]")"

# An org-level project spans repositories. Trusting its items as-is would select
# issue 288 in *this* repository because a sibling repository's 288 is in scope.
checkp 'another repository is not this one' z5labs/devex Module workspace-ci '291' \
  "$(page "[$(issue_item 288 z5labs/other workspace-ci),
            $(issue_item 291 z5labs/devex workspace-ci)]")"

# A project holds draft issues and pull requests too. Neither has an issue
# number in this repository, and a draft's `content` carries no `number` at all.
checkp 'draft issues and pull requests are skipped' z5labs/devex Module workspace-ci '291' \
  "$(page '[{"content":{"__typename":"DraftIssue"},"fieldValueByName":{"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"workspace-ci"}},
            {"content":{"__typename":"PullRequest","number":304,"repository":{"nameWithOwner":"z5labs/devex"}},"fieldValueByName":{"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"workspace-ci"}},
            '"$(issue_item 291 z5labs/devex workspace-ci)"']')"

# An item that was never given a value for the field is out of scope, not in it.
checkp 'an unset field value is out of scope' z5labs/devex Module workspace-ci '291' \
  "$(page "[$(issue_item 288 z5labs/devex -),
            $(issue_item 291 z5labs/devex workspace-ci)]")"

# Defensive: only a single-select value is read. A value of any other type on a
# field that has since been retyped must not match by carrying the right `name`.
checkp 'only a single-select value counts' z5labs/devex Module workspace-ci '291' \
  "$(page '[{"content":{"__typename":"Issue","number":288,"repository":{"nameWithOwner":"z5labs/devex"}},"fieldValueByName":{"__typename":"ProjectV2ItemFieldTextValue","name":"workspace-ci"}},
            '"$(issue_item 291 z5labs/devex workspace-ci)"']')"

# `--paginate` prints one whole response per page, concatenated. Reading only
# the first would silently drop every item past 100 — a scoped backlog that
# looks short rather than one that errors.
checkp 'every page contributes, deduped' z5labs/devex Module workspace-ci '288 291 306' \
  "$(page "[$(issue_item 288 z5labs/devex workspace-ci),
            $(issue_item 291 z5labs/devex workspace-ci)]")
$(page "[$(issue_item 291 z5labs/devex workspace-ci),
            $(issue_item 306 z5labs/devex workspace-ci)]")"

# A project can be owned by a user rather than an organisation, and the query
# root differs. Both responses have to read the same.
checkp 'a user-owned project reads the same' z5labs/devex Module workspace-ci '291' \
  "$(page "[$(issue_item 291 z5labs/devex workspace-ci)]" "$FIELDS" user)"

checkp 'no items at all is an empty scope, not an error' z5labs/devex Module workspace-ci '' \
  "$(page '[]')"

# The four failures. Each one would otherwise resolve to an empty candidate set,
# which reads as a drained backlog and stops the loop looking like success.
checkp_fail 'a typo in the value names the options' z5labs/devex Module Workspace-CI \
  'workspace-ci,pdf' \
  "$(page "[$(issue_item 291 z5labs/devex workspace-ci)]")"

checkp_fail 'an unknown field names the fields' z5labs/devex Modul workspace-ci \
  'Title, Status, Module' \
  "$(page "[$(issue_item 291 z5labs/devex workspace-ci)]")"

checkp_fail 'a field of the wrong type is refused' z5labs/devex Module workspace-ci \
  'not a single-select field' \
  "$(page "[$(issue_item 291 z5labs/devex workspace-ci)]" \
          '[{"__typename":"ProjectV2Field","name":"Module"}]')"

checkp_fail 'an unresolvable project is refused' z5labs/devex Module workspace-ci \
  'no project was returned' \
  '{"data":{"organization":{"projectV2":null}}}'

checkp_fail 'an empty response is refused' z5labs/devex Module workspace-ci \
  'returned no response' ''

printf '\nscope assembly\n'

# Where the scope came from, as opposed to what a project response means. Every
# case here runs the *whole* script — flags, config, the merge of the two,
# selection — against a scratch repository and a `gh` that answers from files, so
# the rules that decide which scope a run is under are exercised with no network
# at all. That matters most for the case the seam above cannot reach: a complete
# scope assembled from flags alone, against a config carrying no `select.project`.
#
# `dependencies.style` is `none` in every case, so the walk takes the first
# in-scope candidate and never reads a body. Three answers are therefore
# distinguishable — unscoped picks 288, `Module = workspace-ci` picks 291,
# `Status = Todo` picks 302 — and each case asserts which of the three it got.
# An assertion that could not tell a dropped scope from an applied one would pass
# for the bug these flags are most able to introduce.
SCRATCH=$(mktemp -d) || { printf 'cannot create a scratch directory\n' >&2; exit 1; }
trap 'rm -rf "$SCRATCH"' EXIT

git init -q "$SCRATCH/repo" >/dev/null 2>&1 \
  || { printf 'cannot init a scratch repository\n' >&2; exit 1; }
mkdir -p "$SCRATCH/repo/.claude" "$SCRATCH/bin" "$SCRATCH/pages"

# The three gh calls this path makes, answered from files. The project response
# is chosen by the `field=` the query was given, which is what lets a case prove
# the flag reached the *query* rather than only the message. Anything else is a
# loud failure: `gh issue view` would mean the dependency walk read a body under
# style `none`.
cat >"$SCRATCH/bin/gh" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  'repo view')  printf 'z5labs/devex\n' ;;
  'issue list') cat "$STUB_ISSUES" ;;
  'api graphql')
    f=""
    for a in "$@"; do case "$a" in field=*) f=${a#field=} ;; esac; done
    if [ -n "$f" ] && [ -f "$STUB_PAGES/$f" ]; then cat "$STUB_PAGES/$f"; else cat "$STUB_PAGES/default"; fi ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$SCRATCH/bin/gh"

printf '288\tthe first\n291\tthe second\n302\tthe third\n' >"$SCRATCH/issues"

page "[$(issue_item 288 z5labs/devex pdf),
       $(issue_item 291 z5labs/devex workspace-ci),
       $(issue_item 302 z5labs/devex workspace-ci)]" >"$SCRATCH/pages/Module"
page "[$(issue_item 288 z5labs/devex Done),
       $(issue_item 291 z5labs/devex Done),
       $(issue_item 302 z5labs/devex Todo)]" >"$SCRATCH/pages/Status"
# A field the project does not have still gets the project's field list back,
# which is the whole reason `project_query` reads `fields` rather than
# `field(name:)`.
page '[]' >"$SCRATCH/pages/default"

# run_sut <select.project json, or - for none> [args...]
run_sut() {
  local project=$1
  shift
  local block=""
  [ "$project" = - ] || block=",
    \"project\": $project"
  cat >"$SCRATCH/repo/.claude/backlog.json" <<JSON
{
  "select": { "label": "story", "milestone": null, "limit": 200$block },
  "dependencies": { "style": "none" },
  "verify": ["true"],
  "merge": { "label": "auto-merge", "workflow": "auto-merge.yaml" },
  "review": $REVIEW_BLOCK,
  "worktreeDir": ".claude/worktrees"
}
JSON
  RUN_OUT=$(cd "$SCRATCH/repo" && PATH="$SCRATCH/bin:$PATH" \
    STUB_ISSUES="$SCRATCH/issues" STUB_PAGES="$SCRATCH/pages" \
    "$SUT" "$@" 2>"$SCRATCH/err")
  RUN_RC=$?
  RUN_ERR=$(tr '\n' ' ' <"$SCRATCH/err")
}

# checks <name> <expected issue number> <select.project json|-> [args...]
checks() {
  local name=$1 want=$2
  shift 2
  run_sut "$@"
  local got
  got=$(printf '%s' "$RUN_OUT" | jq -r '.number // empty' 2>/dev/null)
  if [ "$RUN_RC" -eq 0 ] && [ "$got" = "$want" ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [#%s]\n' "$name" "$got"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want [#%s] exit 0, got [%s] exit %d: %s\n' \
      "$name" "$want" "${got:-<none>}" "$RUN_RC" "$RUN_ERR"
  fi
}

# checks_fail <name> <substring of the message> <select.project json|-> [args...]
checks_fail() {
  local name=$1 want=$2
  shift 2
  run_sut "$@"
  case "$RUN_ERR" in
    *"$want"*) ;;
    *) fail=$((fail + 1))
       printf '  FAIL %-58s message lacks [%s]: %s\n' "$name" "$want" "$RUN_ERR"
       return ;;
  esac
  if [ "$RUN_RC" -eq 4 ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [exit 4]\n' "$name"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want exit 4, got exit %d\n' "$name" "$RUN_RC"
  fi
}

BOARD='{"owner":"z5labs","number":14,"field":"Status","value":null}'
PINNED='{"owner":"z5labs","number":14,"field":"Status","value":"Todo"}'

# The case the config-only design could not express at all: no select.project in
# the file, a complete scope on the command line.
checks 'a scope assembled from flags alone' 291 - \
  --project-owner z5labs --project-number 14 --project-field Module --project-value workspace-ci

checks 'the =value form assembles the same scope' 291 - \
  --project-owner=z5labs --project-number=14 --project-field=Module --project-value=workspace-ci

# 291 rather than 302 is the assertion: the flag reached the query, and the
# configured Status axis did not.
checks '--project-field overrides the configured field' 291 "$BOARD" \
  --project-field Module --project-value workspace-ci

checks '--project-value alone keeps the configured field' 302 "$BOARD" \
  --project-value Todo

checks 'a fully configured scope still needs no flags' 302 "$PINNED"

checks '--no-project-filter drops the configured scope' 288 "$PINNED" \
  --no-project-filter

# The existing `fields` lookup, reached through the flag rather than the config.
# No second code path, and no second list of field names to disagree with it.
checks_fail 'an unknown --project-field names the fields' 'Title, Status, Module' "$BOARD" \
  --project-field Modul --project-value workspace-ci

# Each of these would otherwise be a run silently widened to another dimension,
# or to the whole backlog. All three say which piece is missing.
checks_fail 'a scope with nothing behind it names all three' 'owner (select.project.owner or --project-owner)' - \
  --project-value workspace-ci

checks_fail 'a config missing only the field says so' 'field (select.project.field or --project-field)' \
  '{"owner":"z5labs","number":14}' --project-value workspace-ci

checks_fail 'an axis with no value is not an unscoped run' 'no value to scope by' "$BOARD" \
  --project-field Module

checks_fail 'a scope flag and --no-project-filter is refused' 'cannot be combined with' "$BOARD" \
  --project-field Module --no-project-filter

# The message names the flag, not the config key, when the flag is where the bad
# number came from.
checks_fail 'a bad --project-number blames the flag' '--project-number must be a positive integer' - \
  --project-owner z5labs --project-number fourteen --project-field Module --project-value workspace-ci

printf '\nruntime selectors\n'

# Every selector is a default in `.claude/backlog.json` that one run can
# override, and this section is where that is asserted for the label and the
# milestone — the two the config used to own outright.
#
# The milestone is the one with a measured failure behind it. avroc pinned
# `select.milestone: "v0.2.0"`, the milestone was later deleted, and `gh issue
# list --milestone v0.2.0` answered `[]` with exit 0 over an open, eligible,
# unmilestoned story. That printed BACKLOG EMPTY and halted the loop on a
# workable backlog, with nothing in the run pointing at the milestone. So an
# unknown milestone is exit 4 naming the ones that exist, from the config as
# readily as from the flag.
#
# The same shape as the scope-assembly section above: the whole script, a
# scratch repository, a `gh` that answers from files, and an assertion on *which
# issue* came back rather than on a message. Each fixture list holds a different
# issue number, so a flag that never reached the query is a different answer and
# not merely a different diagnostic.
mkdir -p "$SCRATCH/sel/repo/.claude" "$SCRATCH/sel/bin" "$SCRATCH/sel/lists" \
         "$SCRATCH/sel/views" "$SCRATCH/sel/pages"

git init -q "$SCRATCH/sel/repo" >/dev/null 2>&1 \
  || { printf 'cannot init a scratch repository\n' >&2; exit 1; }

# `issue list` is answered by the label and milestone it was *given*, which is
# what lets a case prove a flag reached the query. `api repos/.../milestones` and
# `issue view` really run the `--jq` they were passed, so the fixtures are the
# shapes GitHub returns rather than the answers the script wants.
cat >"$SCRATCH/sel/bin/gh" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  'repo view') printf 'z5labs/devex\n' ;;
  'issue list')
    label="" milestone="" prev=""
    for a in "$@"; do
      case "$prev" in --label) label=$a ;; --milestone) milestone=$a ;; esac
      prev=$a
    done
    f="$STUB_LISTS/$label#$milestone"
    [ -f "$f" ] || { printf 'stub gh: no issue list for label=%s milestone=%s\n' "$label" "$milestone" >&2; exit 1; }
    cat "$f" ;;
  'issue view')
    q="" prev=""
    for a in "$@"; do [ "$prev" = --jq ] && q=$a; prev=$a; done
    f="$STUB_VIEWS/$3"
    [ -f "$f" ] || { printf 'stub gh: no issue %s\n' "$3" >&2; exit 1; }
    if [ -n "$q" ]; then jq -r "$q" "$f"; else cat "$f"; fi ;;
  'api graphql')
    f=""
    for a in "$@"; do case "$a" in field=*) f=${a#field=} ;; esac; done
    if [ -n "$f" ] && [ -f "$STUB_PAGES/$f" ]; then cat "$STUB_PAGES/$f"; else cat "$STUB_PAGES/default"; fi ;;
  'api '*)
    # `--paginate` sits between `api` and the path, so the path is found by
    # scanning rather than by position.
    q="" path="" prev=""
    for a in "$@"; do
      [ "$prev" = --jq ] && q=$a
      case "$a" in repos/*/milestones*) path=$a ;; esac
      prev=$a
    done
    [ -n "$path" ] || { printf 'stub gh: unexpected api call: %s\n' "$*" >&2; exit 1; }
    if [ -n "$q" ]; then jq -r "$q" "$STUB_MILESTONES"; else cat "$STUB_MILESTONES"; fi ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$SCRATCH/sel/bin/gh"

printf '288\tmilestoned v0.2.0\n' >"$SCRATCH/sel/lists/story#v0.2.0"
printf '291\tmilestoned v0.3.0\n' >"$SCRATCH/sel/lists/story#v0.3.0"
printf '302\tunmilestoned\n'      >"$SCRATCH/sel/lists/story#"
printf '305\ta task, no milestone\n' >"$SCRATCH/sel/lists/task#"
printf '307\ta task in v0.2.0\n'     >"$SCRATCH/sel/lists/task#v0.2.0"

printf '{"number":302,"title":"unmilestoned","state":"OPEN"}\n'  >"$SCRATCH/sel/views/302"
printf '{"number":404,"title":"already done","state":"CLOSED"}\n' >"$SCRATCH/sel/views/404"

# Only 288 carries the pinned value, so a project scope that was *not* cleared
# selects 288 or nothing — never the unmilestoned 302 the cleared run wants.
page "[$(issue_item 288 z5labs/devex Todo),
       $(issue_item 302 z5labs/devex Done)]" >"$SCRATCH/sel/pages/Status"
page '[]' >"$SCRATCH/sel/pages/default"

MILESTONES_ALL='[{"title":"v0.2.0"},{"title":"v0.3.0"}]'

# sel_run <label> <milestone json> <project json|-> <milestones json> [args...]
sel_run() {
  local label=$1 milestone=$2 project=$3 milestones=$4
  shift 4
  local block=""
  [ "$project" = - ] || block=",
    \"project\": $project"
  cat >"$SCRATCH/sel/repo/.claude/backlog.json" <<JSON
{
  "select": { "label": "$label", "milestone": $milestone, "limit": 200$block },
  "dependencies": { "style": "none" },
  "verify": ["true"],
  "merge": { "label": "auto-merge", "workflow": "auto-merge.yaml" },
  "review": $REVIEW_BLOCK,
  "worktreeDir": ".claude/worktrees"
}
JSON
  printf '%s\n' "$milestones" >"$SCRATCH/sel/milestones"
  RUN_OUT=$(cd "$SCRATCH/sel/repo" && PATH="$SCRATCH/sel/bin:$PATH" \
    STUB_LISTS="$SCRATCH/sel/lists" STUB_VIEWS="$SCRATCH/sel/views" \
    STUB_PAGES="$SCRATCH/sel/pages" STUB_MILESTONES="$SCRATCH/sel/milestones" \
    "$SUT" "$@" 2>"$SCRATCH/sel/err")
  RUN_RC=$?
  RUN_ERR=$(tr '\n' ' ' <"$SCRATCH/sel/err")
}

# checkr <name> <expected issue number> <label> <milestone json> <project|-> <milestones json> [args...]
checkr() {
  local name=$1 want=$2
  shift 2
  sel_run "$@"
  local got
  got=$(printf '%s' "$RUN_OUT" | jq -r '.number // empty' 2>/dev/null)
  if [ "$RUN_RC" -eq 0 ] && [ "$got" = "$want" ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [#%s]\n' "$name" "$got"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want [#%s] exit 0, got [%s] exit %d: %s\n' \
      "$name" "$want" "${got:-<none>}" "$RUN_RC" "$RUN_ERR"
  fi
}

# checkr_fail <name> <substring> <label> <milestone json> <project|-> <milestones json> [args...]
checkr_fail() {
  local name=$1 want=$2
  shift 2
  sel_run "$@"
  case "$RUN_ERR" in
    *"$want"*) ;;
    *) fail=$((fail + 1))
       printf '  FAIL %-58s message lacks [%s]: %s\n' "$name" "$want" "$RUN_ERR"
       return ;;
  esac
  if [ "$RUN_RC" -eq 4 ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [exit 4]\n' "$name"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want exit 4, got exit %d\n' "$name" "$RUN_RC"
  fi
}

# checkr_note <name> <substring of stderr> <label> <milestone json> <project|-> <milestones json> [args...]
checkr_note() {
  local name=$1 want=$2
  shift 2
  sel_run "$@"
  case "$RUN_ERR" in
    *"$want"*)
      pass=$((pass + 1))
      printf '  ok   %-58s [noted]\n' "$name" ;;
    *) fail=$((fail + 1))
       printf '  FAIL %-58s diagnostics lack [%s]: %s\n' "$name" "$want" "$RUN_ERR" ;;
  esac
}

# Precedence, one key at a time. Every case answers with a different issue
# number, so a flag that was accepted and then ignored fails here.
checkr 'a configured milestone is used when no flag overrides it' 288 \
  story '"v0.2.0"' - "$MILESTONES_ALL"

checkr '--milestone overrides the configured milestone' 291 \
  story '"v0.2.0"' - "$MILESTONES_ALL" --milestone v0.3.0

checkr 'the --milestone=value form does the same' 291 \
  story '"v0.2.0"' - "$MILESTONES_ALL" --milestone=v0.3.0

checkr '--milestone names one where the config has none' 291 \
  story null - "$MILESTONES_ALL" --milestone v0.3.0

checkr '--no-milestone-filter drops the configured milestone' 302 \
  story '"v0.2.0"' - "$MILESTONES_ALL" --no-milestone-filter

checkr '--label overrides the configured label' 307 \
  story '"v0.2.0"' - "$MILESTONES_ALL" --label task

checkr '--label and --milestone compose' 305 \
  story '"v0.2.0"' - "$MILESTONES_ALL" --label task --no-milestone-filter

# One word for "no optional narrowing at all", so a caller does not have to know
# which axes this repository happens to configure. 302 rather than 288 is the
# assertion: had either narrowing survived, the pinned Status scope holds only
# 288 and the v0.2.0 list holds only 288.
checkr '--all drops the milestone and the project scope together' 302 \
  story '"v0.2.0"' '{"owner":"z5labs","number":14,"field":"Status","value":"Todo"}' \
  "$MILESTONES_ALL" --all

# The label is not an optional narrowing but the definition of the backlog, so
# `--all` leaves it alone and `--label` still applies beside it.
checkr '--all keeps the label, and --label still applies' 305 \
  story '"v0.2.0"' '{"owner":"z5labs","number":14,"field":"Status","value":"Todo"}' \
  "$MILESTONES_ALL" --all --label task

# The reported failure, as a test. A milestone that no longer exists has to be
# exit 4 naming the ones that do — from the config, which is where the stale
# value lived, and not only from the flag.
checkr_fail 'an unknown --milestone names the milestones that exist' 'v0.2.0,v0.3.0' \
  story null - "$MILESTONES_ALL" --milestone v0.9.9

checkr_fail 'an unknown --milestone blames the flag' '--milestone names milestone' \
  story null - "$MILESTONES_ALL" --milestone v0.9.9

checkr_fail 'a stale configured milestone is refused, not empty' 'select.milestone names milestone' \
  story '"v0.2.0"' - '[]'

checkr_fail 'a repository with no milestones at all says so' 'no milestones at all' \
  story '"v0.2.0"' - '[]'

# Mutually exclusive combinations. Each would otherwise have to discard one of
# the two silently, and discarding either is a run that is not the run asked for.
checkr_fail '--milestone and --no-milestone-filter is refused' 'cannot be combined with --milestone' \
  story null - "$MILESTONES_ALL" --milestone v0.3.0 --no-milestone-filter

checkr_fail '--all and --milestone is refused' 'cannot be combined with --milestone' \
  story null - "$MILESTONES_ALL" --all --milestone v0.3.0

checkr_fail '--all and a project flag is refused' 'cannot be combined with --project-value' \
  story null - "$MILESTONES_ALL" --all --project-value Todo

checkr_fail '--all and --no-project-filter is refused' 'cannot be combined with --no-project-filter' \
  story null - "$MILESTONES_ALL" --all --no-project-filter

# `--issue` names one issue instead of searching for one. 302 is invisible to
# both configured narrowings — it is unmilestoned and out of the pinned scope —
# so selecting it is the whole assertion.
checkr '--issue selects an issue the narrowings would have excluded' 302 \
  story '"v0.2.0"' '{"owner":"z5labs","number":14,"field":"Status","value":"Todo"}' \
  "$MILESTONES_ALL" --issue 302

checkr 'the --issue=N form does the same' 302 \
  story '"v0.2.0"' - "$MILESTONES_ALL" --issue=302

# A run that selected an out-of-backlog issue has to say so. Without these lines
# its diagnostics read exactly like a run that searched and found it at the top.
checkr_note '--issue names the label narrowing it bypassed' "bypassed the label narrowing (label 'story')" \
  story '"v0.2.0"' - "$MILESTONES_ALL" --issue 302

checkr_note '--issue names the milestone narrowing it bypassed' "bypassed the milestone narrowing (milestone 'v0.2.0')" \
  story '"v0.2.0"' - "$MILESTONES_ALL" --issue 302

checkr_note '--issue names the project narrowing it bypassed' "bypassed the project narrowing (Status = 'Todo'" \
  story '"v0.2.0"' '{"owner":"z5labs","number":14,"field":"Status","value":"Todo"}' \
  "$MILESTONES_ALL" --issue 302

checkr_note '--issue says the backlog was not searched' 'the backlog was not searched' \
  story null - "$MILESTONES_ALL" --issue 302

checkr_fail '--issue refuses a closed issue' 'is CLOSED, not OPEN' \
  story null - "$MILESTONES_ALL" --issue 404

checkr_fail '--issue refuses a number that is not one' 'must be a positive integer' \
  story null - "$MILESTONES_ALL" --issue latest

checkr_fail '--issue and --label is refused' 'cannot be combined with --label' \
  story null - "$MILESTONES_ALL" --issue 302 --label task

checkr_fail '--issue and --milestone is refused' 'cannot be combined with --milestone' \
  story null - "$MILESTONES_ALL" --issue 302 --milestone v0.3.0

checkr_fail '--issue and a project flag is refused' 'cannot be combined with --project-value' \
  story null - "$MILESTONES_ALL" --issue 302 --project-value Todo

checkr_fail '--issue and --all is refused' 'cannot be combined with --all' \
  story null - "$MILESTONES_ALL" --issue 302 --all

checkr_fail 'an unknown argument is refused' "unknown argument '--milestones'" \
  story null - "$MILESTONES_ALL" --milestones v0.3.0

# An empty value is never how a narrowing is cleared, and the remedy differs by
# flag. Naming the wrong one — telling a caller to pass `--all` when what they
# emptied was the label — is worse than naming none, because `--all` is a
# different run that would have succeeded.
checkr_fail 'an empty --milestone points at the clearing flags' 'pass --no-milestone-filter or --all' \
  story null - "$MILESTONES_ALL" --milestone=

checkr_fail 'an empty --project-value points at --no-project-filter' 'pass --no-project-filter or --all' \
  story null - "$MILESTONES_ALL" --project-value=

checkr_fail 'an empty --label says the label cannot be cleared' 'there is nothing to clear' \
  story null - "$MILESTONES_ALL" --label=

# The usage text every argument error prints. `--all` composes with `--label`,
# so a flat `| --all` at the end of one line would tell the caller their valid
# run is invalid.
checkr_fail 'the usage text shows --all composing with --label' 'select-issue.sh [--label <name>] --all' \
  story null - "$MILESTONES_ALL" --nonsense

printf '\nthe reviewer roster\n'

# The roster is validated here, at selection, rather than at step 7 where it is
# read. Everything wrong with it is wrong before an issue is branched, and by
# step 7 the implementation is written, CI is green and a pull request is open —
# at which point the only remedy is an edit to `.claude/backlog.json`, which is
# the one move a run is not allowed to make to get itself unstuck. Same
# reasoning as the empty `verify` check, and the same place in the script.
#
# `roster_fail <name> <substring> <review json>` and `roster_ok` both run the
# whole script; a roster that passes has to still select an issue, so a check
# that rejects everything cannot pass this section by accident.
roster_run() { # <review json> [args...]
  local review=$1; shift
  REVIEW_BLOCK=$review sel_run story null - "$MILESTONES_ALL" "$@"
}

roster_ok() { # <name> <review json>
  roster_run "$2"
  if [ "$RUN_RC" -eq 0 ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [accepted]\n' "$1"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want exit 0, got exit %d: %s\n' "$1" "$RUN_RC" "$RUN_ERR"
  fi
}

roster_fail() { # <name> <substring> <review json>
  roster_run "$3"
  case "$RUN_ERR" in
    *"$2"*) ;;
    *) fail=$((fail + 1))
       printf '  FAIL %-58s message lacks [%s]: %s\n' "$1" "$2" "$RUN_ERR"
       return ;;
  esac
  if [ "$RUN_RC" -eq 4 ]; then
    pass=$((pass + 1)); printf '  ok   %-58s [exit 4]\n' "$1"
  else
    fail=$((fail + 1)); printf '  FAIL %-58s want exit 4, got exit %d\n' "$1" "$RUN_RC"
  fi
}

roster_ok 'a single-rung roster'            '{ "reviewers": ["copilot"] }'
roster_ok 'a roster that fails over'        '{ "reviewers": ["copilot", "local"] }'
roster_ok 'a roster ending in none'         '{ "reviewers": ["copilot", "local", "none"] }'
roster_ok 'none alone, an unreviewed merge chosen on purpose' '{ "reviewers": ["none"] }'

roster_fail 'an unknown rung names the rungs that exist' "unknown rung 'coderabbit'" \
  '{ "reviewers": ["copilot", "coderabbit"] }'

# The generic rung. A bare login is not a rung — `bot:` is what says "this is a
# review bot, and the plugin knows nothing about it beyond how to ask it", which
# is the distinction the escalation at step 7 rests on.
roster_ok 'a bot rung names any installed review bot' \
  '{ "reviewers": ["copilot", "bot:coderabbitai[bot]"] }'

roster_ok 'a bot rung alone, with a refusal wording supplied' \
  '{ "reviewers": ["bot:coderabbitai[bot]"], "refusals": { "bot:coderabbitai[bot]": "[Rr]eview skipped" } }'

roster_fail 'a bot rung with no login says what to write' "'bot:' rung with no login" \
  '{ "reviewers": ["bot:"] }'

# A login holding a space or a comma cannot survive `--reviewers a,b,c`, which
# is how the driver overrides the roster for a run.
roster_fail 'a bot login with a space is refused' "a GitHub login holds no whitespace" \
  '{ "reviewers": ["bot:code rabbit"] }'

roster_fail 'a bot login with a comma is refused' "a GitHub login holds no whitespace" \
  '{ "reviewers": ["bot:a,b"] }'

# The refusal wording, which is the operator asserting they have SEEN that bot
# decline. Everything wrong with it has to be wrong here: at step 7 a pattern
# that will not compile reads as "did not match", which turns a refusal into a
# completed review and merges it.
roster_fail 'a refusal pattern that will not compile is refused' 'not a usable POSIX extended regular expression' \
  '{ "reviewers": ["bot:coderabbitai[bot]"], "refusals": { "bot:coderabbitai[bot]": "*(" } }'

# REGRESSION: without `--`, grep reads a pattern opening with a dash as options
# and answers 2, so a perfectly good pattern is rejected here as uncompilable —
# and at step 7 the same parse means the review body is never tested at all.
roster_ok 'a refusal pattern opening with a dash still compiles' \
  '{ "reviewers": ["bot:coderabbitai[bot]"], "refusals": { "bot:coderabbitai[bot]": "-- review skipped" } }'

roster_fail 'an empty refusal pattern matches every review' 'review.refusals["bot:coderabbitai[bot]"] is empty' \
  '{ "reviewers": ["bot:coderabbitai[bot]"], "refusals": { "bot:coderabbitai[bot]": "" } }'

roster_fail 'a refusal pattern that is not a string' 'must be a string holding' \
  '{ "reviewers": ["bot:coderabbitai[bot]"], "refusals": { "bot:coderabbitai[bot]": true } }'

roster_fail 'refusals that are not an object' 'must be an object keyed by rung' \
  '{ "reviewers": ["copilot"], "refusals": ["[Rr]eview skipped"] }'

# A key that does nothing is worse than a key that fails: the operator writes a
# wording, the run ignores it, and the rung goes on classifying by the rules
# they think they replaced. Copilot's wording is the plugin's, under either
# spelling of the rung.
roster_fail 'a refusal keyed to copilot is refused, not ignored' 'is built into await-review.sh' \
  '{ "reviewers": ["copilot"], "refusals": { "copilot": "[Rr]eview skipped" } }'

roster_fail 'a refusal keyed to copilots login is refused too' 'is built into await-review.sh' \
  '{ "reviewers": ["copilot"], "refusals": { "bot:copilot-pull-request-reviewer[bot]": "[Rr]eview skipped" } }'

roster_fail 'a refusal keyed to local is refused' 'is built into await-review.sh' \
  '{ "reviewers": ["copilot", "local"], "refusals": { "local": "[Rr]eview skipped" } }'

roster_fail 'a refusal keyed to a login with no bot: prefix' 'its keys are bot:<login> rungs' \
  '{ "reviewers": ["copilot"], "refusals": { "coderabbitai[bot]": "[Rr]eview skipped" } }'

roster_fail 'an empty roster says which of the two is meant' 'review.reviewers is empty' \
  '{ "reviewers": [] }'

# REGRESSION: the rungs were joined into a string and split by the shell, so one
# element holding two words validated as two legal rungs — a roster nobody wrote
# and every rung of it reachable. Word splitting also let a rung containing a
# glob expand against the working directory on the way past, which is the same
# defect wearing a different hat.
roster_fail 'one element holding two rungs is one unknown rung' "unknown rung 'copilot local'" \
  '{ "reviewers": ["copilot local"] }'

roster_fail 'a rung that is a glob is not expanded' "unknown rung '*'" \
  '{ "reviewers": ["*"] }'

roster_fail 'an empty string is a rung, and an unknown one' "unknown rung ''" \
  '{ "reviewers": [""] }'

# `none` last is a downgrade its operator chose; `none` first is a roster whose
# fallbacks can never run, which reads as though it has them.
roster_fail 'none before another rung is refused' "'none' at position 1 of 2" \
  '{ "reviewers": ["none", "copilot"] }'

roster_fail 'none in the middle is refused' "'none' at position 2 of 3" \
  '{ "reviewers": ["copilot", "none", "local"] }'

roster_fail 'a missing roster points at the key' 'review.reviewers is missing' \
  '{ }'

roster_fail 'a roster that is not an array' 'must be an array of rung names' \
  '{ "reviewers": "copilot" }'

# The migration. `required` is mechanical to translate, which is exactly why it
# is refused rather than translated: this key changed because a repository whose
# Copilot quota is exhausted has to learn a roster exists, and a config that
# keeps working unchanged is a config nobody reads.
roster_fail 'review.required is refused, not equivalenced' 'review.required is no longer read' \
  '{ "required": true }'

roster_fail 'review.required true names its replacement' '["copilot"]' \
  '{ "required": true }'

roster_fail 'review.required false names its replacement' '["none"]' \
  '{ "required": false }'

# A config carrying both is the half-done migration, and it is the dangerous
# one: `reviewers` would be read and `required` would sit there looking
# authoritative to whoever next opens the file.
roster_fail 'a config carrying both is still refused' 'review.required is no longer read' \
  '{ "required": true, "reviewers": ["copilot"] }'

printf '\ncross-repository dependencies\n'

# The dependency walk itself, end to end, under `dependencies.style` = `native`
# — the one part of selection the `--native-deps` seam above cannot reach,
# because the seam stops at printing references and the bug this section covers
# is in what the walk does with them afterwards.
#
# A cross-repository `owner/repo#N` used to be recorded as an unmet blocker
# without its state ever being read, so it never decayed when the dependency
# landed: the issue holding it was unreachable for the rest of the repository's
# life. It is silent in the ordinary case — that issue is skipped and the
# backlog keeps moving — and only surfaces as BLOCKED once it is the last
# candidate, at which point the report blames the dependency rather than the
# resolver.
#
# `gh` answers from files here the same way it does above: the `blockedBy`
# response is chosen by the `number=` the query was given, and an issue's state
# by the `--repo` and number it was asked for. A state fixture that is absent is
# a `gh` that failed, which is how the fail-closed half is exercised — that half
# is the reason the old behaviour was written, and it has to survive the fix.
mkdir -p "$SCRATCH/nrepo/.claude" "$SCRATCH/nbin" "$SCRATCH/deps" "$SCRATCH/states"

git init -q "$SCRATCH/nrepo" >/dev/null 2>&1 \
  || { printf 'cannot init a scratch repository\n' >&2; exit 1; }

cat >"$SCRATCH/nbin/gh" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  'repo view')  printf 'z5labs/devex\n' ;;
  'issue list') cat "$STUB_ISSUES" ;;
  'issue view')
    # Two callers. `dep_state` asks for `--json state --jq .state` and is the
    # only thing that views an issue at all under style `native`, where the body
    # is never read. `--issue <n>` asks for `--json number,title,state` with no
    # `--jq`, so the absence of one is what tells the two apart — and only the
    # dep_state reads are counted, since the cache assertion is about those.
    n=$3 repo="" q="" prev=""
    for a in "$@"; do
      [ "$prev" = --repo ] && repo=$a
      [ "$prev" = --jq ] && q=$a
      prev=$a
    done
    f="$STUB_STATES/${repo//\//_}#$n"
    [ -f "$f" ] || { printf 'stub gh: cannot view %s#%s\n' "$repo" "$n" >&2; exit 1; }
    if [ -n "$q" ]; then
      printf '%s#%s\n' "$repo" "$n" >>"$STUB_CALLS"
      cat "$f"
    else
      printf '{"number":%s,"title":"issue %s","state":"%s"}\n' "$n" "$n" "$(cat "$f")"
    fi ;;
  'api graphql')
    n=""
    for a in "$@"; do case "$a" in number=*) n=${a#number=} ;; esac; done
    f="$STUB_DEPS/$n"
    [ -f "$f" ] || { printf 'stub gh: no blockedBy fixture for #%s\n' "$n" >&2; exit 1; }
    cat "$f" ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$SCRATCH/nbin/gh"

cat >"$SCRATCH/nrepo/.claude/backlog.json" <<'JSON'
{
  "select": { "label": "story", "milestone": null, "limit": 200 },
  "dependencies": { "style": "native" },
  "verify": ["true"],
  "merge": { "label": "auto-merge", "workflow": "auto-merge.yaml" },
  "review": { "reviewers": ["copilot"] },
  "worktreeDir": ".claude/worktrees"
}
JSON

nreset() { rm -rf "$SCRATCH/deps" "$SCRATCH/states"; mkdir -p "$SCRATCH/deps" "$SCRATCH/states"; }

ndeps()  { npage "$2" "$1" >"$SCRATCH/deps/$1"; }          # <issue> <blockedBy nodes>
nstate() { printf '%s\n' "$3" >"$SCRATCH/states/${1//\//_}#$2"; }  # <repo> <number> <state>

# nrun <issue numbers, space separated> [args...]
nrun() {
  : >"$SCRATCH/nissues"
  local n
  for n in $1; do printf '%s\tissue %s\n' "$n" "$n" >>"$SCRATCH/nissues"; done
  shift
  : >"$SCRATCH/calls"
  RUN_OUT=$(cd "$SCRATCH/nrepo" && PATH="$SCRATCH/nbin:$PATH" \
    STUB_ISSUES="$SCRATCH/nissues" STUB_DEPS="$SCRATCH/deps" \
    STUB_STATES="$SCRATCH/states" STUB_CALLS="$SCRATCH/calls" \
    "$SUT" "$@" 2>"$SCRATCH/nerr")
  RUN_RC=$?
  RUN_ERR=$(tr '\n' ' ' <"$SCRATCH/nerr")
}

# checkd <name> <expected issue number> <reason substring, or - for none> <issue numbers> [args...]
checkd() {
  local name=$1 want=$2 reason=$3 got
  shift 3
  nrun "$@"
  got=$(printf '%s' "$RUN_OUT" | jq -r '.number // empty' 2>/dev/null)
  if [ "$RUN_RC" -ne 0 ] || [ "$got" != "$want" ]; then
    fail=$((fail + 1))
    printf '  FAIL %-58s want [#%s] exit 0, got [%s] exit %d: %s\n' \
      "$name" "$want" "${got:-<none>}" "$RUN_RC" "$RUN_ERR"
    return
  fi
  if [ "$reason" != - ]; then
    case "$RUN_ERR" in
      *"$reason"*) ;;
      *) fail=$((fail + 1))
         printf '  FAIL %-58s reason lacks [%s]: %s\n' "$name" "$reason" "$RUN_ERR"
         return ;;
    esac
  fi
  pass=$((pass + 1))
  printf '  ok   %-58s [#%s]\n' "$name" "$got"
}

# checkd_blocked <name> <substring of the report> <issue numbers> [args...]
checkd_blocked() {
  local name=$1 want=$2 report
  shift 2
  nrun "$@"
  report=$(printf '%s' "$RUN_OUT" | tr '\n' ' ')
  case "$report" in
    *"$want"*) ;;
    *) fail=$((fail + 1))
       printf '  FAIL %-58s report lacks [%s]: %s\n' "$name" "$want" "$report"
       return ;;
  esac
  if [ "$RUN_RC" -eq 11 ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s [exit 11]\n' "$name"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s want exit 11, got exit %d: %s\n' "$name" "$RUN_RC" "$report"
  fi
}

# The reported reproduction, transposed onto this fixture's repository names:
# every dependency of both candidates is closed, two of them elsewhere, so the
# lowest-numbered candidate is eligible. This used to report BLOCKED on a
# backlog that was fully workable.
nreset
ndeps 63 "[$(dep 328 z5labs/other),$(dep 32 z5labs/devex)]"
ndeps 65 "[$(dep 330 z5labs/other),$(dep 64 z5labs/devex)]"
nstate z5labs/other 328 CLOSED
nstate z5labs/other 330 CLOSED
nstate z5labs/devex 32  CLOSED
nstate z5labs/devex 64  CLOSED
checkd 'a closed cross-repository dependency no longer blocks' 63 - '63 65'

# The other half of the same read: an edge elsewhere that is genuinely open is
# still a blocker, and the report says what it is rather than that it could not
# be looked at.
nreset
ndeps 63 "[$(dep 328 z5labs/other)]"
ndeps 65 '[]'
nstate z5labs/other 328 OPEN
checkd 'an open cross-repository dependency still blocks' 65 'z5labs/other#328 is OPEN' '63 65'

# Fail-closed, preserved. A private repository, a token without the scope, a
# deleted issue: the state cannot be read, so the edge stays a blocker. Calling
# the issue eligible here is the failure the original behaviour was defending
# against, and removing the case where it fired wrongly must not remove this.
nreset
ndeps 63 "[$(dep 328 z5labs/other)]"
ndeps 65 '[]'
checkd 'an unreadable cross-repository dependency still blocks' 65 'z5labs/other#328 is UNREADABLE' '63 65'

nreset
ndeps 63 "[$(dep 328 z5labs/other)]"
checkd_blocked 'an unreadable edge is named in the BLOCKED report' \
  'z5labs/other#328 is UNREADABLE' '63'

# An issue number is only unique within its repository, so the state cache has
# to be keyed on `owner/repo#N`. Keyed on the bare number, this repository's
# closed #12 would answer for the other repository's open one and #63 would come
# out eligible with a live blocker.
nreset
ndeps 63 "[$(dep 12 z5labs/devex),$(dep 12 z5labs/other)]"
ndeps 65 '[]'
nstate z5labs/devex 12 CLOSED
nstate z5labs/other 12 OPEN
checkd 'a cached same-repo state does not answer for elsewhere' 65 'z5labs/other#12 is OPEN' '63 65'

# The same collision in the other order, which a cache keyed on the number would
# also get wrong — and this direction is the one that reads as eligible.
nreset
ndeps 63 "[$(dep 12 z5labs/other),$(dep 12 z5labs/devex)]"
ndeps 65 '[]'
nstate z5labs/other 12 CLOSED
nstate z5labs/devex 12 OPEN
checkd 'a cached cross-repo state does not answer for here' 65 '#12 is OPEN' '63 65'

# The cache is why a state is read once per dependency rather than once per edge
# pointing at it. Two candidates blocked by the same issue elsewhere: one view.
nreset
ndeps 63 "[$(dep 328 z5labs/other)]"
ndeps 65 "[$(dep 328 z5labs/other)]"
nstate z5labs/other 328 OPEN
checkd_blocked 'both candidates blocked by one edge elsewhere' \
  'z5labs/other#328 is OPEN' '63 65'
CALLS=$(grep -c . "$SCRATCH/calls")
if [ "$CALLS" -eq 1 ]; then
  pass=$((pass + 1))
  printf '  ok   %-58s [1 view]\n' 'a cross-repository state is read once and cached'
else
  fail=$((fail + 1))
  printf '  FAIL %-58s want 1 view, got %d: %s\n' \
    'a cross-repository state is read once and cached' "$CALLS" "$(tr '\n' ' ' <"$SCRATCH/calls")"
fi

# `--issue <n>` changes which issues are considered and nothing about whether the
# one considered is workable — the dependency walk runs against it exactly as it
# would have if the search had turned it up. Naming an issue is a statement about
# order, not a licence to start something whose blockers are open.
nreset
ndeps 63 "[$(dep 328 z5labs/other)]"
nstate z5labs/other 328 OPEN
nstate z5labs/devex 63 OPEN
checkd_blocked '--issue still walks the dependencies' \
  'z5labs/other#328 is OPEN' '63 65' --issue 63

# And the report blames the issue that was named rather than describing a
# backlog search that never happened.
checkd_blocked '--issue names itself in the BLOCKED report' \
  'issue #63, named with --issue' '63 65' --issue 63

nreset
ndeps 63 "[$(dep 328 z5labs/other)]"
nstate z5labs/other 328 CLOSED
nstate z5labs/devex 63 OPEN
checkd '--issue selects when its dependencies are all CLOSED' 63 - '63 65' --issue 63

# Out of the listed backlog entirely — the case `--issue` exists for. #77 is not
# in the issue list at all, so a run that quietly fell back to searching would
# answer #63.
nreset
ndeps 77 '[]'
nstate z5labs/devex 77 OPEN
checkd '--issue reaches an issue the list never returned' 77 - '63 65' --issue 77

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
