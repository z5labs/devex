#!/usr/bin/env bash
#
# closing-refs_test.sh — the fixture corpus for closing-refs.sh.
#
# Two halves, both offline.
#
# **Extraction.** Every case is a pull request body fed to `--extract`, and the
# assertion is the exact list of issues its closing keywords name. A reference
# missed is a story whose issue the merge leaves open; one invented is a body
# the cycle stops to fix for nothing. The case that matters most is the one the
# skill's own convention creates: a post-merge criterion like `close #757`,
# quoted inside a fence, must not count — GitHub does not link it there, and the
# fence is the whole reason it is safe to write.
#
# **The check.** The whole script against a scratch checkout and a `gh` that
# answers from files: the body is read from `GET /repos/{owner}/{repo}/pulls/{n}`
# and must name exactly the story's issue.
#
# Run: plugins/backlog/scripts/closing-refs_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/closing-refs.sh"
[ -x "$SUT" ] || { printf 'closing-refs.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

ok()  { pass=$((pass + 1)); printf '  ok   %-60s [%s]\n' "$1" "$2"; }
bad() { fail=$((fail + 1)); printf '  FAIL %-60s %s\n' "$1" "$2"; }

# check <name> <expected refs, space separated> <body>
check() {
  local got
  got=$(printf '%s' "$3" | "$SUT" --extract | tr '\n' ' ')
  got=${got% }
  if [ "$got" = "$2" ]; then ok "$1" "${got:-<none>}"; else bad "$1" "want [$2] got [$got]"; fi
}

printf '\nthe keywords\n'

check 'Closes #N on a line of its own'      '#12' $'Some summary.\n\nCloses #12'
check 'every keyword GitHub documents' '#1 #2 #3 #4 #5 #6 #7 #8 #9' \
  $'close #1\ncloses #2\nclosed #3\nfix #4\nfixes #5\nfixed #6\nresolve #7\nresolves #8\nresolved #9'
check 'any case'                            '#12' 'CLOSES #12'
check 'a colon after the keyword'           '#12' 'Fixes: #12'
check 'mid-sentence'                        '#12' 'This change closes #12 by adding a guard.'
check 'another repository'                  'z5labs/other#42' 'Resolves z5labs/other#42'
check 'an issue URL'                        'z5labs/other#42' 'Fixes https://github.com/z5labs/other/issues/42'
check 'two keywords, two issues, in order'  '#14 #12' $'Closes #14\nFixes #12'
check 'a repeat is one issue'               '#12' $'Closes #12\n\nAlso closes #12.'

printf '\nwhat is not a closing reference\n'

# GitHub needs a keyword per issue, so the second reference here closes nothing.
check 'one reference per keyword'           '#12' 'Closes #12, #13'
check 'a reference with no keyword'         ''    'Related to #12 and #13.'
check 'a keyword inside a longer word'      ''    'This encloses #12 and prefixes #13.'
check 'a keyword not followed by a reference' '' 'Fixed a bug where #12 crashed.'
check 'a keyword followed by other words'   ''    'Fixes the crash in #12.'

# The skill's own convention, and the case this script exists to get right: the
# post-merge criterion is written in a fence so that its `close #757` is not a
# closing keyword. Read, it would make the check report a body that is fine.
check 'a post-merge criterion inside a fence' '#480' \
'Closes #480

### After merge — carried out once this pull request has merged

```text
- [ ] After merge: comment on #757 with a link, then close #757 as Resolved.
```'

check 'a ~~~ fence too' '' $'~~~\nCloses #12\n~~~'

# Written outside its fence, it is a second closing reference — which is what the
# check is there to catch before the merge closes #757 early.
check 'the same criterion outside its fence is counted' '#480 #757' \
$'Closes #480\n\n- [ ] After merge: comment on #757, then close #757 as Resolved.'

printf '\nthe check, against a gh that answers from files\n'

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
  repos/z5labs/devex)          printf '{"full_name":"z5labs/devex","default_branch":"main"}\n' | out ;;
  repos/z5labs/devex/pulls/*)
    f="$FX/pull-${path##*/}"
    [ -f "$f" ] || { printf '{"message":"Not Found"}'; printf 'gh: Not Found (HTTP 404)\n' >&2; exit 1; }
    out <"$f" ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$SCRATCH/bin/gh"

pull() { jq -n --arg b "$2" '{number: 1, body: $b}' >"$SCRATCH/fx/pull-$1"; }  # <pr> <body>

# verdict <name> <pr> <issue> <expected exit> [<substring of the message>]
verdict() {
  local out rc
  out=$(cd "$SCRATCH/repo" && PATH="$SCRATCH/bin:$PATH" FX="$SCRATCH/fx" "$SUT" "$2" "$3" 2>&1); rc=$?
  if [ "$rc" != "$4" ]; then bad "$1" "want exit $4 got $rc: $out"; return; fi
  if [ -n "${5:-}" ]; then case "$out" in *"$5"*) ;; *) bad "$1" "message lacks [$5]: $out"; return ;; esac; fi
  ok "$1" "exit $rc"
}

pull 7 $'Implements the story.\n\nCloses #480'
verdict 'exactly the story is exit 0' 7 480 0

# Written in full against this repository, in another case, it is the same issue.
pull 8 'Closes Z5labs/DevEx#480'
verdict 'this repository written out in full is the same issue' 8 480 0

pull 9 'Implements the story.'
verdict 'no closing keyword is exit 1, naming the fix' 9 480 1 "add 'Closes #480'"

pull 10 'Closes #479'
verdict 'another issue is exit 1, naming it' 10 480 1 'closes #479, not #480'

pull 11 $'Closes #480\nCloses #757'
verdict 'one issue too many is exit 1, naming both' 11 480 1 'closes #480 #757 -- more than #480'

pull 12 ''
verdict 'an empty body is exit 1' 12 480 1 'no closing keyword'

verdict 'a pull request that cannot be read is exit 4' 99 480 4 'cannot read the body of PR #99'

printf '\nGraphQL\n'
if [ -s "$GH_GUARD_LOG" ]; then
  bad 'no GraphQL-backed gh call was made' "$(tr '\n' ';' <"$GH_GUARD_LOG")"
else
  ok 'no GraphQL-backed gh call was made' 'none'
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
