#!/usr/bin/env bash
#
# select-issue.sh — pick the next eligible backlog issue, as one call.
#
# Usage: select-issue.sh
#        select-issue.sh --extract <blocked-by|depends-on|none> [file]
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
# Exit codes:
#   0   an issue was selected; stdout is {"number":N,"title":"..."}
#   4   usage or precondition failure (no gh/jq/git, or the config is unusable)
#   10  BACKLOG EMPTY — no open issue carries the label (and milestone)
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

[ $# -eq 0 ] || fail 4 "usage: select-issue.sh [--extract <style> [file]]"

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

if [ -z "$CANDIDATES" ]; then
  printf 'BACKLOG EMPTY\n'
  note "no open issue in $REPO carries the label '$LABEL'${MILESTONE:+ in milestone '$MILESTONE'}"
  exit 10
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

printf 'BLOCKED — every open issue carrying the label '\''%s'\'' has an unmet dependency:\n' "$LABEL"
printf '%s' "$REASONS"
exit 11
