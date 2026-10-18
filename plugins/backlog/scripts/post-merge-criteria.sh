#!/usr/bin/env bash
#
# post-merge-criteria.sh — the acceptance criteria that can only be met once the
# pull request has merged.
#
# Usage: post-merge-criteria.sh <issue-number>
#        post-merge-criteria.sh --extract       # an issue body on stdin, no network
#
# Prints a JSON array of strings, one per post-merge criterion, in the order the
# issue lists them. `[]` is the common answer and is not a failure: a story with
# no post-merge criterion runs exactly as it always did.
#
# The convention. A criterion is a task-list item — `- [ ]`, `* [x]`, `1. [ ]` —
# and it is a post-merge criterion when its text BEGINS `After merge:`:
#
#   - [ ] After merge: comment on #757 with a link to the merged pull request
#         and what changed, then close #757 as Resolved.
#
# Why this is a script and not a sentence in the skill.
#
# The answer decides what the worker does in two places far apart: which boxes
# it must satisfy before the pull request opens, and which it carries out after
# the merge. A criterion misread as an ordinary one is work the worker cannot
# finish before the pull request — so it stops, or improvises. One misread the
# other way is a box nobody satisfies. And reading it by eye repeats every
# mistake the dependency extraction in select-issue.sh once made: a fenced
# example quoting the convention, or prose that merely mentions it, read as a
# declaration. `post-merge-criteria_test.sh` is the offline corpus for those.
#
# Rules, each pinned by that corpus:
#   - Text inside a ``` or ~~~ fence is example text, not a criterion — unless
#     the fence is indented under a post-merge criterion, where it is part of it.
#   - `After merge:` has to START the item's text. Case does not matter, and a
#     bold `**After merge:**` counts. Anywhere later in the item, it is prose.
#   - A criterion runs on: lines indented deeper than its bullet are part of it,
#     nested items included, and blank lines between them do not end it.
#   - The box's state is not read. The issue body is the spec as it was written,
#     and the cycle never ticks the issue's own boxes.
#   - The text is returned verbatim, `After merge:` included, with the
#     continuation lines' indentation under the bullet removed.
#
# Exit codes:
#   0  a JSON array on stdout, possibly empty
#   4  usage or precondition failure — the issue could not be read

set -uo pipefail

fail() { local code=$1; shift; printf 'post-merge-criteria: %s\n' "$*" >&2; exit "$code"; }

command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

# Records are separated by \036 (ASCII record separator), which no issue body
# carries, so a criterion may span lines without a quoting scheme of its own.
EXTRACT_AWK=$(cat <<'AWK'
function lead(s) { match(s, /^[[:space:]]*/); return RLENGTH }

function flush() {
  if (cur == "") return
  printf "%s\036", cur
  cur = ""; held = ""
}

# Removes up to `n` columns of leading whitespace — the indentation that puts a
# line under the bullet, not the indentation the line carries inside it.
function dedent(s, n,   k) {
  k = lead(s)
  if (k > n) k = n
  return substr(s, k + 1)
}

function append(s) {
  cur = cur "\n" held dedent(s, body)
  held = ""
}

{
  probe = $0
  sub(/^[[:space:]]*/, "", probe)
  isfence = (probe ~ /^(```|~~~)/)

  # A fence opened inside a post-merge criterion belongs to it, verbatim.
  if (owned) {
    append($0)
    if (isfence) { owned = 0; fence = 0 }
    next
  }
  if (fence) { if (isfence) fence = 0; next }

  if (cur != "") {
    if ($0 !~ /[^[:space:]]/) { held = held "\n"; next }
    if (lead($0) > bullet) {
      append($0)
      if (isfence) owned = 1
      next
    }
    flush()
  }

  if (isfence) { fence = 1; next }

  if (match($0, /^[[:space:]]*([-*+]|[0-9]+[.)])[[:space:]]+\[[ xX]\][[:space:]]+/)) {
    # Saved before lead() runs: it calls match() and overwrites RLENGTH.
    width = RLENGTH
    text = substr($0, width + 1)
    label = tolower(text)
    sub(/^(\*\*|__)/, "", label)
    if (label ~ /^after[[:space:]]+merge[[:space:]]*:/) {
      cur = text
      body = width
      bullet = lead($0)
    }
  }
}

END { flush() }
AWK
)

extract() { # body on stdin
  awk "$EXTRACT_AWK" | jq -R -s -c 'split("\u001e") | .[:-1]'
}

case "${1:-}" in
  --extract)
    [ $# -eq 1 ] || fail 4 "--extract reads the body on stdin and takes no other argument"
    extract
    exit 0
    ;;
  ''|-*)
    fail 4 "usage: post-merge-criteria.sh <issue-number> | --extract"
    ;;
esac

[ $# -eq 1 ] || fail 4 "usage: post-merge-criteria.sh <issue-number> | --extract"
case "$1" in *[!0-9]*) fail 4 "issue number must be a number, got '$1'" ;; esac
command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"

# An unreadable body is a failure, never `[]`. Reporting no post-merge criteria
# for an issue that has some is a merge whose follow-up silently never happens.
body=$(gh issue view "$1" --json body --jq .body 2>/dev/null) \
  || fail 4 "cannot read the body of issue #$1"

printf '%s\n' "$body" | extract
