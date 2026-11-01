#!/usr/bin/env bash
#
# closing-refs.sh — which issues a pull request body's closing keywords name.
#
# Usage: closing-refs.sh <pr-number> <issue-number>
#        closing-refs.sh --extract        # a body on stdin, no network
#
# `<pr> <n>` reads the pull request's body and exits 0 when its closing keywords
# name exactly one issue, and that issue is #<n>. `--extract` prints every issue
# they name, one per line, in the order written: `#14` for this repository,
# `owner/repo#N` for anywhere else.
#
# Why this replaces asking GitHub.
#
# Step 5 of next-issue used to read `closingIssuesReferences`, GitHub's own parse
# of the body, which exists only in GraphQL. A Claude Code cloud session's GitHub
# proxy refuses GraphQL, and no REST route returns the same list, so the body is
# read over REST (`GET /repos/{owner}/{repo}/pulls/{n}`) and parsed here. This is
# a WEAKER check than GitHub's and says so in the skill: it can tell that the
# keyword the cycle needs is written and that no other issue is named beside it,
# but not that GitHub linked it — a reference GitHub declines to link, such as one
# to a repository the token cannot see, reads the same here as one it accepted.
#
# What counts as a closing reference, kept to GitHub's documented forms:
#
#   * A keyword — close, closes, closed, fix, fixes, fixed, resolve, resolves,
#     resolved — as a word of its own, any case, then a space or a colon, then
#     the reference: `#N`, `owner/repo#N`, or an issue URL on github.com.
#   * One reference per keyword. `Closes #12, #13` closes 12 and only 12, and
#     reading 13 as well would report a body that is fine as one that closes too
#     much.
#   * Text inside a ``` or ~~~ fence is not read, because GitHub does not link
#     keywords there — which is exactly why the skill writes post-merge criteria
#     inside one, since they can carry a `close #757` of their own.
#   * Inline code is read. Whether GitHub links a keyword inside backticks is not
#     something this plugin has observed, so it is counted, in the direction
#     that reports an extra issue rather than missing one.
#
# Exit codes:
#   0  (`<pr> <n>`) the body names exactly #<n>; (`--extract`) the list printed
#   1  the body names no issue, another issue, or more than one; stderr says which
#   4  usage or precondition failure — the repository or the body could not be read

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

fail() { local code=$1; shift; printf 'closing-refs: %s\n' "$*" >&2; exit "$code"; }

EXTRACT_AWK=$(cat <<'AWK'
function out(ref) { if (!(ref in seen)) { seen[ref] = 1; order[++n] = ref } }

# The reference at the very start of `s`, in its printed form, or "".
function leading_ref(s,   m, parts) {
  if (match(s, /^https?:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/issues\/[0-9]+/)) {
    m = substr(s, RSTART, RLENGTH)
    sub(/^https?:\/\/github\.com\//, "", m)
    split(m, parts, "/")
    return parts[1] "/" parts[2] "#" parts[4]
  }
  if (match(s, /^([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)?#[0-9]+/))
    return substr(s, RSTART, RLENGTH)
  return ""
}

{
  probe = $0
  sub(/^[[:space:]]*/, "", probe)
  if (probe ~ /^(```|~~~)/) { fence = !fence; next }
  if (fence) next

  line = $0
  lower = tolower(line)
  pos = 1
  while (pos <= length(lower)) {
    seg = substr(lower, pos)
    if (!match(seg, /(^|[^a-z0-9_])(close[sd]?|fix(e[sd])?|resolve[sd]?)([[:space:]]*:[[:space:]]*|[[:space:]]+)/)) break
    start = pos + RSTART - 1
    len = RLENGTH
    ref = leading_ref(substr(line, start + len))
    if (ref != "") out(ref)
    pos = start + len
  }
}

END { for (i = 1; i <= n; i++) print order[i] }
AWK
)

extract() { awk "$EXTRACT_AWK"; }

if [ "${1:-}" = --extract ]; then
  [ $# -eq 1 ] || fail 4 "--extract reads the body on stdin and takes no other argument"
  extract
  exit 0
fi

[ $# -eq 2 ] || fail 4 "usage: closing-refs.sh <pr-number> <issue-number> | --extract"
PR=$1
ISSUE=$2
case "$PR" in ''|*[!0-9]*) fail 4 "pull request number must be an integer (got '$PR')" ;; esac
case "$ISSUE" in ''|*[!0-9]*) fail 4 "issue number must be an integer (got '$ISSUE')" ;; esac
command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"

REPO=$("$HERE/repo.sh") || fail 4 "cannot resolve the repository; see the repo: line above"
REPO=${REPO%%$'\t'*}
[ -n "$REPO" ] || fail 4 "cannot resolve the repository; repo.sh printed no slug"

BODY=$(gh api "repos/$REPO/pulls/$PR" --jq '.body // ""' 2>/dev/null) \
  || fail 4 "cannot read the body of PR #$PR in $REPO"

# A reference to this repository written out in full is the same issue as `#N`,
# and GitHub compares owner and name without regard to case.
REFS=$(printf '%s\n' "$BODY" | extract | awk -v repo="$REPO" '
  { r = $0; i = index(r, "#")
    if (i > 1 && tolower(substr(r, 1, i - 1)) == tolower(repo)) r = substr(r, i)
    if (!(r in seen)) { seen[r] = 1; print r } }')

if [ "$REFS" = "#$ISSUE" ]; then
  printf '#%s\n' "$ISSUE"
  exit 0
fi

if [ -z "$REFS" ]; then
  fail 1 "PR #$PR's body has no closing keyword naming an issue outside a fence; add 'Closes #$ISSUE' on a line of its own"
fi
FOUND=$(printf '%s' "$REFS" | tr '\n' ' ')
case " $FOUND " in
  *" #$ISSUE "*)
    fail 1 "PR #$PR's body closes ${FOUND% } -- more than #$ISSUE. The merge would close every one of them; move any reference that is not this story's into a fence, or reword it without a closing keyword" ;;
  *)
    fail 1 "PR #$PR's body closes ${FOUND% }, not #$ISSUE. Add 'Closes #$ISSUE', and move or reword the reference that is not this story's" ;;
esac
