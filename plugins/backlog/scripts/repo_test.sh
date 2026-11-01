#!/usr/bin/env bash
#
# repo_test.sh — the fixture corpus for repo.sh.
#
# Two halves, both offline.
#
# **Parsing.** Every case is a remote URL a clone can carry, fed to `--parse`.
# HTTPS and SSH are the two forms the cycle has to read — a laptop clone is
# commonly one and a CI or cloud checkout the other — and a remote that does not
# parse has to be exit 4 naming the URL, never a slug guessed out of it.
#
# **Resolving.** The whole script against a scratch repository and a `gh` that
# answers from files. `GET /repos/{owner}/{name}` is what confirms the parse, and
# its `full_name` is what gets printed, which is how a clone made before a rename
# resolves to the name the repository has now.
#
# Run: plugins/backlog/scripts/repo_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/repo.sh"
[ -x "$SUT" ] || { printf 'repo.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

ok()  { pass=$((pass + 1)); printf '  ok   %-62s [%s]\n' "$1" "$2"; }
bad() { fail=$((fail + 1)); printf '  FAIL %-62s %s\n' "$1" "$2"; }

# parses <name> <url> <expected owner/name>
parses() {
  local got rc
  got=$("$SUT" --parse "$2" 2>/dev/null); rc=$?
  if [ "$rc" = 0 ] && [ "$got" = "$3" ]; then ok "$1" "$got"; else bad "$1" "want [$3] exit 0, got [$got] exit $rc"; fi
}

# refuses <name> <url> <substring the message must carry> [<substring it must NOT carry>]
refuses() {
  local err rc
  err=$("$SUT" --parse "$2" 2>&1 >/dev/null); rc=$?
  if [ "$rc" != 4 ]; then bad "$1" "want exit 4, got $rc: $err"; return; fi
  case "$err" in *"$3"*) ;; *) bad "$1" "message lacks [$3]: $err"; return ;; esac
  if [ -n "${4:-}" ]; then case "$err" in *"$4"*) bad "$1" "message still carries [$4]: $err"; return ;; esac; fi
  ok "$1" 'exit 4'
}

printf '\nHTTPS\n'

parses 'https, no suffix'                  https://github.com/z5labs/devex            z5labs/devex
parses 'https, with .git'                  https://github.com/z5labs/devex.git        z5labs/devex
parses 'https, trailing slash'             https://github.com/z5labs/devex/           z5labs/devex
parses 'https, with a user'                https://Zaba505@github.com/z5labs/devex.git z5labs/devex
parses 'https, with a token in user-info'  https://x-access-token:ghs_abc@github.com/z5labs/devex.git z5labs/devex
parses 'http through a local git proxy'    http://local_proxy@127.0.0.1:43123/git/z5labs/devex z5labs/devex
parses 'a name with dots and underscores'  https://github.com/z5labs/my_repo.v2.git   z5labs/my_repo.v2
parses 'case is kept as written'           https://github.com/Z5Labs/DevEx            Z5Labs/DevEx

printf '\nSSH\n'

parses 'scp-like'                          git@github.com:z5labs/devex.git            z5labs/devex
parses 'scp-like, no suffix'               git@github.com:z5labs/devex                z5labs/devex
parses 'scp-like, no user'                 github.com:z5labs/devex.git                z5labs/devex
parses 'ssh://'                            ssh://git@github.com/z5labs/devex.git      z5labs/devex
parses 'ssh:// with a port'                ssh://git@ssh.github.com:443/z5labs/devex.git z5labs/devex
parses 'a host alias from ~/.ssh/config'   github-work:z5labs/devex.git               z5labs/devex

printf '\nnot a GitHub remote — exit 4, naming the URL\n'

refuses 'a local path'                     /srv/git/z5labs/devex       "'/srv/git/z5labs/devex'"
refuses 'a relative path'                  ../devex                    "'../devex'"
refuses 'a file:// URL'                    file:///srv/git/z5labs/devex "'file:///srv/git/z5labs/devex'"
refuses 'a host with no path'              https://github.com          "'https://github.com'"
refuses 'an owner with no repository'      https://github.com/z5labs   "'https://github.com/z5labs'"
refuses 'a bare word'                      devex                       "'devex'"
refuses 'a name that is only dots'         https://github.com/z5labs/..  "'https://github.com/z5labs/..'"
refuses 'an owner with a space'            'https://github.com/z5 labs/devex' "z5 labs"
# The URL is named, but a credential in it is not: a CI checkout carries a token
# in its remote, and a failure must not print it into a transcript.
refuses 'a token in user-info is masked in the message' \
  'https://x-access-token:ghs_secret@github.com/z5labs' '***@github.com' 'ghs_secret'

printf '\nresolving\n'

SCRATCH=$(mktemp -d) || { printf 'cannot create a scratch directory\n' >&2; exit 1; }
trap 'rm -rf "$SCRATCH"' EXIT
mkdir -p "$SCRATCH/bin" "$SCRATCH/repos"
export GH_GUARD="$HERE/testdata/gh-guard.sh"
export GH_GUARD_LOG="$SCRATCH/graphql-calls"
: >"$GH_GUARD_LOG"

# `gh api repos/<owner>/<name>` answers from $STUB_REPOS/<owner>_<name>, with the
# `--jq` really run, and an absent fixture is GitHub's 404. Every call is logged.
cat >"$SCRATCH/bin/gh" <<'STUB'
#!/usr/bin/env bash
. "$GH_GUARD"
printf '%s\n' "$*" >>"$STUB_CALLS"
q="" path="" prev=""
for a in "$@"; do [ "$prev" = --jq ] && q=$a; case "$a" in repos/*) path=$a ;; esac; prev=$a; done
[ "$1" = api ] && [ -n "$path" ] || { printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1; }
f="$STUB_REPOS/$(printf '%s' "${path#repos/}" | tr / _)"
[ -f "$f" ] || { printf '{"message":"Not Found"}'; printf 'gh: Not Found (HTTP 404)\n' >&2; exit 1; }
if [ -n "$q" ]; then jq -r "$q" "$f"; else cat "$f"; fi
STUB
chmod +x "$SCRATCH/bin/gh"

printf '{"full_name":"z5labs/devex","default_branch":"main"}\n' >"$SCRATCH/repos/z5labs_devex"
# A repository renamed since the clone: GitHub answers the old name with the new
# repository, so the old name in the remote is what is looked up and the new one
# is what comes back.
printf '{"full_name":"z5labs/devx","default_branch":"trunk"}\n' >"$SCRATCH/repos/z5labs_old-devex"

# resolve <remote url, or - for none> — runs the script in a fresh checkout
resolve() {
  rm -rf "$SCRATCH/repo"
  git init -q "$SCRATCH/repo" >/dev/null 2>&1
  [ "$1" = - ] || git -C "$SCRATCH/repo" remote add origin "$1"
  : >"$SCRATCH/calls"
  OUT=$(cd "$SCRATCH/repo" && PATH="$SCRATCH/bin:$PATH" STUB_REPOS="$SCRATCH/repos" STUB_CALLS="$SCRATCH/calls" \
    "$SUT" 2>"$SCRATCH/err")
  RC=$?
  ERR=$(tr '\n' ' ' <"$SCRATCH/err")
}

# resolves <name> <remote> <expected stdout, tab as \t>
resolves() {
  resolve "$2"
  local want
  want=$(printf "$3")
  if [ "$RC" = 0 ] && [ "$OUT" = "$want" ]; then ok "$1" "$(printf '%s' "$OUT" | tr '\t' ' ')"
  else bad "$1" "want [$(printf '%s' "$want" | tr '\t' ' ')] exit 0, got [$(printf '%s' "$OUT" | tr '\t' ' ')] exit $RC: $ERR"; fi
}

# resolve_fails <name> <remote|-> <substring>
resolve_fails() {
  resolve "$2"
  if [ "$RC" != 4 ]; then bad "$1" "want exit 4, got $RC: $ERR"; return; fi
  case "$ERR" in *"$3"*) ok "$1" 'exit 4' ;; *) bad "$1" "message lacks [$3]: $ERR" ;; esac
}

resolves 'an HTTPS remote resolves to the slug and default branch' \
  https://github.com/z5labs/devex 'z5labs/devex\tmain'
resolves 'an SSH remote resolves the same' \
  git@github.com:z5labs/devex.git 'z5labs/devex\tmain'
resolves 'a renamed repository resolves to its new name' \
  https://github.com/z5labs/old-devex.git 'z5labs/devx\ttrunk'

resolve https://github.com/z5labs/devex
if grep -qx 'api repos/z5labs/devex --jq .*' "$SCRATCH/calls" && [ "$(grep -c . "$SCRATCH/calls")" = 1 ]; then
  ok 'one REST call, against the parsed slug' 'GET repos/z5labs/devex'
else
  bad 'one REST call, against the parsed slug' "$(tr '\n' ';' <"$SCRATCH/calls")"
fi

resolve_fails 'no origin remote' - "no 'origin' remote"
resolve_fails 'an origin that does not parse names the URL' /srv/git/devex "'/srv/git/devex'"
# Confirmed, not trusted: a slug that parses and names nothing on GitHub is a
# failure carrying GitHub's own reason, not a slug printed anyway.
resolve_fails 'a repository GitHub cannot read carries its reason' \
  https://github.com/z5labs/nowhere 'Not Found (HTTP 404)'

rm -rf "$SCRATCH/repo"; mkdir -p "$SCRATCH/repo"
OUT=$(cd "$SCRATCH/repo" && PATH="$SCRATCH/bin:$PATH" "$SUT" 2>&1); RC=$?
case "$RC:$OUT" in
  4:*'not inside a git repository'*) ok 'outside a git checkout' 'exit 4' ;;
  *) bad 'outside a git checkout' "want exit 4, got $RC: $OUT" ;;
esac

printf '\nGraphQL\n'
if [ -s "$GH_GUARD_LOG" ]; then
  bad 'no GraphQL-backed gh call was made' "$(tr '\n' ';' <"$GH_GUARD_LOG")"
else
  ok 'no GraphQL-backed gh call was made' 'none'
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
