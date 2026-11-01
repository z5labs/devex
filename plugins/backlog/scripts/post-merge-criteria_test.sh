#!/usr/bin/env bash
#
# post-merge-criteria_test.sh — the fixture corpus for post-merge-criteria.sh.
#
# Offline. Every case but the last few is an issue body fed to `--extract`, and
# the assertion is the exact JSON array that comes back; the last few read the
# body from a `gh` that answers from files. A criterion read as post-merge when it
# is not is work the worker defers past the merge and never checks before the
# pull request; one missed is a box the worker cannot satisfy, so it stops or
# improvises. Both directions are cases here.
#
# Run: plugins/backlog/scripts/post-merge-criteria_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/post-merge-criteria.sh"
[ -x "$SUT" ] || { printf 'post-merge-criteria.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

# check <name> <expected compact JSON> <body>
check() {
  local name=$1 want=$2 body=$3 got
  got=$(printf '%s' "$body" | "$SUT" --extract)
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1))
    printf '  ok   %-58s\n' "$name"
  else
    fail=$((fail + 1))
    printf '  FAIL %-58s\n       want %s\n       got  %s\n' "$name" "$want" "$got"
  fi
}

printf '\nhow many\n'

check 'no post-merge criterion' '[]' \
'### Acceptance Criteria

- [ ] The parser accepts a trailing comma.
- [ ] `go test ./...` passes.'

check 'an empty body' '[]' ''

check 'one post-merge criterion' \
'["After merge: comment on #757 with a link to the merged pull request, then close #757 as Resolved."]' \
'### Acceptance Criteria

- [ ] The fix lands with a regression test.
- [ ] After merge: comment on #757 with a link to the merged pull request, then close #757 as Resolved.'

check 'several, in the order the issue lists them' \
'["After merge: comment on #757.","After merge: close #757 as Resolved.","After merge: post in the release thread."]' \
'### Acceptance Criteria

- [ ] After merge: comment on #757.
- [ ] The fix lands.
- [ ] After merge: close #757 as Resolved.

- [ ] After merge: post in the release thread.'

printf '\nwhat counts as one\n'

# The convention's own example, quoted inside a fence in an issue that explains
# it — issue #458 itself is written that way. An example is not a declaration.
check 'an After merge: line inside a ``` fence' '[]' \
'#### The convention

```
- [ ] After merge: comment on #757 and close it.
```

### Acceptance Criteria

- [ ] The skill documents the convention.'

check 'an After merge: line inside a ~~~ fence' '[]' \
'~~~markdown
- [ ] After merge: close #1.
~~~'

check 'a criterion after a closed fence still counts' '["After merge: close #1."]' \
'```
- [ ] After merge: not this one.
```

- [ ] After merge: close #1.'

check 'After merge: text that does not start the criterion' '[]' \
'- [ ] Document what happens to an After merge: criterion.
- [ ] The step that runs After merge: is named in the report.'

check 'After merge: in prose, not in a task-list item' '[]' \
'After merge: comment on #757.

- After merge: close #757.'

check 'a checked box is still a criterion' '["After merge: close #9."]' \
'- [x] After merge: close #9.'

check 'case and bold do not matter' \
'["after merge: close #1.","**After merge:** close #2.","AFTER MERGE: close #3."]' \
'- [ ] after merge: close #1.
- [ ] **After merge:** close #2.
* [X] AFTER MERGE: close #3.'

check 'an ordered task list' '["After merge: close #4."]' \
'1. [ ] Implement it.
2. [ ] After merge: close #4.'

check 'a nested post-merge criterion under an ordinary one' '["After merge: close #5."]' \
'- [ ] The cycle handles it:
  - [ ] After merge: close #5.
  - [ ] the rest is ordinary.'

printf '\nwhere one ends\n'

check 'a criterion wrapped onto indented lines' \
'["After merge: comment on #757 with a link to the merged pull request\nand what changed, then close #757 as Resolved."]' \
'- [ ] After merge: comment on #757 with a link to the merged pull request
      and what changed, then close #757 as Resolved.
- [ ] Not this one.'

check 'nested items and a blank line belong to it' \
'["After merge: do both:\n- comment on #757;\n\n- close #757."]' \
'- [ ] After merge: do both:
  - comment on #757;

  - close #757.

Some later paragraph.'

check 'a fence indented under it belongs to it' \
'["After merge: run\n```\n- [ ] After merge: not a second one\n```\nthen close #1."]' \
'- [ ] After merge: run
  ```
  - [ ] After merge: not a second one
  ```
  then close #1.'

check 'an unindented line ends it' '["After merge: close #1."]' \
'- [ ] After merge: close #1.
Trailing paragraph, not indented.'

check 'a heading ends it' '["After merge: close #1."]' \
'- [ ] After merge: close #1.

### Related Issues

- #2'

printf '\nusage\n'

usage_check() { # <name> <expected exit> <args...>
  local name=$1 want=$2; shift 2
  "$SUT" "$@" >/dev/null 2>&1 </dev/null
  local got=$?
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '  ok   %-58s [exit %s]\n' "$name" "$got"
  else
    fail=$((fail + 1)); printf '  FAIL %-58s want exit %s got %s\n' "$name" "$want" "$got"
  fi
}

usage_check 'no arguments' 4
usage_check 'an issue number that is not a number' 4 not-a-number
usage_check '--extract takes no other argument' 4 --extract 12
usage_check 'an unknown flag' 4 --bogus

printf '\nthe body, read from GitHub\n'

# The one network call, run whole against a scratch checkout and a `gh` that
# answers from files: the repository from the origin remote, confirmed over REST,
# and the body from `GET /repos/{owner}/{repo}/issues/{n}`. `gh issue view` was
# the call here, and it is GraphQL-backed, which a Claude Code cloud session's
# GitHub proxy refuses. The stub sources the guard that refuses everything
# GraphQL-backed and logs it, and the last case here asserts nothing was.
SCRATCH=$(mktemp -d) || { printf 'cannot create a scratch directory\n' >&2; exit 1; }
trap 'rm -rf "$SCRATCH"' EXIT
mkdir -p "$SCRATCH/bin" "$SCRATCH/fx"
export GH_GUARD="$HERE/testdata/gh-guard.sh"
export GH_GUARD_LOG="$SCRATCH/graphql-calls"
: >"$GH_GUARD_LOG"
git init -q "$SCRATCH/repo" >/dev/null 2>&1 || { printf 'cannot init a scratch repository\n' >&2; exit 1; }
git -C "$SCRATCH/repo" remote add origin https://github.com/z5labs/devex.git

cat >"$SCRATCH/bin/gh" <<'STUB'
#!/usr/bin/env bash
. "$GH_GUARD"
q="" path="" prev=""
for a in "$@"; do [ "$prev" = --jq ] && q=$a; case "$a" in repos/*) path=$a ;; esac; prev=$a; done
out() { if [ -n "$q" ]; then jq -r "$q"; else cat; fi; }
case "$path" in
  repos/z5labs/devex)            printf '{"full_name":"z5labs/devex","default_branch":"main"}\n' | out ;;
  repos/z5labs/devex/issues/*)
    f="$FX/issue-${path##*/}"
    [ -f "$f" ] || { printf '{"message":"Not Found"}'; printf 'gh: Not Found (HTTP 404)\n' >&2; exit 1; }
    out <"$f" ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$SCRATCH/bin/gh"

jq -n --arg b $'### Acceptance Criteria\n\n- [ ] The fix lands.\n- [ ] After merge: close #757 as Resolved.' \
  '{number: 5, state: "open", body: $b}' >"$SCRATCH/fx/issue-5"
# An issue created with no description answers `"body": null`.
printf '{"number":6,"state":"open","body":null}\n' >"$SCRATCH/fx/issue-6"

# fetch <name> <issue> <expected exit> <expected stdout, or ''>
fetch() {
  local got rc
  got=$(cd "$SCRATCH/repo" && PATH="$SCRATCH/bin:$PATH" FX="$SCRATCH/fx" "$SUT" "$2" 2>/dev/null)
  rc=$?
  if [ "$rc" = "$3" ] && { [ -z "$4" ] || [ "$got" = "$4" ]; }; then
    pass=$((pass + 1)); printf '  ok   %-58s\n' "$1"
  else
    fail=$((fail + 1)); printf '  FAIL %-58s\n       want exit %s [%s]\n       got  exit %s [%s]\n' "$1" "$3" "$4" "$rc" "$got"
  fi
}

fetch 'the criteria of an issue read over REST' 5 0 '["After merge: close #757 as Resolved."]'
fetch 'an issue with no body has none, and is not a failure' 6 0 '[]'
# Unreadable is never `[]`: a follow-up the cycle never learned about is one
# that silently never happens.
fetch 'an issue that cannot be read is exit 4, not []' 7 4 ''

printf '\nGraphQL\n'
if [ -s "$GH_GUARD_LOG" ]; then
  fail=$((fail + 1)); printf '  FAIL %-58s %s\n' 'no GraphQL-backed gh call was made' "$(tr '\n' ';' <"$GH_GUARD_LOG")"
else
  pass=$((pass + 1)); printf '  ok   %-58s\n' 'no GraphQL-backed gh call was made'
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
