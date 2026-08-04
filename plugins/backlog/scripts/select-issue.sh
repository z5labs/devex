#!/usr/bin/env bash
#
# select-issue.sh — pick the next eligible backlog issue, as one call.
#
# Usage: select-issue.sh [--project-value <value> | --no-project-filter]
#        select-issue.sh --extract <blocked-by|depends-on|none> [file]
#        select-issue.sh --project-items <repo> <field> <value> [file]
#
# Why this is a script and not a numbered step.
#
# Selection is the one part of the cycle with no judgment in it at all: given
# the label, the milestone, the limit and the dependency convention, the answer
# is a function of the backlog. It was previously three awk/sed/grep pipelines
# written out in prose for an agent to retype, checked against a table of
# expected answers by eye. Every one of them was wrong in a way the table did
# not exercise:
#
#   * `tolower($0) ~ /blocked by/` was unanchored and did not require the colon
#     the convention is written with, so "this issue is not blocked by
#     anything", "the old parser was blocked by a flaky test", and the phrase
#     appearing inside a fenced code block all opened a dependency list, and the
#     next bullet in the body became a dependency that does not exist.
#   * A cross-repository `owner/repo#N` did the opposite of what was documented.
#     Under `depends-on` it matched nothing and the issue came out eligible.
#     Under `blocked-by` it was not a list item, so it *terminated* the list —
#     losing the real in-repo dependencies below it as well.
#   * The inline form `Blocked by: #12` and GitHub's task-list form `- [ ] #12`
#     both extracted nothing, which reads as an unblocked issue.
#
# Three of those five produce a *silently eligible* issue, which is the one
# wrong answer that gets work done in the wrong order rather than not at all.
#
# `--extract` exposes the extraction alone, reading a body from a file or stdin
# and printing one reference per line. It needs no network, which is what lets
# `select-issue_test.sh` hold the fixture corpus these bugs came from.
#
# `--project-items` is the same seam for the project scope below: it reads the
# GraphQL pages from a file or stdin and prints the in-scope issue numbers, so
# every rule about what counts as in scope is exercised by the test without a
# network call.
#
# Exit codes:
#   0   an issue was selected; stdout is {"number":N,"title":"..."}
#   4   usage or precondition failure (no gh/jq/git, the config is unusable, or
#       a requested project scope could not be resolved)
#   10  BACKLOG EMPTY — no open issue carries the label (and milestone, and
#       project field value)
#   11  BLOCKED — open issues remain and none is eligible

set -uo pipefail

fail() { local code=$1; shift; printf 'select-issue: %s\n' "$*" >&2; exit "$code"; }
note() { printf 'select-issue: %s\n' "$*" >&2; }

# ---------------------------------------------------------------- extract -----
# One reference per line, in first-seen order, deduped, in the form it was
# written: `#14` for this repository, `owner/repo#N` for anywhere else. Keeping
# the two shapes distinct is what lets the caller tell "issue 14 here" from
# "something I cannot resolve" instead of the two looking identical.
#
# Two deliberate asymmetries, both chosen so that an ambiguous body errs toward
# reporting a dependency rather than missing one:
#
#   * In a list, the FIRST reference on an item is the dependency and the rest
#     of the line is a gloss — `- #12 — needs #34 first` is a dependency on 12.
#   * On the inline label line, EVERY reference counts, because `Blocked by:
#     #12, #14` has no gloss convention and taking only the first would silently
#     drop 14.
#
# A blank line does not end a list. Markdown calls a bullet list with blank
# lines between its items a single loose list, and ending at the first blank
# would silently drop every dependency after it.
EXTRACT_AWK=$(cat <<'AWK'
function out(ref) {
  if (!(ref in seen)) { seen[ref] = 1; order[++n] = ref }
}

# The label line, with markdown decoration removed, lowercased. Used only to
# recognise the phrase — never to read a reference out of, because stripping
# `_` would corrupt a repository name that contains one.
function label_form(s,   t) {
  t = s
  sub(/^[[:space:]]*#+[[:space:]]*/, "", t)
  gsub(/[*_`]/, "", t)
  sub(/^[[:space:]]+/, "", t)
  return tolower(t)
}

function first_ref(s) {
  if (match(s, /([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)?#[0-9]+/))
    return substr(s, RSTART, RLENGTH)
  return ""
}

function all_refs(s,   rest, m) {
  rest = s
  while (match(rest, /([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)?#[0-9]+/)) {
    m = substr(rest, RSTART, RLENGTH)
    out(m)
    rest = substr(rest, RSTART + RLENGTH)
  }
}

# Anchored at the start of the remainder: the reference has to follow the
# phrase immediately, so "Depends on the parser landing, see #99" is not a
# dependency on 99.
function leading_ref(s) {
  if (match(s, /^([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)?#[0-9]+/))
    return substr(s, RSTART, RLENGTH)
  return ""
}

# "no longer depends on #17" is a note that a dependency was removed. Reading
# it as live blocks the issue forever with a reason that is not in its body.
function negated(p) {
  return p ~ /(^|[^a-z])(not|never|no longer|nor)[[:space:]]*$/ || p ~ /n.t[[:space:]]*$/
}

function is_item(s) {
  return s ~ /^[[:space:]]*[-*+][[:space:]]/ || s ~ /^[[:space:]]*[0-9]+[.)][[:space:]]/
}

function item_body(s,   t) {
  t = s
  sub(/^[[:space:]]*([-*+]|[0-9]+[.)])[[:space:]]*/, "", t)
  sub(/^\[[ xX]\][[:space:]]*/, "", t)   # GitHub task-list checkbox
  return t
}

function depends_on(line,   lower, pos, seg, start, len, prefix, tail, ref) {
  lower = tolower(line)
  pos = 1
  while (pos <= length(lower)) {
    seg = substr(lower, pos)
    if (!match(seg, /depends[[:space:]]+on[[:space:]]*:?[[:space:]]*/)) return
    start = pos + RSTART - 1
    len   = RLENGTH
    prefix = substr(lower, 1, start - 1)
    tail   = substr(line, start + len)      # original case: repo names matter
    if (!negated(prefix)) {
      ref = leading_ref(tail)
      if (ref != "") out(ref)
    }
    pos = start + len
  }
}

# A fenced block is example text. The convention this parses is written inside
# fences in more than one issue template, and a template is not a declaration.
{
  probe = $0
  sub(/^[[:space:]]*/, "", probe)
  if (probe ~ /^(```|~~~)/) { fence = !fence; next }
  if (fence) next
}

STYLE == "depends-on" { depends_on($0); next }

STYLE == "blocked-by" {
  lf = label_form($0)
  # The phrase has to open the line and be terminated by a colon or by the end
  # of the line. That is what tells "Blocked by:" apart from "was blocked by a
  # flaky test", and it is the form the convention is actually written in.
  if (lf ~ /^blocked[[:space:]]+by[[:space:]]*(:|$)/) {
    inlist = 1
    rest = $0
    if (rest ~ /:/) { sub(/^[^:]*:/, "", rest); all_refs(rest) }
    next
  }
  if (inlist) {
    if (is_item($0)) { r = first_ref(item_body($0)); if (r != "") out(r); next }
    if ($0 ~ /[^[:space:]]/) inlist = 0   # non-blank, not an item: list over
  }
  next
}

END { for (i = 1; i <= n; i++) print order[i] }
AWK
)

extract_refs() { # <style> ; body on stdin
  [ "$1" = none ] && { cat >/dev/null; return 0; }
  awk -v STYLE="$1" "$EXTRACT_AWK"
}

if [ "${1:-}" = --extract ]; then
  [ $# -ge 2 ] || fail 4 "usage: select-issue.sh --extract <blocked-by|depends-on|none> [file]"
  case "$2" in
    blocked-by|depends-on|none) ;;
    *) fail 4 "unknown style '$2'; expected blocked-by, depends-on or none" ;;
  esac
  if [ -n "${3:-}" ]; then extract_refs "$2" <"$3"; else extract_refs "$2"; fi
  exit 0
fi

# ---------------------------------------------------------------- project -----
# Optional scoping by one value of a GitHub Projects v2 single-select field —
# "just the workspace-ci stories today". Label and milestone are the only other
# axes the backlog can be narrowed on, and neither models the grouping a project
# board already holds.
#
# Read as a *field value*, over GraphQL, rather than any of the alternatives:
#
#   * `gh project item-list --format json` flattens custom fields into
#     lowercased top-level keys. That shape is undocumented, it collides as soon
#     as two fields differ only by case or punctuation, and a rename upstream
#     changes it silently. `ProjectV2ItemFieldSingleSelectValue` is the schema
#     type and says what it is.
#   * A convention in the issue body would need a template change and a backfill
#     of every open issue before it filtered anything, and it inherits every
#     parsing failure mode documented above this line. A single-select cannot
#     hold a typo'd value, and the project enforces that however the issue was
#     filed — including `gh issue create --body`, which bypasses templates.
#
# Two things this deliberately does not trust:
#
#   * An org-level project spans repositories, so its items are intersected with
#     this repository's issues rather than taken as-is. Without that, `#42` in a
#     sibling repository selects issue 42 here.
#   * The requested value is checked against the field's declared options before
#     anything is filtered. A typo would otherwise match no item and read as an
#     empty backlog — and an empty backlog stops the loop quietly, which is the
#     failure that looks like success.
#
# Every failure here is exit 4, never a silent fall-through to the unscoped
# backlog. Selecting from the whole backlog when a scope was asked for is the
# one wrong answer that gets work done in the wrong order rather than not at
# all, which is the same standard the dependency walk below is held to.

# The field is found in `fields` rather than asked for by `field(name: $field)`.
# Measured: an unknown name makes `field(name:)` fail the *whole query* — `Could
# not resolve to a Unions::ProjectV2FieldConfiguration with the name Module` —
# so the response that would have carried the list of real field names is the
# one thing a typo cannot get you. `fieldValueByName` is the opposite and
# returns null for a name that does not exist, which is why the item side can
# keep using it.
project_query() { # <organization|user>
  cat <<QUERY
query(\$owner: String!, \$number: Int!, \$field: String!, \$endCursor: String) {
  $1(login: \$owner) {
    projectV2(number: \$number) {
      title
      fields(first: 100) {
        nodes {
          __typename
          ... on ProjectV2FieldCommon { name }
          ... on ProjectV2SingleSelectField { options { name } }
        }
      }
      items(first: 100, after: \$endCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          content {
            __typename
            ... on Issue { number repository { nameWithOwner } }
          }
          fieldValueByName(name: \$field) {
            __typename
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
        }
      }
    }
  }
}
QUERY
}

# `gh api graphql --paginate` prints one whole response per page, concatenated,
# which `jq -s` reads as a stream of documents. The owner may be an organisation
# or a user and the query root differs, so `proj` accepts either and every page
# that resolved to neither drops out.
PROJECT_JQ_DEF='
def proj: (.data.organization // .data.user) | .projectV2?;
def pages: [ .[] | proj ] | map(select(. != null));
def thefield: ((pages[0].fields.nodes // []) | map(select(.name == $field)) | .[0]) // null;
'

# Prints one in-scope issue number per line. Reads the pages on stdin so the
# test can hold them as fixtures; the network lives in project_fetch alone.
project_items() { # <repo> <field> <value>
  local repo=$1 field=$2 want=$3
  local pages typename known options options_msg

  pages=$(cat)
  [ -n "$pages" ] || fail 4 "the project query returned no response for field '$field'"

  typename=$(printf '%s' "$pages" | jq -s -r --arg field "$field" "$PROJECT_JQ_DEF"'
    if (pages | length) == 0 then "NO-PROJECT" else (thefield.__typename // "NO-FIELD") end') \
    || fail 4 "the project query response does not parse as JSON"

  case "$typename" in
    NO-PROJECT)
      fail 4 "no project was returned; check select.project.owner and select.project.number" ;;
    NO-FIELD)
      known=$(printf '%s' "$pages" | jq -s -r --arg field "$field" "$PROJECT_JQ_DEF"'
        (pages[0].fields.nodes // []) | map(.name // empty) | join(", ")')
      fail 4 "the project has no field named '$field'${known:+; its fields are: $known}" ;;
    ProjectV2SingleSelectField) ;;
    *)
      fail 4 "project field '$field' is a $typename, not a single-select field; only a single-select field can scope the backlog" ;;
  esac

  options=$(printf '%s' "$pages" | jq -s -r --arg field "$field" "$PROJECT_JQ_DEF"'thefield.options[]?.name')
  # Exact match, and the options list is what says so. Matching loosely would
  # make `Workspace-CI` and `workspace-ci` the same request against a field that
  # considers them two different options, or none.
  if ! printf '%s\n' "$options" | grep -qxF -- "$want"; then
    options_msg=$(printf '%s' "$options" | tr '\n' ',')
    fail 4 "project field '$field' has no option '$want'; its options are: ${options_msg%,}"
  fi

  printf '%s' "$pages" | jq -s -r --arg field "$field" --arg repo "$repo" --arg want "$want" "$PROJECT_JQ_DEF"'
    [ pages[].items.nodes[]? ]
    | map(select(
        .content.__typename == "Issue"
        and .content.repository.nameWithOwner == $repo
        and .fieldValueByName.__typename == "ProjectV2ItemFieldSingleSelectValue"
        and .fieldValueByName.name == $want))
    | map(.content.number) | unique | .[]' \
    || fail 4 "the project query response could not be read for field '$field'"
}

if [ "${1:-}" = --project-items ]; then
  command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"
  [ $# -ge 4 ] || fail 4 "usage: select-issue.sh --project-items <repo> <field> <value> [file]"
  if [ -n "${5:-}" ]; then project_items "$2" "$3" "$4" <"$5"; else project_items "$2" "$3" "$4"; fi
  exit 0
fi

# The organisation root is tried first and the user root only after GitHub says
# the login is not an organisation, because a project number is scoped to its
# owner: guessing the wrong root would report "no such project" for a project
# that exists.
#
# gh's stderr goes to a file rather than into the captured output, which would
# corrupt the JSON. The caller owns that file: this runs inside a command
# substitution, whose subshell does not run the EXIT trap and cannot hand a path
# back to be cleaned up.
PROJECT_ERRFILE=""
trap '[ -n "$PROJECT_ERRFILE" ] && rm -f "$PROJECT_ERRFILE"; true' EXIT

project_fetch() { # <owner> <number> <field> ; needs PROJECT_ERRFILE
  local owner=$1 number=$2 field=$3
  local root out rc message

  for root in organization user; do
    out=$(gh api graphql --paginate \
            -F owner="$owner" -F number="$number" -F field="$field" \
            -f query="$(project_query "$root")" 2>"$PROJECT_ERRFILE")
    rc=$?
    if [ "$rc" -eq 0 ]; then printf '%s' "$out"; return 0; fi
    if grep -qiE 'read:project|not been granted|INSUFFICIENT_SCOPES' "$PROJECT_ERRFILE"; then
      note "$(cat "$PROJECT_ERRFILE")"
      fail 4 "the GitHub token cannot read projects; run 'gh auth refresh -s read:project' and retry"
    fi
    grep -qiE 'could not resolve to an organization' "$PROJECT_ERRFILE" && continue
    break
  done

  message=$(tr '\n' ' ' <"$PROJECT_ERRFILE")
  fail 4 "cannot read project number $number owned by '$owner': ${message:-gh api graphql failed}"
}

# ------------------------------------------------------------------- args -----
# The scope belongs on the invocation as much as in the config: "just workspace-ci
# today" is a property of one run, and a config-only knob would mean editing
# .claude/backlog.json before and after every scoped run.
#
# Two flags rather than one with an empty-string sentinel. `--project-value ''`
# would have to mean "no scope", which is the same conflation `--milestone ''`
# is avoided for below.
OPT_PROJECT_VALUE=""
OPT_PROJECT_VALUE_SET=0
OPT_NO_PROJECT=0

while [ $# -gt 0 ]; do
  case "$1" in
    --project-value)
      [ $# -ge 2 ] || fail 4 "--project-value needs a value"
      OPT_PROJECT_VALUE=$2
      OPT_PROJECT_VALUE_SET=1
      shift 2 ;;
    --project-value=*)
      OPT_PROJECT_VALUE=${1#*=}
      OPT_PROJECT_VALUE_SET=1
      shift ;;
    --no-project-filter)
      OPT_NO_PROJECT=1
      shift ;;
    *)
      fail 4 "unknown argument '$1'; usage: select-issue.sh [--project-value <value> | --no-project-filter]" ;;
  esac
done

if [ "$OPT_PROJECT_VALUE_SET" -eq 1 ]; then
  [ -n "$OPT_PROJECT_VALUE" ] \
    || fail 4 "--project-value needs a non-empty value; to run unscoped, pass --no-project-filter or nothing at all"
  [ "$OPT_NO_PROJECT" -eq 0 ] \
    || fail 4 "--project-value and --no-project-filter cannot both be given"
fi

# ----------------------------------------------------------------- config -----
command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"
command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || fail 4 "not inside a git repository"
cd "$ROOT" || fail 4 "cannot enter the repository root at $ROOT"

CFG=.claude/backlog.json
[ -f "$CFG" ] || fail 4 "$CFG is missing; run backlog:setup-backlog to create it"
jq -e . "$CFG" >/dev/null 2>&1 || fail 4 "$CFG does not parse as JSON"

LABEL=$(jq -r '.select.label // ""' "$CFG")
MILESTONE=$(jq -r '.select.milestone // ""' "$CFG")
LIMIT=$(jq -r '.select.limit // ""' "$CFG")
PROJECT_KIND=$(jq -r 'if (.select.project // null) == null then "absent" else (.select.project | type) end' "$CFG")
STYLE=$(jq -r '.dependencies.style // ""' "$CFG")
VERIFY_N=$(jq -r 'if (.verify | type) == "array" then (.verify | length) else -1 end' "$CFG")

[ -n "$LABEL" ] || fail 4 "$CFG: select.label is missing or empty"
case "$LIMIT" in
  ''|*[!0-9]*) fail 4 "$CFG: select.limit must be a positive integer (found '${LIMIT:-null}')" ;;
  0)           fail 4 "$CFG: select.limit must be at least 1" ;;
esac
case "$STYLE" in
  blocked-by|depends-on|none) ;;
  *) fail 4 "$CFG: dependencies.style must be one of blocked-by, depends-on, none (found '${STYLE:-null}'); run backlog:setup-backlog" ;;
esac
# Checked here rather than at step 4, where an empty list is indistinguishable
# from a list that passed: a config with no verify commands opens a pull request
# nothing local ever looked at, and selection is the first chance to say so.
[ "$VERIFY_N" -ge 1 ] 2>/dev/null \
  || fail 4 "$CFG: verify must be a non-empty array of commands; run backlog:setup-backlog"

# The scope is whatever the config pins, unless this run says otherwise. Absent
# from both, no project call is made at all and selection is exactly what it was
# before this filter existed.
PROJECT_OWNER=""
PROJECT_NUMBER=""
PROJECT_FIELD=""
PROJECT_VALUE=""
case "$PROJECT_KIND" in
  absent) ;;
  object)
    PROJECT_OWNER=$(jq -r '.select.project.owner // ""' "$CFG")
    PROJECT_NUMBER=$(jq -r '.select.project.number // ""' "$CFG")
    PROJECT_FIELD=$(jq -r '.select.project.field // ""' "$CFG")
    PROJECT_VALUE=$(jq -r '.select.project.value // ""' "$CFG")
    ;;
  *) fail 4 "$CFG: select.project must be an object or null (found a $PROJECT_KIND)" ;;
esac

[ "$OPT_PROJECT_VALUE_SET" -eq 1 ] && PROJECT_VALUE=$OPT_PROJECT_VALUE
[ "$OPT_NO_PROJECT" -eq 1 ] && PROJECT_VALUE=""

# Validated up front rather than at the point of use: a scope that cannot be
# resolved has to stop selection, not degrade it to the unscoped backlog.
if [ -n "$PROJECT_VALUE" ]; then
  [ "$PROJECT_KIND" = object ] \
    || fail 4 "a project scope of '$PROJECT_VALUE' was requested but $CFG has no select.project; add owner, number and field to it"
  [ -n "$PROJECT_OWNER" ] || fail 4 "$CFG: select.project.owner is missing or empty"
  [ -n "$PROJECT_FIELD" ] || fail 4 "$CFG: select.project.field is missing or empty"
  case "$PROJECT_NUMBER" in
    ''|*[!0-9]*) fail 4 "$CFG: select.project.number must be a positive integer (found '${PROJECT_NUMBER:-null}')" ;;
    0)           fail 4 "$CFG: select.project.number must be at least 1" ;;
  esac
fi

REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) \
  || fail 4 "cannot resolve the repository slug from gh"
[ -n "$REPO" ] || fail 4 "cannot resolve the repository slug from gh"

# ------------------------------------------------------------------- list -----
# --limit is always passed. gh's default page size is 30, and on a longer
# backlog the issues past the first page do not error — they are simply absent,
# so the cycle reports an empty or blocked backlog that is neither.
#
# --milestone is passed only when one is configured. An empty --milestone is not
# the same as no milestone filter, which is why this is an argument array rather
# than a string the caller interpolates.
LIST_ARGS=(--repo "$REPO" --state open --label "$LABEL" --limit "$LIMIT")
[ -n "$MILESTONE" ] && LIST_ARGS+=(--milestone "$MILESTONE")

CANDIDATES=$(gh issue list "${LIST_ARGS[@]}" \
  --json number,title --jq 'sort_by(.number)[] | "\(.number)\t\(.title)"') \
  || fail 4 "gh issue list failed for $REPO"

SCOPE_NOTE=""
[ -n "$PROJECT_VALUE" ] && SCOPE_NOTE=" with $PROJECT_FIELD = '$PROJECT_VALUE' in project $PROJECT_OWNER/$PROJECT_NUMBER"

if [ -z "$CANDIDATES" ]; then
  printf 'BACKLOG EMPTY\n'
  note "no open issue in $REPO carries the label '$LABEL'${MILESTONE:+ in milestone '$MILESTONE'}"
  exit 10
fi

# ---------------------------------------------------------------- scoping -----
# Applied before the dependency walk, so the walk sees only in-scope issues and
# cannot be held up by a blocker outside the scope it was asked for.
#
# The limit above still bounds the *label* query, not this one: on a backlog
# longer than select.limit, an in-scope issue can fall off the end before the
# scope is ever applied. That is why the limit defaults to 200 rather than to
# gh's page size of 30.
if [ -n "$PROJECT_VALUE" ]; then
  # `|| exit $?` on both: a `fail` inside a command substitution exits only the
  # subshell, and without this the script would carry on with an empty scope —
  # which is the unscoped-selection failure this whole section exists to make
  # impossible.
  PROJECT_ERRFILE=$(mktemp) || fail 4 "cannot create a temporary file for the project query"
  PROJECT_PAGES=$(project_fetch "$PROJECT_OWNER" "$PROJECT_NUMBER" "$PROJECT_FIELD") || exit $?
  IN_SCOPE=$(project_items "$REPO" "$PROJECT_FIELD" "$PROJECT_VALUE" <<<"$PROJECT_PAGES") || exit $?

  CANDIDATES=$(awk -F'\t' 'NR == FNR { if ($0 != "") keep[$0] = 1; next } ($1 in keep)' \
    <(printf '%s\n' "$IN_SCOPE") <(printf '%s\n' "$CANDIDATES"))

  if [ -z "$CANDIDATES" ]; then
    printf 'BACKLOG EMPTY\n'
    note "no open issue in $REPO carries the label '$LABEL'${MILESTONE:+ in milestone '$MILESTONE'}$SCOPE_NOTE"
    exit 10
  fi
  note "project scope leaves $(printf '%s\n' "$CANDIDATES" | grep -c .) candidate(s)$SCOPE_NOTE"
fi

# --------------------------------------------------------------- eligible -----
# Walk in ascending number order and take the first issue whose every declared
# dependency is CLOSED. An issue with a dependency this script cannot resolve is
# skipped rather than treated as eligible — but it is skipped as *ineligible*,
# with its reason recorded, so the backlog is not starved by one unresolvable
# reference while other issues are workable.
STATE_CACHE=""
dep_state() { # <issue-number>
  local hit
  case "$STATE_CACHE" in
    *"|$1="*) hit=${STATE_CACHE##*"|$1="}; printf '%s' "${hit%%|*}"; return 0 ;;
  esac
  local s
  s=$(gh issue view "$1" --repo "$REPO" --json state --jq .state 2>/dev/null) || s=""
  [ -n "$s" ] || s=UNREADABLE
  STATE_CACHE="$STATE_CACHE|$1=$s|"
  printf '%s' "$s"
}

REASONS=""
while IFS=$'\t' read -r NUM TITLE; do
  [ -n "$NUM" ] || continue

  if [ "$STYLE" = none ]; then
    note "#$NUM eligible (dependencies.style is none)"
    jq -n --argjson n "$NUM" --arg t "$TITLE" '{number:$n, title:$t}'
    exit 0
  fi

  BODY=$(gh issue view "$NUM" --repo "$REPO" --json body --jq .body 2>/dev/null) || BODY=""
  REFS=$(printf '%s\n' "$BODY" | extract_refs "$STYLE")

  blockers=""
  for ref in $REFS; do
    case "$ref" in
      \#*)
        d=${ref#\#}
        st=$(dep_state "$d")
        [ "$st" = CLOSED ] || blockers="$blockers #$d is $st;"
        ;;
      *)
        # owner/repo#N. Not modelled, and guessing is worse than declining:
        # calling the issue eligible is the failure this whole step avoids.
        blockers="$blockers $ref is a cross-repository dependency this cycle cannot resolve;"
        ;;
    esac
  done

  if [ -z "$blockers" ]; then
    note "#$NUM eligible${REFS:+ (dependencies all CLOSED)}"
    jq -n --argjson n "$NUM" --arg t "$TITLE" '{number:$n, title:$t}'
    exit 0
  fi

  note "#$NUM not eligible:$blockers"
  REASONS="$REASONS#$NUM:$blockers
"
# Process substitution, not a pipe: a `while | read` loop runs in a subshell,
# where the `exit 0` above would end the subshell and let the script fall
# through to the BLOCKED report having just selected an issue. Not a heredoc
# either — an unquoted one would treat a backslash in an issue title as an
# escape.
done < <(printf '%s\n' "$CANDIDATES")

printf 'BLOCKED — every open issue carrying the label '\''%s'\''%s has an unmet dependency:\n' "$LABEL" "$SCOPE_NOTE"
printf '%s' "$REASONS"
exit 11
