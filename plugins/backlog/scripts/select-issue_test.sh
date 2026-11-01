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
# instead of the body, so its corpus is REST responses rather than markdown —
# `GET /repos/{owner}/{repo}/issues/{n}/dependencies/blocked_by`, one JSON array
# of the blocking issues per page — fixtured the same way, and read through the
# same `--native-deps` seam the walk uses, so nothing here needs the network.
#
# Each edge prints as `<ref>\t<STATE>`; `checkn` shows the tab as `=`. The state
# is the blocker's own, from the same response, which is why a native edge needs
# no second read to resolve.
#
# The failures carry the weight. Every one of them would otherwise resolve to an
# empty reference list, and an empty reference list is indistinguishable from an
# issue with no blockers — which is precisely the wrong answer this style exists
# to remove, arrived at from the other direction.

dep() { # <number> <repo> [open|closed] — one blocking issue, as REST returns it
  printf '{"id":%s001,"number":%s,"state":"%s","title":"blocker %s","repository_url":"https://api.github.com/repos/%s"}' \
    "$1" "$1" "${3:-closed}" "$1" "$2"
}

# checkn <name> <repo> <expected edges, space separated, ref=STATE> <pages>
checkn() {
  local name=$1 repo=$2 want=$3 pages=$4 got rc
  got=$(printf '%s' "$pages" | "$SUT" --native-deps "$repo" 2>/dev/null)
  rc=$?
  got=$(printf '%s' "$got" | tr '\t\n' '= ')
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
checkn 'the documented fixture, as typed edges' z5labs/devex '#12=CLOSED #14=OPEN' \
  "[$(dep 12 z5labs/devex closed),$(dep 14 z5labs/devex open)]"

# The honest empty, and the reason the style has to be declared rather than
# fallen back to: on a repository that has not populated dependencies this is
# every issue's answer, and it reads as an unblocked backlog.
checkn 'no dependencies is an empty edge set, not an error' z5labs/devex '' '[]'

# A cross-repository edge is named by its blocker's `repository_url`, in the form
# it would be written, so the caller can tell "issue 14 here" from "issue 42
# elsewhere" — the same distinction the body extractor draws.
checkn 'a cross-repository blocker is named by its repository_url' z5labs/devex 'z5labs/other#42=OPEN #14=CLOSED' \
  "[$(dep 42 z5labs/other open),$(dep 14 z5labs/devex closed)]"

# An issue number is only unique within its repository. Keyed on the number
# alone, `z5labs/other#12` would collapse into `#12` and be resolved against
# this repository's issue 12 — a dependency on the wrong issue entirely.
checkn 'the same number in two repositories stays two edges' z5labs/devex '#12=CLOSED z5labs/other#12=OPEN' \
  "[$(dep 12 z5labs/devex closed),$(dep 12 z5labs/other open)]"

# GitHub treats owner and name without regard to case, so a repository_url that
# spells this repository differently is still this repository — read as another,
# an in-repository edge would be resolved as a cross-repository one.
checkn 'this repository in another case is still this repository' z5labs/devex '#12=OPEN' \
  "[$(dep 12 Z5labs/DevEx open)]"

# `--paginate` prints one JSON array per page, concatenated. Reading only the
# first would drop every dependency past 100 — an issue that looks less blocked
# than it is.
checkn 'every page contributes, deduped, order preserved' z5labs/devex '#12=CLOSED #14=CLOSED #19=OPEN' \
  "[$(dep 12 z5labs/devex),$(dep 14 z5labs/devex)]
[$(dep 14 z5labs/devex),$(dep 19 z5labs/devex open)]"

# The failures, each of which would otherwise print nothing and exit 0.
checkn_fail 'an empty response is refused' z5labs/devex \
  'returned no response' ''

# GitHub's error shape where a list belongs: a missing issue, or a GitHub that
# does not serve typed dependencies. Reading it as "no blockers" is the whole
# failure mode.
checkn_fail 'an error object instead of a list is refused' z5labs/devex \
  'not a list of issues' '{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}'

checkn_fail 'a blocker with no repository_url is refused, not filtered' z5labs/devex \
  'no number, repository_url or state' '[{"number":12,"state":"open"}]'

checkn_fail 'a blocker with no state is refused, not filtered' z5labs/devex \
  'no number, repository_url or state' '[{"number":12,"repository_url":"https://api.github.com/repos/z5labs/devex"}]'

checkn_fail 'a blocker with no number is refused, not filtered' z5labs/devex \
  'no number, repository_url or state' '[{"state":"open","repository_url":"https://api.github.com/repos/z5labs/devex"}]'

checkn_fail 'a response that is not JSON is refused' z5labs/devex \
  'does not parse as JSON' 'not json at all'

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

# Every stub below refuses GraphQL the way a Claude Code cloud session's GitHub
# proxy does, and records each refusal; the last case of this file asserts that
# none happened. The project-scope cases are the one place the guard is told to
# let `gh api graphql` through, by GQL=allow — Projects v2 exists only in
# GraphQL, and those cases are about a local run.
export GH_GUARD="$HERE/testdata/gh-guard.sh"
export GH_GUARD_LOG="$SCRATCH/graphql-calls"
: >"$GH_GUARD_LOG"
GQL=""

# One `gh` for every end-to-end section of this file, answering the REST routes
# select-issue.sh reads from a fixture directory, $STUB_FX:
#
#   repos/z5labs/devex                  the repository, as repo.sh confirms it
#   repos/*/milestones                  $STUB_FX/milestones, a JSON array
#   repos/*/issues?labels=&milestone=   $STUB_FX/lists/<label>#<milestone number>,
#                                       one `number<TAB>title[<TAB>pr]` line per
#                                       issue in creation order, served a page at
#                                       a time by `page` and `per_page`; `pr`
#                                       marks a pull request
#   repos/*/issues/<n>/dependencies/    $STUB_FX/deps/<n>, a JSON array
#     blocked_by
#   repos/<o>/<r>/issues/<n>            $STUB_FX/states/<o>_<r>#<n> (OPEN, CLOSED,
#                                       or PR for a pull request) and
#                                       $STUB_FX/bodies/<o>_<r>#<n>
#
# An absent fixture is GitHub's 404. `--jq` is really run, so the fixtures are the
# shapes GitHub returns rather than the answers the script wants. Every call goes
# to $STUB_FX/calls, and every issue read to $STUB_FX/reads, so a case can assert
# what was NOT asked.
mkdir -p "$SCRATCH/bin"
cat >"$SCRATCH/bin/gh" <<'STUB'
#!/usr/bin/env bash
. "$GH_GUARD"
printf '%s\n' "$*" >>"$STUB_FX/calls"
q="" path="" prev=""
for a in "$@"; do
  [ "$prev" = --jq ] && q=$a
  case "$a" in repos/*) path=$a ;; esac
  prev=$a
done
out() { if [ -n "$q" ]; then jq -r "$q"; else cat; fi; }
missing() { printf '{"message":"Not Found"}'; printf 'gh: Not Found (HTTP 404)\n' >&2; exit 1; }

# Reached only past the guard, so only when a case set GH_GUARD_GRAPHQL=allow:
# the project query, answered by the `field=` it was given — which is what lets
# a case prove a flag reached the query rather than only the message.
if [ "$1 ${2:-}" = 'api graphql' ]; then
  f=""
  for a in "$@"; do case "$a" in field=*) f=${a#field=} ;; esac; done
  if [ -n "$f" ] && [ -f "$STUB_FX/pages/$f" ]; then cat "$STUB_FX/pages/$f"; else cat "$STUB_FX/pages/default"; fi
  exit 0
fi

[ "$1" = api ] && [ -n "$path" ] || { printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1; }
route=${path%%\?*}
query=""
[ "$route" = "$path" ] || query=${path#*\?}
param() { printf '&%s&' "$query" | sed -n "s/.*&$1=\([^&]*\)&.*/\1/p"; }

case "$route" in
  repos/z5labs/devex)
    printf '{"full_name":"z5labs/devex","default_branch":"main"}\n' | out ;;
  repos/*/milestones)
    out <"$STUB_FX/milestones" ;;
  repos/*/issues)
    label=$(param labels | sed 's/%20/ /g')
    ms=$(param milestone)
    f="$STUB_FX/lists/$label#$ms"
    [ -f "$f" ] || { printf 'stub gh: no issue list for label=%s milestone=%s\n' "$label" "$ms" >&2; exit 1; }
    page=$(param page); per=$(param per_page)
    awk -v page="${page:-1}" -v per="${per:-30}" 'NR > (page - 1) * per && NR <= page * per' "$f" \
      | jq -R -s -c 'split("\n") | map(select(length > 0) | split("\t")
          | {number: (.[0] | tonumber), title: .[1], state: "open"}
            + (if .[2] == "pr" then {pull_request: {url: "https://example.test/pull"}} else {} end))' \
      | out ;;
  repos/*/issues/*/dependencies/blocked_by)
    n=${route%/dependencies/blocked_by}; n=${n##*/}
    [ -f "$STUB_FX/deps/$n" ] || missing
    out <"$STUB_FX/deps/$n" ;;
  repos/*/issues/*)
    n=${route##*/}; r=${route#repos/}; r=${r%/issues/*}; key="${r//\//_}#$n"
    [ -f "$STUB_FX/states/$key" ] || missing
    printf '%s#%s\n' "$r" "$n" >>"$STUB_FX/reads"
    s=$(cat "$STUB_FX/states/$key")
    b=""
    [ -f "$STUB_FX/bodies/$key" ] && b=$(cat "$STUB_FX/bodies/$key")
    jq -n --argjson n "$n" --arg s "$s" --arg b "$b" \
      '{number: $n, title: "issue \($n)", state: ($s | ascii_downcase), body: $b}
       + (if $s == "PR" then {state: "open", pull_request: {url: "https://example.test/pull"}} else {} end)' \
      | out ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$SCRATCH/bin/gh"

# fixture_root <dir> — a scratch checkout at <dir>/repo whose origin is
# z5labs/devex, and the fixture directories the stub reads beside it.
fixture_root() {
  mkdir -p "$1/lists" "$1/deps" "$1/states" "$1/bodies" "$1/pages"
  git init -q "$1/repo" >/dev/null 2>&1 \
    || { printf 'cannot init a scratch repository\n' >&2; exit 1; }
  git -C "$1/repo" remote add origin https://github.com/z5labs/devex.git
  mkdir -p "$1/repo/.claude"
  printf '[]\n' >"$1/milestones"
}

# run_in <fixture root> [args...] — the whole script, from the checkout, with
# the stub on PATH. GQL is passed to the guard as it stands at the call.
run_in() {
  local fx=$1
  shift
  : >"$fx/calls"
  : >"$fx/reads"
  RUN_OUT=$(cd "$fx/repo" && PATH="$SCRATCH/bin:$PATH" STUB_FX="$fx" GH_GUARD_GRAPHQL="$GQL" \
    "$SUT" "$@" 2>"$fx/err")
  RUN_RC=$?
  RUN_ERR=$(tr '\n' ' ' <"$fx/err")
}

SCOPE="$SCRATCH/scope"
fixture_root "$SCOPE"
printf '288\tthe first\n291\tthe second\n302\tthe third\n' >"$SCOPE/lists/story#"

page "[$(issue_item 288 z5labs/devex pdf),
       $(issue_item 291 z5labs/devex workspace-ci),
       $(issue_item 302 z5labs/devex workspace-ci)]" >"$SCOPE/pages/Module"
page "[$(issue_item 288 z5labs/devex Done),
       $(issue_item 291 z5labs/devex Done),
       $(issue_item 302 z5labs/devex Todo)]" >"$SCOPE/pages/Status"
# A field the project does not have still gets the project's field list back,
# which is the whole reason `project_query` reads `fields` rather than
# `field(name:)`.
page '[]' >"$SCOPE/pages/default"

# run_sut <select.project json, or - for none> [args...]
run_sut() {
  local project=$1
  shift
  local block=""
  [ "$project" = - ] || block=",
    \"project\": $project"
  cat >"$SCOPE/repo/.claude/backlog.json" <<JSON
{
  "select": { "label": "story", "milestone": null, "limit": 200$block },
  "dependencies": { "style": "none" },
  "verify": ["true"],
  "merge": { "label": "auto-merge", "workflow": "auto-merge.yaml" },
  "review": $REVIEW_BLOCK,
  "worktreeDir": ".claude/worktrees"
}
JSON
  run_in "$SCOPE" "$@"
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

# The scope cases are a local run, the one place a project read is legitimate.
GQL=allow

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

printf '\nproject scope in a Claude Code cloud session\n'

# A cloud session's GitHub proxy refuses GraphQL, and Projects v2 exists only in
# GraphQL, so a requested scope has no route at all there. The guard now answers
# the project query exactly as that proxy does. The answer has to be exit 4
# saying so — never a fall-through to the unscoped backlog, which would get work
# done in the wrong order looking like an ordinary iteration.
#
# These refusals are the expected ones, so they are logged apart from the file
# the last case of this suite asserts is empty.
GQL=""
GUARD_LOG_KEPT=$GH_GUARD_LOG
export GH_GUARD_LOG="$SCRATCH/expected-refusals"
: >"$GH_GUARD_LOG"

checks_fail 'a configured project value is local-only in a cloud session' \
  'project-scoped selection is local-only in a Claude Code cloud session' "$PINNED"
checks_fail 'and names --no-project-filter as the way out' '--no-project-filter' "$PINNED"
# A scope from the command line is cleared by not passing it: --no-project-filter
# beside --project-value is refused, so naming only that would be a dead end.
checks_fail 'a scope from flags says to drop them' 'drop --project-value' "$BOARD" --project-value Todo

# The logged query spans lines, so it is the command lines that are counted.
if [ "$(grep -c '^api graphql' "$GH_GUARD_LOG")" = 3 ] \
   && ! grep -qE '^(issue|pr|project|search|repo view|label list|release list)( |$)' "$GH_GUARD_LOG"; then
  pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' 'the only refused call was the project query' '3 of 3'
else
  fail=$((fail + 1)); printf '  FAIL %-58s %s\n' 'the only refused call was the project query' "$(grep -E '^[a-z]' "$GH_GUARD_LOG" | cut -c1-60 | tr '\n' ';')"
fi
export GH_GUARD_LOG=$GUARD_LOG_KEPT

# This repository's own shape: a board and an axis configured, `value: null`.
# Selection makes no project call at all, so it is unaffected by the proxy.
checks 'a null project value makes no project call at all' 288 "$BOARD"
if grep -q graphql "$SCOPE/calls"; then
  fail=$((fail + 1)); printf '  FAIL %-58s %s\n' 'and no GraphQL call reached gh' "$(tr '\n' ';' <"$SCOPE/calls")"
else
  pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' 'and no GraphQL call reached gh' 'none'
fi
checks '--no-project-filter on a pinned value runs in the cloud' 288 "$PINNED" --no-project-filter

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
# The issue list is filtered by milestone NUMBER — `GET /repos/{o}/{r}/issues`
# takes no title — so the title is resolved to its number through
# `/milestones`, and the lists below are keyed by the number the query carried.
# A title that reached the query unresolved finds no list at all.
#
# The same shape as the scope-assembly section above: the whole script, a
# scratch repository, the same `gh`, and an assertion on *which issue* came back
# rather than on a message. Each fixture list holds a different issue number, so
# a flag that never reached the query is a different answer and not merely a
# different diagnostic.
SEL="$SCRATCH/sel"
fixture_root "$SEL"

printf '288\tmilestoned v0.2.0\n'    >"$SEL/lists/story#2"
printf '291\tmilestoned v0.3.0\n'    >"$SEL/lists/story#3"
printf '302\tunmilestoned\n'         >"$SEL/lists/story#"
printf '305\ta task, no milestone\n' >"$SEL/lists/task#"
printf '307\ta task in v0.2.0\n'     >"$SEL/lists/task#2"

printf 'OPEN\n'   >"$SEL/states/z5labs_devex#302"
printf 'CLOSED\n' >"$SEL/states/z5labs_devex#404"
printf 'PR\n'     >"$SEL/states/z5labs_devex#410"

# Only 288 carries the pinned value, so a project scope that was *not* cleared
# selects 288 or nothing — never the unmilestoned 302 the cleared run wants.
page "[$(issue_item 288 z5labs/devex Todo),
       $(issue_item 302 z5labs/devex Done)]" >"$SEL/pages/Status"
page '[]' >"$SEL/pages/default"

MILESTONES_ALL='[{"number":2,"title":"v0.2.0"},{"number":3,"title":"v0.3.0"}]'

# A local run, so a scope that was not cleared would be read rather than refused
# — and would then answer 288, which is the point of the fixture above.
GQL=allow

# sel_run <label> <milestone json> <project json|-> <milestones json> [args...]
# SEL_LIMIT replaces select.limit for the call it is set on.
sel_run() {
  local label=$1 milestone=$2 project=$3 milestones=$4
  shift 4
  local block=""
  [ "$project" = - ] || block=",
    \"project\": $project"
  cat >"$SEL/repo/.claude/backlog.json" <<JSON
{
  "select": { "label": "$label", "milestone": $milestone, "limit": ${SEL_LIMIT:-200}$block },
  "dependencies": { "style": "none" },
  "verify": ["true"],
  "merge": { "label": "auto-merge", "workflow": "auto-merge.yaml" },
  "review": $REVIEW_BLOCK,
  "worktreeDir": ".claude/worktrees"
}
JSON
  printf '%s\n' "$milestones" >"$SEL/milestones"
  run_in "$SEL" "$@"
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

# The title is resolved to its number, and the number is what reaches the issue
# list. Without that step there is no way to ask the REST endpoint for one
# milestone at all.
sel_run story null - "$MILESTONES_ALL" --milestone v0.3.0
if grep -q 'repos/z5labs/devex/issues?.*&milestone=3&' "$SEL/calls" && ! grep -q 'milestone=v0' "$SEL/calls"; then
  pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' 'a milestone title reaches the list as its number' 'milestone=3'
else
  fail=$((fail + 1)); printf '  FAIL %-58s %s\n' 'a milestone title reaches the list as its number' "$(grep 'issues?' "$SEL/calls" | tr '\n' ';')"
fi

# An absent milestone is no filter. Passing `milestone=none` instead would be
# "issues with no milestone", which is a different request that happens to give
# the same answer on this fixture.
sel_run story null - "$MILESTONES_ALL"
if grep -q 'issues?' "$SEL/calls" && ! grep -q 'milestone=' "$SEL/calls"; then
  pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' 'no milestone is no milestone parameter at all' 'absent'
else
  fail=$((fail + 1)); printf '  FAIL %-58s %s\n' 'no milestone is no milestone parameter at all' "$(grep 'issues?' "$SEL/calls" | tr '\n' ';')"
fi

# Every pull request is an issue to the issues endpoint, so a number naming one
# answers there too — and it is refused, not worked as a story.
checkr_fail '--issue refuses a pull request' 'is a pull request, not an issue' \
  story null - "$MILESTONES_ALL" --issue 410

printf '\npaging the issue list\n'

# The list is read a page at a time up to select.limit. A page holds at most
# 100, and gh's own default was 30, so a backlog past either would lose its tail
# silently to a read that stopped at one page.
#
# The first page here is nothing but pull requests carrying the label — every
# pull request is an issue to this endpoint — and the only story is on page two.
# Read as one page, or with the pull requests counted as candidates, this is
# BACKLOG EMPTY or a pull request selected; read right, it is #500.
: >"$SEL/lists/paged#"
for n in $(seq 1 100); do printf '%s\ta pull request\tpr\n' "$n" >>"$SEL/lists/paged#"; done
printf '500\tthe one story\n' >>"$SEL/lists/paged#"
checkr 'a story on the second page is found' 500 paged null - "$MILESTONES_ALL"
checkr_calls=$(grep -c 'issues?' "$SEL/calls")
if [ "$checkr_calls" = 2 ] && grep -q '&page=2$' "$SEL/calls"; then
  pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' 'and the read stopped at the short page' '2 pages'
else
  fail=$((fail + 1)); printf '  FAIL %-58s %s\n' 'and the read stopped at the short page' "$(grep 'issues?' "$SEL/calls" | tr '\n' ';')"
fi

# A pull request with a lower number than every story is still not one.
printf '3\ta pull request\tpr\n4\tthe story\n' >"$SEL/lists/mixed#"
checkr 'a pull request on the list is never a candidate' 4 mixed null - "$MILESTONES_ALL"

# The limit bounds what is read, oldest first, and the walk then goes in
# ascending number order. 701 was created last here, so with select.limit 2 it is
# not read at all: the answer is 705, the lower of the two that were. Unbounded,
# it is 701.
printf '705\tcreated first\n706\tcreated second\n701\tcreated third\n' >"$SEL/lists/limited#"
SEL_LIMIT=2 checkr 'select.limit bounds the list, oldest first' 705 limited null - "$MILESTONES_ALL"
checkr 'and without the bound the lowest number wins' 701 limited null - "$MILESTONES_ALL"

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

printf '\nthe reviewer App variables\n'

# `review.app.idEnv`/`keyEnv` name the environment variables the `local` rung's
# App credentials are read from — NAMES, never values, which is what makes them
# safe in a tracked file. They are validated in the same place as the roster and
# for the same reason: an issue must not be branched against a credential that
# cannot be read.
#
# What a wrong name costs is the whole reason this validation is loud. A GitHub
# App is installed per account, so naming the other account's pair mints a JWT
# for an App that is not installed here; the installation lookup says so;
# app-token.sh reports exit 3, which is UNAVAILABLE, which the roster reads as a
# rung that is DOWN. On `["copilot", "local", "none"]` with Copilot's allowance
# exhausted, that is a merge with no review at all — a misconfiguration wearing
# an outage's clothes.
#
# `roster_ok` and `roster_fail` run the whole script against the review block
# they are given, so a block that passes here still has to select an issue.

# app_note <name> <substring of stderr> <review json>
app_note() {
  roster_run "$3"
  case "$RUN_ERR" in
    *"$2"*) pass=$((pass + 1)); printf '  ok   %-58s [noted]\n' "$1" ;;
    *) fail=$((fail + 1)); printf '  FAIL %-58s diagnostics lack [%s]: %s\n' "$1" "$2" "$RUN_ERR" ;;
  esac
}

# Absent is the ordinary case and stays entirely legal: it means both defaults,
# which is what every repository had before this key existed.
roster_ok 'no app block at all is the default pair' \
  '{ "reviewers": ["copilot", "local"] }'

roster_ok 'both names, which is what a second account costs' \
  '{ "reviewers": ["copilot", "local"], "app": { "idEnv": "Z5LABS_REVIEW_APP_ID", "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

# The run says which pair it will read. A run whose reviewer rung later reports
# a variable is unset should not be the first time those names appear.
app_note 'the configured names are named in the diagnostics' 'Z5LABS_REVIEW_APP_ID' \
  '{ "reviewers": ["copilot", "local"], "app": { "idEnv": "Z5LABS_REVIEW_APP_ID", "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

# Half a pair. A configured idEnv beside a defaulted keyEnv reads one App's ID
# against another App's private key — half a credential one step earlier — and
# GitHub rejects the resulting JWT with nothing in the message pointing at the
# mismatch.
roster_fail 'idEnv without keyEnv is half a credential' 'review.app has no keyEnv' \
  '{ "reviewers": ["local"], "app": { "idEnv": "Z5LABS_REVIEW_APP_ID" } }'

roster_fail 'keyEnv without idEnv is the same mistake' 'review.app has no idEnv' \
  '{ "reviewers": ["local"], "app": { "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

roster_fail 'a null half is refused as an absent one' 'review.app has no keyEnv' \
  '{ "reviewers": ["local"], "app": { "idEnv": "Z5LABS_REVIEW_APP_ID", "keyEnv": null } }'

# A block holding neither is not half a pair — pointing at the other key would
# answer a question nobody asked. It says nothing, and the fix is to remove it.
roster_fail 'an empty block says to drop it' 'holds neither idEnv nor keyEnv' \
  '{ "reviewers": ["local"], "app": {} }'

roster_fail 'an empty name reads nothing at all' 'review.app.idEnv is empty' \
  '{ "reviewers": ["local"], "app": { "idEnv": "", "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

# A name that cannot BE a variable name. `${!name}` on one is a bash "bad
# substitution" inside app-token.sh, which would surface as a crash in a
# pipeline rather than as the sentence naming the offending value — and the
# offending value is what the message has to carry, because a login mapped to a
# variable name by hand is exactly where the dashes come from.
roster_fail 'a name holding dashes names the offending value' "names 'Z5LABS-REVIEW-APP-ID'" \
  '{ "reviewers": ["local"], "app": { "idEnv": "Z5LABS-REVIEW-APP-ID", "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

roster_fail 'a name opening with a digit is refused' 'not a legal environment variable name' \
  '{ "reviewers": ["local"], "app": { "idEnv": "1ST_REVIEW_APP_ID", "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

roster_fail 'a name holding a dollar sign is refused' 'not a legal environment variable name' \
  '{ "reviewers": ["local"], "app": { "idEnv": "REVIEW_APP_ID$", "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

# The value confusion, which is the one worth failing loudly on: this key holds
# a NAME, and a number here is somebody having written the App ID itself into a
# tracked file.
roster_fail 'a number is a credential, not a name' 'must be a string naming an environment variable' \
  '{ "reviewers": ["local"], "app": { "idEnv": 12345, "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

roster_fail 'app that is not an object' 'review.app must be an object holding idEnv and keyEnv' \
  '{ "reviewers": ["local"], "app": "Z5LABS_REVIEW_APP_ID" }'

# A misspelling is the failure this catches: `id_env` alongside a real `keyEnv`
# would otherwise be a key that does nothing while reading, to whoever wrote it,
# as a key that works.
roster_fail 'an unknown key is refused, not ignored' "review.app names an unknown key 'id_env'" \
  '{ "reviewers": ["local"], "app": { "id_env": "Z5LABS_REVIEW_APP_ID", "keyEnv": "Z5LABS_REVIEW_APP_KEY" } }'

printf '\ncross-repository dependencies, native\n'

# The dependency walk itself, end to end, under `dependencies.style` = `native`
# — the one part of selection the `--native-deps` seam above cannot reach,
# because the seam stops at printing edges and the bugs this section covers are
# in what the walk does with them afterwards.
#
# A cross-repository `owner/repo#N` used to be recorded as an unmet blocker
# without its state ever being read, so it never decayed when the dependency
# landed: the issue holding it was unreachable for the rest of the repository's
# life. It is silent in the ordinary case — that issue is skipped and the
# backlog keeps moving — and only surfaces as BLOCKED once it is the last
# candidate, at which point the report blames the dependency rather than the
# resolver.
#
# Under `native` each edge arrives with its blocker's state, from the same
# `blocked_by` response that named it, so nothing here reads an issue to resolve
# one — and a case below asserts exactly that.
NAT="$SCRATCH/native"
fixture_root "$NAT"
GQL=""

cat >"$NAT/repo/.claude/backlog.json" <<'JSON'
{
  "select": { "label": "story", "milestone": null, "limit": 200 },
  "dependencies": { "style": "native" },
  "verify": ["true"],
  "merge": { "label": "auto-merge", "workflow": "auto-merge.yaml" },
  "review": { "reviewers": ["copilot"] },
  "worktreeDir": ".claude/worktrees"
}
JSON

# xreset <fixture root> — every dependency, state and body fixture dropped
xreset() { rm -rf "$1/deps" "$1/states" "$1/bodies"; mkdir -p "$1/deps" "$1/states" "$1/bodies"; }
ndeps()  { printf '%s\n' "$2" >"$NAT/deps/$1"; }                 # <issue> <blocked_by json>
xstate() { printf '%s\n' "$3" >"$1/states/${2//\//_}"; }        # <root> <owner/repo#n> <state>
xbody()  { printf '%s\n' "$3" >"$1/bodies/${2//\//_}"; }        # <root> <owner/repo#n> <body>

# xrun <fixture root> <issue numbers, space separated> [args...]
xrun() {
  local fx=$1 n
  : >"$fx/lists/story#"
  for n in $2; do printf '%s\tissue %s\n' "$n" "$n" >>"$fx/lists/story#"; done
  shift 2
  run_in "$fx" "$@"
}

# checkd <root> <name> <expected issue number> <reason substring, or - for none> <issue numbers> [args...]
checkd() {
  local fx=$1 name=$2 want=$3 reason=$4 got
  shift 4
  xrun "$fx" "$@"
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

# checkd_blocked <root> <name> <substring of the report> <issue numbers> [args...]
checkd_blocked() {
  local fx=$1 name=$2 want=$3 report
  shift 3
  xrun "$fx" "$@"
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

# reads_none <name> — no issue was read to resolve a dependency
reads_none() {
  if [ -s "$NAT/reads" ]; then
    fail=$((fail + 1)); printf '  FAIL %-58s read %s\n' "$1" "$(tr '\n' ' ' <"$NAT/reads")"
  else
    pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' "$1" 'no issue read'
  fi
}

# The reported reproduction, transposed onto this fixture's repository names:
# every dependency of both candidates is closed, two of them elsewhere, so the
# lowest-numbered candidate is eligible. This used to report BLOCKED on a
# backlog that was fully workable.
xreset "$NAT"
ndeps 63 "[$(dep 328 z5labs/other closed),$(dep 32 z5labs/devex closed)]"
ndeps 65 "[$(dep 330 z5labs/other closed),$(dep 64 z5labs/devex closed)]"
checkd "$NAT" 'a closed cross-repository dependency no longer blocks' 63 - '63 65'
reads_none 'a native edge is resolved by the read that found it'

# The other half of the same read: an edge elsewhere that is genuinely open is
# still a blocker, and the report says what it is rather than that it could not
# be looked at.
xreset "$NAT"
ndeps 63 "[$(dep 328 z5labs/other open)]"
ndeps 65 '[]'
checkd "$NAT" 'an open cross-repository dependency still blocks' 65 'z5labs/other#328 is OPEN' '63 65'

checkd_blocked "$NAT" 'an open edge is named in the BLOCKED report' \
  'z5labs/other#328 is OPEN' '63'

# An issue number is only unique within its repository, so the state cache has
# to be keyed on `owner/repo#N`. Keyed on the bare number, this repository's
# closed #12 would answer for the other repository's open one and #63 would come
# out eligible with a live blocker.
xreset "$NAT"
ndeps 63 "[$(dep 12 z5labs/devex closed),$(dep 12 z5labs/other open)]"
ndeps 65 '[]'
checkd "$NAT" 'a same-repo state does not answer for elsewhere' 65 'z5labs/other#12 is OPEN' '63 65'

# The same collision in the other order, which a cache keyed on the number would
# also get wrong — and this direction is the one that reads as eligible.
xreset "$NAT"
ndeps 63 "[$(dep 12 z5labs/other closed),$(dep 12 z5labs/devex open)]"
ndeps 65 '[]'
checkd "$NAT" 'a cross-repo state does not answer for here' 65 '#12 is OPEN' '63 65'

# The blocked-by read failing outright is not "no blockers". That issue is
# ineligible with the error recorded, and the walk goes on to the next one.
xreset "$NAT"
ndeps 65 '[]'
checkd "$NAT" 'an unreadable blocked-by is ineligible, not unblocked' 65 'its native dependencies could not be read' '63 65'

# `--issue <n>` changes which issues are considered and nothing about whether the
# one considered is workable — the dependency walk runs against it exactly as it
# would have if the search had turned it up. Naming an issue is a statement about
# order, not a licence to start something whose blockers are open.
xreset "$NAT"
ndeps 63 "[$(dep 328 z5labs/other open)]"
xstate "$NAT" 'z5labs/devex#63' OPEN
checkd_blocked "$NAT" '--issue still walks the dependencies' \
  'z5labs/other#328 is OPEN' '63 65' --issue 63

# And the report blames the issue that was named rather than describing a
# backlog search that never happened.
checkd_blocked "$NAT" '--issue names itself in the BLOCKED report' \
  'issue #63, named with --issue' '63 65' --issue 63

xreset "$NAT"
ndeps 63 "[$(dep 328 z5labs/other closed)]"
xstate "$NAT" 'z5labs/devex#63' OPEN
checkd "$NAT" '--issue selects when its dependencies are all CLOSED' 63 - '63 65' --issue 63

# Out of the listed backlog entirely — the case `--issue` exists for. #77 is not
# in the issue list at all, so a run that quietly fell back to searching would
# answer #63.
xreset "$NAT"
ndeps 77 '[]'
xstate "$NAT" 'z5labs/devex#77' OPEN
checkd "$NAT" '--issue reaches an issue the list never returned' 77 - '63 65' --issue 77

printf '\ncross-repository dependencies, from the body\n'

# The same walk under a body style, where a reference is only a number and its
# state is read with `GET /repos/{owner}/{repo}/issues/{n}` against the
# repository the reference names. That read is the one that can fail: a private
# repository, a token without the scope, a deleted issue — or, in a Claude Code
# cloud session, any repository not attached to the session. Each is UNREADABLE,
# and UNREADABLE stays a blocker, because calling a blocked issue eligible is the
# failure this whole step exists to avoid.
BOD="$SCRATCH/body"
fixture_root "$BOD"

cat >"$BOD/repo/.claude/backlog.json" <<'JSON'
{
  "select": { "label": "story", "milestone": null, "limit": 200 },
  "dependencies": { "style": "blocked-by" },
  "verify": ["true"],
  "merge": { "label": "auto-merge", "workflow": "auto-merge.yaml" },
  "review": { "reviewers": ["copilot"] },
  "worktreeDir": ".claude/worktrees"
}
JSON

# bissue <number> <body> — an open candidate whose body is <body>
bissue() { xstate "$BOD" "z5labs/devex#$1" OPEN; xbody "$BOD" "z5labs/devex#$1" "$2"; }

xreset "$BOD"
bissue 63 $'Blocked by:\n- z5labs/other#328\n- #32'
bissue 65 ''
xstate "$BOD" 'z5labs/other#328' CLOSED
xstate "$BOD" 'z5labs/devex#32' CLOSED
checkd "$BOD" 'closed dependencies here and elsewhere, read from the body' 63 - '63 65'

# Fail-closed, preserved. The state cannot be read, so the edge stays a blocker.
xreset "$BOD"
bissue 63 $'Blocked by:\n- z5labs/other#328'
bissue 65 ''
checkd "$BOD" 'an unreadable cross-repository dependency still blocks' 65 'z5labs/other#328 is UNREADABLE' '63 65'

checkd_blocked "$BOD" 'an unreadable edge is named in the BLOCKED report' \
  'z5labs/other#328 is UNREADABLE' '63'

# The body itself. Read as empty on a failed read, it would declare nothing and
# the issue would come out eligible on a read that never happened.
xreset "$BOD"
bissue 65 ''
checkd "$BOD" 'an issue whose body cannot be read is not eligible' 65 '#63 not eligible: its body could not be read' '63 65'

# The cache is why a state is read once per dependency rather than once per edge
# pointing at it. Two candidates blocked by the same issue elsewhere: one read.
xreset "$BOD"
bissue 63 $'Blocked by:\n- z5labs/other#328'
bissue 65 $'Blocked by:\n- z5labs/other#328'
xstate "$BOD" 'z5labs/other#328' OPEN
checkd_blocked "$BOD" 'both candidates blocked by one edge elsewhere' \
  'z5labs/other#328 is OPEN' '63 65'
READS=$(grep -c '^z5labs/other#328$' "$BOD/reads")
if [ "$READS" -eq 1 ]; then
  pass=$((pass + 1))
  printf '  ok   %-58s [1 read]\n' 'a cross-repository state is read once and cached'
else
  fail=$((fail + 1))
  printf '  FAIL %-58s want 1 read, got %d: %s\n' \
    'a cross-repository state is read once and cached' "$READS" "$(tr '\n' ' ' <"$BOD/reads")"
fi

# The cache key carries the repository, in this direction too.
xreset "$BOD"
bissue 63 $'Blocked by:\n- #12\n- z5labs/other#12'
bissue 65 ''
xstate "$BOD" 'z5labs/devex#12' CLOSED
xstate "$BOD" 'z5labs/other#12' OPEN
checkd "$BOD" 'a cached state here does not answer for elsewhere' 65 'z5labs/other#12 is OPEN' '63 65'

printf '\nGraphQL\n'

# The guard every stub in this file sources logged each GraphQL-backed call it
# refused, outside the cases that expected one. A script that drifted back onto
# `gh issue`, `gh repo view` or `gh api graphql` would answer most cases here the
# way they want — a refused read is "unreadable", and several cases expect that —
# so this is the case that fails for it.
if [ -s "$GH_GUARD_LOG" ]; then
  fail=$((fail + 1))
  printf '  FAIL %-58s %s\n' 'no GraphQL-backed gh call was made' "$(tr '\n' ';' <"$GH_GUARD_LOG")"
else
  pass=$((pass + 1))
  printf '  ok   %-58s [%s]\n' 'no GraphQL-backed gh call was made' 'none'
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
