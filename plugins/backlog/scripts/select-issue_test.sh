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
# Run: plugins/backlog/scripts/select-issue_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/select-issue.sh"
[ -x "$SUT" ] || { printf 'select-issue.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

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

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
