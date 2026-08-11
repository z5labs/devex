#!/usr/bin/env bash
#
# app-token_test.sh — the fixture corpus for app-token.sh's four sentences.
#
# The credential states that never reach GitHub are pinned in
# `await-review_test.sh`, through all three doors that read them. This file is
# the other half: what the mint says once it does reach GitHub, where four
# different things can be wrong and no two of them share a remedy.
#
#   the key does not sign            regenerate or re-paste the .pem
#   the ID and the key are two       take both values off one App's settings
#     different Apps'                  page
#   the App is not installed here    install it on this account, or export the
#                                      other account's pair
#   the installation lacks           raise Pull requests to Read and write
#     Pull requests: write
#
# All four are exit 3, because the roster only ever needed to know that the rung
# cannot run. That is exactly why they are asserted on the MESSAGE: the exit
# code cannot tell them apart and was never meant to, so the distinction lives
# in a string, and a string with no test on it is a string that drifts. The
# case that matters most is the third — a credential naming an App installed on
# the operator's *other* account is a misconfiguration, and reads identically to
# an unconfigured rung unless the sentence says otherwise.
#
# Offline. `gh` is a stub answering from files, and the key is generated at run
# time; there is no key material in this file.
#
# Run: plugins/backlog/scripts/app-token_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/app-token.sh"
[ -x "$SUT" ] || { printf 'app-token.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

ok()  { pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' "$1" "$2"; }
bad() { fail=$((fail + 1)); printf '  FAIL %-58s %s\n' "$1" "$2"; }

if ! command -v openssl >/dev/null 2>&1; then
  printf 'openssl is not on PATH; every case here signs a JWT\n' >&2
  exit 1
fi

SCRATCH=$(mktemp -d) || { printf 'cannot create a scratch directory\n' >&2; exit 1; }
trap 'rm -rf "$SCRATCH"' EXIT
mkdir -p "$SCRATCH/bin" "$SCRATCH/fx"
FX="$SCRATCH/fx"

openssl genrsa -out "$SCRATCH/app.pem" 2048 2>/dev/null \
  || { printf 'cannot generate a test key\n' >&2; exit 1; }
# A public key signs nothing, which is the cheapest honest way to reach the
# openssl branch without shipping a corrupt PEM in a tracked file.
openssl rsa -in "$SCRATCH/app.pem" -pubout -out "$SCRATCH/app.pub" 2>/dev/null

# An empty fixture is the endpoint failing; a non-empty one is what it answers.
# `--jq` is the caller's, so the stub prints what gh would have printed after
# it — a bare value for the two `--jq` calls, and the whole body for the mint,
# which app-token.sh now parses itself so it can read the grant beside the
# token.
cat >"$SCRATCH/bin/gh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FX/calls"
case "$*" in
  *"repo view"*)   printf 'acme/widgets\n'; exit 0 ;;
  *access_tokens*) [ -s "$FX/token" ]   || exit 1; cat "$FX/token";   exit 0 ;;
  *installation*)  [ -s "$FX/install" ] || exit 1; cat "$FX/install"; exit 0 ;;
  */app*)          [ -s "$FX/app" ]     || exit 1; cat "$FX/app";     exit 0 ;;
esac
exit 1
STUB
chmod +x "$SCRATCH/bin/gh"

# Every reviewer and merge name cleared, so a case is defined by what it sets
# rather than by what the shell running the tests happened to export.
CLEAR=(env -u BACKLOG_APP_ID -u BACKLOG_APP_KEY
           -u BACKLOG_REVIEW_APP_ID -u BACKLOG_REVIEW_APP_KEY)

# The App as GitHub would answer for it. Reset before each case.
fixture() { # <slug> <installation id> <token body>
  printf '%s' "$1" >"$FX/app"
  printf '%s' "$2" >"$FX/install"
  printf '%s' "$3" >"$FX/token"
  : >"$FX/calls"
}

cred_env=(BACKLOG_REVIEW_APP_ID=123456)

OUT=""
ERR=""
RC=0
mint() { # <mode>
  ( cd "$SCRATCH" && PATH="$SCRATCH/bin:$PATH" FX="$FX" \
      exec "${CLEAR[@]}" "${cred_env[@]}" "$SUT" "$@" ) \
    >"$SCRATCH/out" 2>"$SCRATCH/err"
  RC=$?
  OUT=$(cat "$SCRATCH/out")
  ERR=$(cat "$SCRATCH/err")
}

# says <name> <expected exit> <substring the message must carry>...
says() {
  local name=$1 want=$2; shift 2
  local needle missing=""
  if [ "$RC" != "$want" ]; then
    bad "$name" "want exit $want got $RC: $(printf '%s' "$ERR$OUT" | tr '\n' ' ' | cut -c1-140)"
    return
  fi
  for needle in "$@"; do
    printf '%s' "$ERR" | grep -qF -- "$needle" || missing="$missing '$needle'"
  done
  if [ -n "$missing" ]; then
    bad "$name" "exit $want but the message never says$missing"
  else
    ok "$name" "exit $want"
  fi
}

never_says() { # <name> <substring the message must NOT carry>
  if printf '%s' "$ERR" | grep -qF -- "$2"; then
    bad "$1 — and is not the other one" "the message still says '$2'"
  else
    ok "$1 — and is not the other one" "never says '$2'"
  fi
}

calls() { grep -cE "$1" "$FX/calls" 2>/dev/null || true; }

KEY="$SCRATCH/app.pem"
GOOD_TOKEN='{"token":"ghs_fixture","permissions":{"pull_requests":"write"}}'

# ------------------------------------------------------------- it works -------
printf '\nthe mint, when everything is right\n'

cred_env=(BACKLOG_REVIEW_APP_ID=123456 "BACKLOG_REVIEW_APP_KEY=$KEY")

fixture reviewer-bot 4242 "$GOOD_TOKEN"
mint --login
if [ "$RC" = 0 ] && [ "$OUT" = 'reviewer-bot[bot]' ]; then
  ok '--login is the bot login the review lands under' "$OUT"
else
  bad '--login is the bot login the review lands under' "exit $RC, stdout '$OUT'"
fi

fixture reviewer-bot 4242 "$GOOD_TOKEN"
mint --token
if [ "$RC" = 0 ] && [ "$OUT" = ghs_fixture ]; then
  ok '--token is the token and nothing else' 'exit 0'
else
  bad '--token is the token and nothing else' "exit $RC, stdout '$OUT'"
fi
# One POST. The grant is read out of the response the mint already had, so
# asking what the installation may do must not mint a second token to discard.
if [ "$(calls access_tokens)" = 1 ]; then
  ok 'the grant check costs no second mint' 'one POST'
else
  bad 'the grant check costs no second mint' "$(calls access_tokens) POSTs"
fi

# ----------------------------------------------------- the four sentences -----
printf '\nthe four things that can be individually wrong\n'

# 1. The key does not sign. No network at all: a JWT that was never produced
#    cannot be rejected, so nothing here should reach GitHub.
fixture reviewer-bot 4242 "$GOOD_TOKEN"
cred_env=(BACKLOG_REVIEW_APP_ID=123456 "BACKLOG_REVIEW_APP_KEY=$SCRATCH/app.pub")
mint --token
says 'a key that will not sign names the key variable' 3 \
  'BACKLOG_REVIEW_APP_KEY' 'unencrypted RSA private key'
if [ "$(calls '/app|installation|access_tokens')" = 0 ]; then
  ok 'a key that will not sign costs no App call' 'no request'
else
  bad 'a key that will not sign costs no App call' "reached $(tr '\n' ' ' <"$FX/calls")"
fi

cred_env=(BACKLOG_REVIEW_APP_ID=123456 "BACKLOG_REVIEW_APP_KEY=$KEY")

# 2. The ID and the key are two different Apps'. `GET /app` is what says so, and
#    it is asked before the installation lookup precisely so this does not
#    arrive as "not installed".
fixture '' 4242 "$GOOD_TOKEN"
mint --token
says 'a rejected JWT is two Apps, not one' 3 \
  'not two halves of one App' 'BACKLOG_REVIEW_APP_ID' 'BACKLOG_REVIEW_APP_KEY'
never_says 'a rejected JWT is two Apps, not one' 'is not installed on'
if [ "$(calls installation)" = 0 ]; then
  ok 'a rejected JWT never reaches the installation lookup' 'no request'
else
  bad 'a rejected JWT never reaches the installation lookup' 'it looked anyway'
fi

fixture '' 4242 "$GOOD_TOKEN"
mint --login
says '--login answers the same way' 3 'not two halves of one App'

# 3. The App is not installed here — the multi-account failure. The sentence has
#    to name the App that was read, the account it would have to be installed
#    on, and the alternative of exporting the other account's pair, because the
#    exit code it shares with an unconfigured rung says none of that.
fixture reviewer-bot '' "$GOOD_TOKEN"
mint --token
says 'not installed names the App, the account and the pair' 3 \
  "the App 'reviewer-bot'" 'not installed on acme/widgets' \
  'on the acme account' 'review.app.idEnv'
never_says 'not installed names the App, the account and the pair' \
  'not configured on this machine'

# 4. The installation is missing the one permission the rung uses. Checked here
#    rather than left to a 403 on the review POST, which arrives after a
#    subagent has read the diff and written the review.
fixture reviewer-bot 4242 '{"token":"ghs_fixture","permissions":{"pull_requests":"read"}}'
mint --token
says 'a read-only installation says which grant it has' 3 \
  'grants Pull requests at read' 'needs write'
if [ -z "$OUT" ]; then
  ok 'a rejected grant prints no token' 'stdout empty'
else
  bad 'a rejected grant prints no token' "stdout '$OUT'"
fi

fixture reviewer-bot 4242 '{"token":"ghs_fixture","permissions":{"issues":"read"}}'
mint --token
says 'no Pull requests grant at all says that instead' 3 \
  'no Pull requests permission at all'

# ------------------------------------------------------- and what is not ------
printf '\nwhat must not be read as a failure\n'

# A response shape that moved is not an outage. The rung worked before anything
# read `permissions`, and inventing unavailability from a field that changed
# would retire a reviewer that can still review.
fixture reviewer-bot 4242 '{"token":"ghs_fixture"}'
mint --token
if [ "$RC" = 0 ] && [ "$OUT" = ghs_fixture ]; then
  ok 'a response carrying no permissions still mints' 'exit 0'
else
  bad 'a response carrying no permissions still mints' "exit $RC, stdout '$OUT'"
fi

# The counterpart to case 3, and the reason its wording had to change: both are
# exit 3, and only one of them is a mistake.
fixture reviewer-bot 4242 "$GOOD_TOKEN"
cred_env=()
mint --token
says 'no credentials at all is an unconfigured rung' 3 \
  'not configured on this machine' 'an absent credential and not a wrong one'
never_says 'no credentials at all is an unconfigured rung' 'is not installed on'

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
