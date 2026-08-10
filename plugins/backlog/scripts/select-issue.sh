#!/usr/bin/env bash
#
# select-issue.sh — pick the next eligible backlog issue, as one call.
#
# Usage: select-issue.sh [--label <name>] [--milestone <title>]
#                        [--project-owner <login>] [--project-number <n>]
#                        [--project-field <name>] [--project-value <value>]
#        select-issue.sh [--no-milestone-filter] [--no-project-filter]
#        select-issue.sh [--label <name>] --all
#        select-issue.sh --issue <n>
#        select-issue.sh --extract <blocked-by|depends-on|none> [file]
#        select-issue.sh --native-deps <repo> [file]
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
# `--native-deps` is the same seam for the `native` style, which takes the edges
# from GitHub's typed issue dependencies instead of from the body. It reads the
# GraphQL pages and prints references in exactly the form `--extract` does, so
# the eligibility walk below is one code path for every style.
#
# `--project-items` is the same seam for the project scope below: it reads the
# GraphQL pages from a file or stdin and prints the in-scope issue numbers, so
# every rule about what counts as in scope is exercised by the test without a
# network call.
#
# Every selector is a *runtime* decision. `.claude/backlog.json` supplies the
# default for each one and every one of them can be overridden for a single run:
# `--label`, `--milestone`/`--no-milestone-filter`, the four `--project-*` flags
# and `--no-project-filter`, `--all` to drop every optional narrowing at once,
# and `--issue <n>` to name one issue outright.
#
# Config is a default, never a floor. The failure that established this: avroc's
# config pinned `select.milestone: "v0.2.0"`, the milestone was later deleted,
# and `gh issue list --milestone v0.2.0` answered `[]` with exit 0 over a backlog
# holding an eligible unmilestoned story. That reported BACKLOG EMPTY — "success,
# not failure" by the loop's own table — and halted a workable backlog with
# nothing in the run pointing at the milestone. So a milestone is now checked
# against the repository's milestones before the issue query, exactly as a
# project value is checked against its field's options, and a run that wants a
# different milestone (or none) says so on the command line instead of editing a
# tracked file.
#
# Exit codes:
#   0   an issue was selected; stdout is {"number":N,"title":"..."}
#   4   usage or precondition failure (no gh/jq/git, the config is unusable, a
#       requested project scope could not be resolved, or a requested milestone
#       does not exist)
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
    # `native` is a configured style but not a body extraction: under it the
    # body is never read at all. Accepting it here would answer "no references"
    # for a body that declares none *and* for an issue whose typed edges say
    # otherwise, which is the conflation the style exists to remove.
    native) fail 4 "the native style does not parse the body; use --native-deps <repo> to read its typed dependencies" ;;
    *) fail 4 "unknown style '$2'; expected blocked-by, depends-on or none" ;;
  esac
  if [ -n "${3:-}" ]; then extract_refs "$2" <"$3"; else extract_refs "$2"; fi
  exit 0
fi

# ----------------------------------------------------------------- native -----
# GitHub's typed issue dependencies — `gh issue edit <n> --add-blocked-by`, the
# `addBlockedBy` GraphQL mutation, `POST .../dependencies/blocked_by`. A typed
# edge cannot be written ambiguously, it survives a rewording of the body, and
# it removes every failure mode the extraction above is scarred by.
#
# This is opt-in and is never reached by fallback. A repository that has not
# populated dependencies answers "no blockers" for every issue, which reads as
# an unblocked backlog and is not one — the same wrong answer, arrived at from
# the other direction. So `dependencies.style` has to *declare* `native`, and a
# body parse that finds nothing never escalates to it.
#
# Read over GraphQL rather than `GET /repos/{owner}/{repo}/issues/{n}/dependencies/blocked_by`
# for one reason: the REST route is per-repository, so a dependency on an issue
# elsewhere would come back described by a `repository_url` to re-derive, while
# `blockedBy` carries `repository { nameWithOwner }` on the node. A
# cross-repository edge has to be *named* before it can be resolved — the
# eligibility walk below reads its state against the repository the node names —
# so the shape that states it plainly wins.
NATIVE_QUERY=$(cat <<'QUERY'
query($owner: String!, $name: String!, $number: Int!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      number
      blockedBy(first: 100, after: $endCursor) {
        pageInfo { hasNextPage endCursor }
        nodes { number repository { nameWithOwner } }
      }
    }
  }
}
QUERY
)

# `--paginate` prints one whole response per page, concatenated, which `jq -s`
# reads as a stream of documents — the same shape the project query returns.
NATIVE_JQ_DEF='
def issues: [ .[] | .data?.repository?.issue? ] | map(select(. != null));
def nodes:  [ issues[].blockedBy.nodes[]? ];
'

# Prints one reference per line, first-seen order, deduped, in the same two
# shapes `--extract` prints: `#14` for this repository, `owner/repo#N` for
# anywhere else. Reads the pages on stdin so the test can hold them as
# fixtures; the network lives in native_fetch alone.
native_deps() { # <repo>
  local repo=$1
  local pages status

  pages=$(cat)
  [ -n "$pages" ] || fail 4 "the blocked-by query returned no response"

  status=$(printf '%s' "$pages" | jq -s -r "$NATIVE_JQ_DEF"'
    if (issues | length) == 0 then "NO-ISSUE"
    elif ([ issues[] | select(.blockedBy != null) ] | length) == 0 then "NO-FIELD"
    elif (nodes | map(select(.number == null or .repository.nameWithOwner == null)) | length) > 0 then "BAD-NODE"
    else "OK" end') \
    || fail 4 "the blocked-by query response does not parse as JSON"

  case "$status" in
    OK) ;;
    NO-ISSUE)
      fail 4 "the blocked-by query resolved to no issue; check the repository and issue number" ;;
    # An issue that exists but carries no `blockedBy` connection is a GitHub
    # that does not serve typed dependencies. Reading that as an empty edge set
    # would make every issue eligible, which is the one failure this style is
    # declared to avoid.
    NO-FIELD)
      fail 4 "the blocked-by query returned an issue with no blockedBy connection; this GitHub does not serve typed issue dependencies" ;;
    # Defensive, and deliberately fatal rather than a filter: a node that cannot
    # be named is a dependency dropped, and a dropped dependency is a silently
    # eligible issue.
    BAD-NODE)
      fail 4 "the blocked-by query returned a dependency with no number or repository" ;;
    *)
      fail 4 "the blocked-by query response could not be read" ;;
  esac

  printf '%s' "$pages" | jq -s -r --arg repo "$repo" "$NATIVE_JQ_DEF"'
    nodes
    | map(if .repository.nameWithOwner == $repo then "#\(.number)"
          else "\(.repository.nameWithOwner)#\(.number)" end)
    | reduce .[] as $r ([]; if index($r) then . else . + [$r] end)
    | .[]' \
    || fail 4 "the blocked-by query response could not be read"
}

if [ "${1:-}" = --native-deps ]; then
  command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"
  [ $# -ge 2 ] || fail 4 "usage: select-issue.sh --native-deps <repo> [file]"
  if [ -n "${3:-}" ]; then native_deps "$2" <"$3"; else native_deps "$2"; fi
  exit 0
fi

# The only network in this section, and the only thing the seam above does not
# cover. Every diagnostic goes to stderr and the exit status is all the caller
# reads, because a failure here has to make *that issue* ineligible rather than
# stop the walk — the same treatment a dependency whose state cannot be read
# settles on below. The one thing it must never do is succeed with nothing on
# stdout.
native_fetch() { # <repo> <issue-number>
  local owner=${1%%/*} name=${1##*/} pages

  pages=$(gh api graphql --paginate \
            -F owner="$owner" -F name="$name" -F number="$2" \
            -f query="$NATIVE_QUERY") || return 1
  printf '%s' "$pages" | native_deps "$1"
}

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
# corrupt the JSON. The caller owns that file: these run inside a command
# substitution, whose subshell does not run the EXIT trap and cannot hand a path
# back to be cleaned up. One file serves both network callers below, created on
# first use so an ordinary run makes no temporary file at all.
ERRFILE=""
trap '[ -n "$ERRFILE" ] && rm -f "$ERRFILE"; true' EXIT

need_errfile() {
  [ -n "$ERRFILE" ] && return 0
  ERRFILE=$(mktemp) || fail 4 "cannot create a temporary file for a GitHub query"
}

project_fetch() { # <owner> <number> <field> ; needs ERRFILE
  local owner=$1 number=$2 field=$3
  local root out rc message

  for root in organization user; do
    out=$(gh api graphql --paginate \
            -F owner="$owner" -F number="$number" -F field="$field" \
            -f query="$(project_query "$root")" 2>"$ERRFILE")
    rc=$?
    if [ "$rc" -eq 0 ]; then printf '%s' "$out"; return 0; fi
    if grep -qiE 'read:project|not been granted|INSUFFICIENT_SCOPES' "$ERRFILE"; then
      note "$(cat "$ERRFILE")"
      fail 4 "the GitHub token cannot read projects; run 'gh auth refresh -s read:project' and retry"
    fi
    grep -qiE 'could not resolve to an organization' "$ERRFILE" && continue
    break
  done

  message=$(tr '\n' ' ' <"$ERRFILE")
  fail 4 "cannot read project number $number owned by '$owner': ${message:-gh api graphql failed}"
}

# ------------------------------------------------------------------- args -----
# The scope belongs on the invocation as much as in the config: "just workspace-ci
# today" is a property of one run, and a config-only knob would mean editing
# .claude/backlog.json before and after every scoped run.
#
# Every piece of the scope gets a flag, for the same reason the value does. The
# *axis* is as much a property of one run as the value on it: a board routinely
# carries more than one single-select worth scoping by — devex's own project 14
# has both `Status` and `Module` — and "work the In Progress stories" is the same
# kind of request as "work the workspace-ci stories". Pinning the axis in the
# config would make only the second one expressible.
#
# `--project-owner` and `--project-number` are the weakest case: they describe
# the repository's board rather than a run, and the config stays their normal
# home. They exist so a scope can be assembled from flags alone on a repository
# whose config carries no `select.project` at all — without them, scoping such a
# repository at all costs an edit and a commit to a tracked file, which is the
# cost every flag here exists to avoid.
#
# Nothing new has to validate `--project-field`. `project_query` reads `fields`
# rather than `field(name: $field)`, so an unknown name already fails the run at
# exit 4 with the list of real field names; the flag inherits that check
# unchanged rather than growing a second one that could disagree with it.
#
# Separate flags rather than one with an empty-string sentinel. `--project-value ''`
# would have to mean "no scope", which is the same conflation `--milestone ''`
# is avoided for.
#
# The label and the milestone get the same treatment, and for the same reason.
# "Just the v0.3.0 stories today" is exactly as much a property of one run as
# "just the workspace-ci stories today", and until these flags existed only the
# second was expressible — while `run-backlog`'s own description advertised
# "drain the milestone" as a trigger phrase for a request the vocabulary could
# not accept.
#
# `--all` is the one input that means "no optional narrowing at all": it clears
# the milestone and the project scope together, so a caller does not have to know
# which axes a given repository happens to have configured. It deliberately does
# *not* clear the label, which is not an optional narrowing but the definition of
# what the backlog is — an issue without it was never backlog work.
#
# `--issue <n>` names one issue instead of searching for one. The dependency walk
# still runs against it, because "work this issue next" is a statement about
# order and not a licence to start something whose blockers are open; an issue
# with unmet dependencies is BLOCKED naming them, never a silent selection.
OPT_PROJECT_OWNER=""
OPT_PROJECT_OWNER_SET=0
OPT_PROJECT_NUMBER=""
OPT_PROJECT_NUMBER_SET=0
OPT_PROJECT_FIELD=""
OPT_PROJECT_FIELD_SET=0
OPT_PROJECT_VALUE=""
OPT_PROJECT_VALUE_SET=0
OPT_NO_PROJECT=0
OPT_LABEL=""
OPT_LABEL_SET=0
OPT_MILESTONE=""
OPT_MILESTONE_SET=0
OPT_NO_MILESTONE=0
OPT_ALL=0
OPT_ISSUE=""
OPT_ISSUE_SET=0
GIVEN=""       # the project flags this run named, for the messages below
NARROWING=""   # every flag that sets or clears an *optional* narrowing
LABELLED=""    # `--label`, which defines the backlog rather than narrowing it

# Three forms, because `--all` and `--issue` are not alternatives to the same
# things. `--all` drops the optional narrowings and keeps the label, so it
# composes with `--label` and with nothing else; `--issue` replaces selection
# outright and composes with nothing at all. One flat line with `| --all |
# --issue` at the end would say both are exclusive modes, which is wrong for the
# first and is the text every argument error prints.
USAGE='usage: select-issue.sh [--label <name>] [--milestone <title> | --no-milestone-filter] [--project-owner <login>] [--project-number <n>] [--project-field <name>] [--project-value <value> | --no-project-filter]
       select-issue.sh [--label <name>] --all
       select-issue.sh --issue <n>'

set_opt() { # <flag> <value>
  # The remedy for an empty value differs by flag, and naming the wrong one is
  # worse than naming none: a milestone and a project scope are cleared by their
  # own flags (or `--all`), while a label cannot be cleared at all because it is
  # what the backlog is.
  if [ -z "$2" ]; then
    case "$1" in
      --label)     fail 4 "--label needs a non-empty value; the backlog is always defined by a label, so there is nothing to clear" ;;
      --milestone) fail 4 "--milestone needs a non-empty value; to run with no milestone, pass --no-milestone-filter or --all" ;;
      --issue)     fail 4 "--issue needs a non-empty value; to search the backlog instead, pass no --issue at all" ;;
      *)           fail 4 "$1 needs a non-empty value; to run unscoped, pass --no-project-filter or --all" ;;
    esac
  fi
  case "$1" in
    --project-owner)  OPT_PROJECT_OWNER=$2;  OPT_PROJECT_OWNER_SET=1;  GIVEN="$GIVEN $1" ;;
    --project-number) OPT_PROJECT_NUMBER=$2; OPT_PROJECT_NUMBER_SET=1; GIVEN="$GIVEN $1" ;;
    --project-field)  OPT_PROJECT_FIELD=$2;  OPT_PROJECT_FIELD_SET=1;  GIVEN="$GIVEN $1" ;;
    --project-value)  OPT_PROJECT_VALUE=$2;  OPT_PROJECT_VALUE_SET=1;  GIVEN="$GIVEN $1" ;;
    --label)          OPT_LABEL=$2;          OPT_LABEL_SET=1;          LABELLED=" $1"; return 0 ;;
    --milestone)      OPT_MILESTONE=$2;      OPT_MILESTONE_SET=1 ;;
    --issue)
      case "$2" in
        *[!0-9]*|0) fail 4 "--issue must be a positive integer (found '$2')" ;;
      esac
      OPT_ISSUE=$2; OPT_ISSUE_SET=1
      # Not a narrowing of the backlog but a replacement for searching one, so
      # it is checked against the others below rather than counted among them.
      return 0 ;;
  esac
  NARROWING="$NARROWING $1"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --project-owner|--project-number|--project-field|--project-value|--label|--milestone|--issue)
      [ $# -ge 2 ] || fail 4 "$1 needs a value"
      set_opt "$1" "$2"
      shift 2 ;;
    --project-owner=*|--project-number=*|--project-field=*|--project-value=*|--label=*|--milestone=*|--issue=*)
      set_opt "${1%%=*}" "${1#*=}"
      shift ;;
    --no-project-filter)
      OPT_NO_PROJECT=1
      NARROWING="$NARROWING $1"
      shift ;;
    --no-milestone-filter)
      OPT_NO_MILESTONE=1
      NARROWING="$NARROWING $1"
      shift ;;
    --all)
      OPT_ALL=1
      shift ;;
    *)
      fail 4 "unknown argument '$1'; $USAGE" ;;
  esac
done

# A clearing flag and a narrowing one on the same axis is not a request this can
# answer: it would have to silently discard one of the two, and discarding either
# is a run that is not the run that was asked for. `--no-project-filter` is "run
# unscoped", so it contradicts every flag that describes a scope and not just the
# value.
[ "$OPT_NO_PROJECT" -eq 1 ] && [ -n "$GIVEN" ] \
  && fail 4 "--no-project-filter cannot be combined with$GIVEN"
[ "$OPT_NO_MILESTONE" -eq 1 ] && [ "$OPT_MILESTONE_SET" -eq 1 ] \
  && fail 4 "--no-milestone-filter cannot be combined with --milestone"

# `--all` says "no optional narrowing at all", which contradicts every flag that
# narrows one — and every flag that clears one, which it already does. Accepting
# the pair would leave "unnarrowed except for this" as a meaning nobody asked
# for. `--label` is deliberately not on that list: the label defines the backlog
# rather than narrowing it, so `--all --label task` is "the whole `task` backlog".
[ "$OPT_ALL" -eq 1 ] && [ -n "$NARROWING" ] \
  && fail 4 "--all cannot be combined with$NARROWING; it already drops every optional narrowing"

# `--issue` bypasses the search entirely, so every flag that describes *which*
# search to run is contradictory beside it — including `--label`, which under
# every other invocation says what the backlog is. Saying so is the difference
# between a run that knows it selected an out-of-backlog issue and one that
# thinks it narrowed a backlog it never read.
if [ "$OPT_ISSUE_SET" -eq 1 ]; then
  ISSUE_CONFLICTS="$NARROWING$LABELLED"
  [ "$OPT_ALL" -eq 1 ] && ISSUE_CONFLICTS="$ISSUE_CONFLICTS --all"
  [ -n "$ISSUE_CONFLICTS" ] \
    && fail 4 "--issue cannot be combined with$ISSUE_CONFLICTS; it selects one issue instead of searching the backlog"
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
REVIEW_REQUIRED=$(jq -r 'if ((.review | type) == "object") and (.review | has("required")) then (.review.required | tostring) else "" end' "$CFG")
REVIEWERS_KIND=$(jq -r 'if (.review | type) != "object" then "no-review" elif ((.review | has("reviewers")) | not) then "absent" else (.review.reviewers | type) end' "$CFG")
REVIEWERS=$(jq -r 'if (.review.reviewers | type) == "array" then (.review.reviewers | map(tostring) | join(" ")) else "" end' "$CFG")

# Flag over config, for the same reason the project keys work that way: the
# config describes the repository's backlog, and a run is not entitled to rewrite
# a tracked file to describe itself. The config value is still validated when the
# flag did not replace it, so a broken file is caught on the runs that use it.
[ "$OPT_LABEL_SET" -eq 1 ] && LABEL=$OPT_LABEL
[ -n "$LABEL" ] || fail 4 "$CFG: select.label is missing or empty; set it, or pass --label <name>"

[ "$OPT_MILESTONE_SET" -eq 1 ] && MILESTONE=$OPT_MILESTONE
{ [ "$OPT_NO_MILESTONE" -eq 1 ] || [ "$OPT_ALL" -eq 1 ]; } && MILESTONE=""

case "$LIMIT" in
  ''|*[!0-9]*) fail 4 "$CFG: select.limit must be a positive integer (found '${LIMIT:-null}')" ;;
  0)           fail 4 "$CFG: select.limit must be at least 1" ;;
esac
case "$STYLE" in
  blocked-by|depends-on|native|none) ;;
  *) fail 4 "$CFG: dependencies.style must be one of blocked-by, depends-on, native, none (found '${STYLE:-null}'); run backlog:setup-backlog" ;;
esac
# Checked here rather than at step 4, where an empty list is indistinguishable
# from a list that passed: a config with no verify commands opens a pull request
# nothing local ever looked at, and selection is the first chance to say so.
[ "$VERIFY_N" -ge 1 ] 2>/dev/null \
  || fail 4 "$CFG: verify must be a non-empty array of commands; run backlog:setup-backlog"

# The reviewer roster, checked in the same place and for the same reason. An
# unknown rung or a `none` that is not last would otherwise surface at step 7 —
# forty minutes in, with the implementation written, CI green and a pull request
# already open — and the only remedy at that point is an edit to this file,
# which is the one thing a run is not allowed to do to get itself unstuck.
#
# `review.required` is refused rather than translated. The migration is
# mechanical, which is exactly what makes translating it silently the wrong
# move: the key changed because a repository whose Copilot quota is exhausted
# has to *learn* that a roster exists, and a config that keeps working unchanged
# is a config nobody reads.
if [ -n "$REVIEW_REQUIRED" ]; then
  case "$REVIEW_REQUIRED" in
    false) WANT_ROSTER='["none"]' ;;
    *)     WANT_ROSTER='["copilot"]' ;;
  esac
  fail 4 "$CFG: review.required is no longer read; replace it with review.reviewers, an ordered roster tried in order -- $REVIEW_REQUIRED becomes $WANT_ROSTER, and [\"copilot\",\"local\",\"none\"] is what fails over instead of blocking"
fi
case "$REVIEWERS_KIND" in
  array) ;;
  no-review) fail 4 "$CFG: review must be an object holding a reviewers array; run backlog:setup-backlog" ;;
  absent)    fail 4 "$CFG: review.reviewers is missing; set it to an ordered roster, [\"copilot\"] at minimum" ;;
  *)         fail 4 "$CFG: review.reviewers must be an array of rung names (found a $REVIEWERS_KIND)" ;;
esac
[ -n "$REVIEWERS" ] \
  || fail 4 "$CFG: review.reviewers is empty; a roster with no rungs can never review and can never merge, so say which is meant -- [\"copilot\"] to gate, [\"none\"] to merge unreviewed on purpose"

REVIEWER_COUNT=0
for RUNG in $REVIEWERS; do
  REVIEWER_COUNT=$((REVIEWER_COUNT + 1))
  case "$RUNG" in
    copilot|local|none) ;;
    *) fail 4 "$CFG: review.reviewers names an unknown rung '$RUNG'; the rungs are copilot, local and none" ;;
  esac
done
# `none` last is a downgrade its operator chose. `none` anywhere else is a rung
# nothing can follow -- the roster reads as though it has a fallback when the
# fallback is unreachable, which is the worse of the two ways to be wrong.
REVIEWER_POS=0
for RUNG in $REVIEWERS; do
  REVIEWER_POS=$((REVIEWER_POS + 1))
  [ "$RUNG" = none ] && [ "$REVIEWER_POS" -ne "$REVIEWER_COUNT" ] \
    && fail 4 "$CFG: review.reviewers has 'none' at position $REVIEWER_POS of $REVIEWER_COUNT; none merges unreviewed, so no rung after it can ever run -- move it last, or drop the rungs behind it"
done
note "reviewer roster: ${REVIEWERS// / -> }"

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

# Piece by piece, so a run can override the axis, the value, or the board itself
# without any of the three implying the others. A repository whose config has no
# select.project at all assembles the whole scope here.
[ "$OPT_PROJECT_OWNER_SET" -eq 1 ]  && PROJECT_OWNER=$OPT_PROJECT_OWNER
[ "$OPT_PROJECT_NUMBER_SET" -eq 1 ] && PROJECT_NUMBER=$OPT_PROJECT_NUMBER
[ "$OPT_PROJECT_FIELD_SET" -eq 1 ]  && PROJECT_FIELD=$OPT_PROJECT_FIELD
[ "$OPT_PROJECT_VALUE_SET" -eq 1 ]  && PROJECT_VALUE=$OPT_PROJECT_VALUE
{ [ "$OPT_NO_PROJECT" -eq 1 ] || [ "$OPT_ALL" -eq 1 ]; } && PROJECT_VALUE=""

# Validated up front rather than at the point of use: a scope that cannot be
# resolved has to stop selection, not degrade it to the unscoped backlog.
#
# Every message names both places a piece can come from. "Add owner, number and
# field to the config" stopped being the whole answer once each of them had a
# flag, and a run told to edit a tracked file it does not need to edit will edit
# it.
MISSING=""
[ -n "$PROJECT_OWNER" ]  || MISSING="$MISSING owner (select.project.owner or --project-owner);"
[ -n "$PROJECT_NUMBER" ] || MISSING="$MISSING number (select.project.number or --project-number);"
[ -n "$PROJECT_FIELD" ]  || MISSING="$MISSING field (select.project.field or --project-field);"

if [ -n "$PROJECT_VALUE" ]; then
  [ -z "$MISSING" ] \
    || fail 4 "a project scope of '$PROJECT_VALUE' was requested but the scope is incomplete; missing:${MISSING%;}"
  NUMBER_SRC="$CFG: select.project.number"
  [ "$OPT_PROJECT_NUMBER_SET" -eq 1 ] && NUMBER_SRC="--project-number"
  case "$PROJECT_NUMBER" in
    *[!0-9]*) fail 4 "$NUMBER_SRC must be a positive integer (found '$PROJECT_NUMBER')" ;;
    0)        fail 4 "$NUMBER_SRC must be at least 1" ;;
  esac
elif [ -n "$GIVEN" ]; then
  # Naming a board or an axis with nothing to match on it is not an unscoped run
  # — it is a scoped run missing its value, and answering it with the whole
  # backlog is the silent widening every other failure here is written to avoid.
  # `--no-project-filter` is the way to say "unscoped", and it is refused above
  # in combination with these flags precisely so this case cannot be ambiguous.
  fail 4 "the project scope names$GIVEN but no value to scope by; add --project-value <value> or set select.project.value"
fi

REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) \
  || fail 4 "cannot resolve the repository slug from gh"
[ -n "$REPO" ] || fail 4 "cannot resolve the repository slug from gh"

SCOPE_NOTE=""
[ -n "$PROJECT_VALUE" ] && SCOPE_NOTE=" with $PROJECT_FIELD = '$PROJECT_VALUE' in project $PROJECT_OWNER/$PROJECT_NUMBER"
BLOCKED_SUBJECT="every open issue carrying the label '$LABEL'$SCOPE_NOTE"

# -------------------------------------------------------------- milestone -----
# The milestone is checked against the repository's own milestones before any
# issue is listed, exactly as a project value is checked against its field's
# declared options — and for the identical reason. `gh issue list --milestone` on
# a title that does not exist does not object:
#
#   $ gh issue list --repo z5labs/avroc --state open --label story --milestone v0.2.0 --json number
#   []
#   $ echo $?
#   0
#
# So a milestone that was closed, renamed or deleted resolves to an empty
# candidate set, which prints BACKLOG EMPTY and halts the loop on a backlog that
# is fully workable. That is not hypothetical: it is what `select.milestone:
# "v0.2.0"` did to avroc after the milestone was deleted, over an open, eligible,
# unmilestoned story.
#
# `state=all`, because a closed milestone can still hold open issues and naming
# one is a legitimate request. Both sources are checked, config included — a
# stale config value is the case that fired, and it is the one a flag-only check
# would still miss.
if [ -n "$MILESTONE" ] && [ "$OPT_ISSUE_SET" -eq 0 ]; then
  MILESTONE_SRC="$CFG: select.milestone"
  [ "$OPT_MILESTONE_SET" -eq 1 ] && MILESTONE_SRC="--milestone"

  MILESTONES=$(gh api --paginate "repos/$REPO/milestones?state=all&per_page=100" --jq '.[].title') \
    || fail 4 "cannot read the milestones of $REPO to check the one named by $MILESTONE_SRC"

  if ! printf '%s\n' "$MILESTONES" | grep -qxF -- "$MILESTONE"; then
    if [ -z "$MILESTONES" ]; then
      fail 4 "$MILESTONE_SRC names milestone '$MILESTONE', which $REPO does not have; it has no milestones at all"
    fi
    HAVE=$(printf '%s' "$MILESTONES" | tr '\n' ',')
    fail 4 "$MILESTONE_SRC names milestone '$MILESTONE', which $REPO does not have; its milestones are: ${HAVE%,}"
  fi
fi

if [ "$OPT_ISSUE_SET" -eq 1 ]; then
  # ------------------------------------------------------------- one issue -----
  # Named rather than searched for. The dependency walk below still runs against
  # it, so this changes *which* issues are considered and nothing about whether
  # the one considered is workable.
  #
  # Every narrowing it stepped over is named, one line each, whether or not that
  # narrowing was configured. A run that selected an issue outside the backlog it
  # would otherwise have searched has to say so — otherwise its diagnostics read
  # exactly like a run that searched and found this issue at the top.
  MILESTONE_NOTE="no milestone is configured"
  [ -n "$MILESTONE" ] && MILESTONE_NOTE="milestone '$MILESTONE'"
  PROJECT_NOTE="no project scope is configured"
  [ -n "$PROJECT_VALUE" ] && PROJECT_NOTE="$PROJECT_FIELD = '$PROJECT_VALUE' in project $PROJECT_OWNER/$PROJECT_NUMBER"

  note "--issue $OPT_ISSUE selects #$OPT_ISSUE directly; the backlog was not searched"
  note "--issue $OPT_ISSUE bypassed the label narrowing (label '$LABEL')"
  note "--issue $OPT_ISSUE bypassed the milestone narrowing ($MILESTONE_NOTE)"
  note "--issue $OPT_ISSUE bypassed the project narrowing ($PROJECT_NOTE)"

  ISSUE_JSON=$(gh issue view "$OPT_ISSUE" --repo "$REPO" --json number,title,state) \
    || fail 4 "cannot read issue #$OPT_ISSUE in $REPO"
  ISSUE_STATE=$(printf '%s' "$ISSUE_JSON" | jq -r '.state // ""') \
    || fail 4 "the response for issue #$OPT_ISSUE in $REPO does not parse as JSON"
  # A closed issue is refused rather than selected. The cycle ends by closing the
  # issue it worked, so an already-closed one is either finished work or a typo,
  # and both are worth a message rather than a worktree.
  [ "$ISSUE_STATE" = OPEN ] \
    || fail 4 "issue #$OPT_ISSUE in $REPO is ${ISSUE_STATE:-unreadable}, not OPEN"

  CANDIDATES=$(printf '%s' "$ISSUE_JSON" | jq -r '"\(.number)\t\(.title)"') \
    || fail 4 "the response for issue #$OPT_ISSUE in $REPO could not be read"
  BLOCKED_SUBJECT="issue #$OPT_ISSUE, named with --issue,"
else
  # ----------------------------------------------------------------- list -----
  # --limit is always passed. gh's default page size is 30, and on a longer
  # backlog the issues past the first page do not error — they are simply absent,
  # so the cycle reports an empty or blocked backlog that is neither.
  #
  # --milestone is passed only when one is in effect. An empty --milestone is not
  # the same as no milestone filter, which is why this is an argument array
  # rather than a string the caller interpolates.
  LIST_ARGS=(--repo "$REPO" --state open --label "$LABEL" --limit "$LIMIT")
  [ -n "$MILESTONE" ] && LIST_ARGS+=(--milestone "$MILESTONE")

  CANDIDATES=$(gh issue list "${LIST_ARGS[@]}" \
    --json number,title --jq 'sort_by(.number)[] | "\(.number)\t\(.title)"') \
    || fail 4 "gh issue list failed for $REPO"

  if [ -z "$CANDIDATES" ]; then
    printf 'BACKLOG EMPTY\n'
    note "no open issue in $REPO carries the label '$LABEL'${MILESTONE:+ in milestone '$MILESTONE'}"
    exit 10
  fi

  # -------------------------------------------------------------- scoping -----
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
    need_errfile
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
fi

# --------------------------------------------------------------- eligible -----
# Walk in ascending number order and take the first issue whose every declared
# dependency is CLOSED. An issue with a dependency this script cannot read is
# skipped rather than treated as eligible — but it is skipped as *ineligible*,
# with its reason recorded, so the backlog is not starved by one unreadable
# reference while other issues are workable.
#
# A dependency elsewhere is read exactly like one here. `owner/repo#N` names a
# repository `gh issue view --repo` accepts, so the only thing that ever made it
# unresolvable was this function pinning the repository to $REPO — and an edge
# that cannot decay does not describe a dependency, it retires the issue holding
# it. That is the natural shape of a multi-repository backlog: stories in one
# repository waiting on work in another, which is the case `blockedBy` carries
# `repository { nameWithOwner }` for in the first place.
#
# Unreadable is still a blocker, and that is the half of the old behaviour worth
# keeping: a private repository, a token without the scope, or a deleted issue
# yields UNREADABLE rather than an assumption, because calling a blocked issue
# eligible is the failure this whole step avoids.
#
# Keyed on `owner/repo#N` rather than the bare number. An issue number is only
# unique within its repository, so a cache keyed on the number alone would let
# this repository's closed #12 answer for another repository's open one — a
# dependency resolved against the wrong issue entirely.
#
# The answer comes back in DEP_STATE rather than on stdout, which is the only
# form in which the cache works at all. A function whose result is read with
# `$(...)` runs in a subshell, so every write to STATE_CACHE was discarded the
# moment it returned and each edge cost a fresh `gh issue view` — including the
# repeated ones this exists to collapse, on a backlog where several issues
# commonly wait on the same blocker.
STATE_CACHE=""
DEP_STATE=""
dep_state() { # <repo> <issue-number> ; answers in DEP_STATE
  local key="$1#$2" hit
  case "$STATE_CACHE" in
    *"|$key="*) hit=${STATE_CACHE##*"|$key="}; DEP_STATE=${hit%%|*}; return 0 ;;
  esac
  local s
  s=$(gh issue view "$2" --repo "$1" --json state --jq .state 2>/dev/null) || s=""
  [ -n "$s" ] || s=UNREADABLE
  STATE_CACHE="$STATE_CACHE|$key=$s|"
  DEP_STATE=$s
}

[ "$STYLE" = native ] && need_errfile

REASONS=""
while IFS=$'\t' read -r NUM TITLE; do
  [ -n "$NUM" ] || continue

  if [ "$STYLE" = none ]; then
    note "#$NUM eligible (dependencies.style is none)"
    jq -n --argjson n "$NUM" --arg t "$TITLE" '{number:$n, title:$t}'
    exit 0
  fi

  # Exactly one of these two runs, chosen by the declared style and by nothing
  # else. There is deliberately no path from one to the other: a body parse that
  # finds nothing must not escalate to the typed edges, and typed edges that
  # come back empty must not fall back to reading the body. Either fallback
  # turns a repository that has not adopted the other convention into one where
  # every issue reads as eligible.
  blockers=""
  if [ "$STYLE" = native ]; then
    if ! REFS=$(native_fetch "$REPO" "$NUM" 2>"$ERRFILE"); then
      # Unreadable, not unblocked. Ineligible with the reason recorded, and the
      # walk continues — one issue whose dependencies cannot be read does not
      # starve a backlog whose other issues are workable.
      REFS=""
      native_err=$(tr '\n' ' ' <"$ERRFILE")
      blockers=" its native dependencies could not be read: ${native_err:-gh api graphql failed};"
    fi
  else
    BODY=$(gh issue view "$NUM" --repo "$REPO" --json body --jq .body 2>/dev/null) || BODY=""
    REFS=$(printf '%s\n' "$BODY" | extract_refs "$STYLE")
  fi

  for ref in $REFS; do
    case "$ref" in
      \#*)
        d=${ref#\#}
        dep_state "$REPO" "$d"
        [ "$DEP_STATE" = CLOSED ] || blockers="$blockers #$d is $DEP_STATE;"
        ;;
      */*\#*)
        # owner/repo#N — the same read against the repository it names.
        dep_state "${ref%#*}" "${ref##*#}"
        [ "$DEP_STATE" = CLOSED ] || blockers="$blockers $ref is $DEP_STATE;"
        ;;
      *)
        # Defensive. Both producers of $REFS print one of the two shapes above
        # and nothing else, so this is a reference that was not recognised
        # rather than one that was not resolvable — and an unrecognised
        # reference is a dependency dropped, which reads as an eligible issue.
        blockers="$blockers $ref is not a reference this cycle can resolve;"
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

printf 'BLOCKED — %s has an unmet dependency:\n' "$BLOCKED_SUBJECT"
printf '%s' "$REASONS"
exit 11
