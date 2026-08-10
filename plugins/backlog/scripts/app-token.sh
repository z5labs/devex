#!/usr/bin/env bash
#
# app-token.sh — mint a GitHub App installation token, and say plainly when it
# cannot be minted rather than failing somewhere inside a pipeline.
#
# Usage: app-token.sh --check     preconditions only; makes no network call
#        app-token.sh --login     the App's bot login, `<slug>[bot]`
#        app-token.sh --token     an installation token for this repository
#
# Environment:
#   BACKLOG_APP_ID   the App's numeric ID
#   BACKLOG_APP_KEY  its RSA private key — either the PEM text itself, or a
#                    path to a file holding it
#
# The names match the repository secrets `backlog:setup-backlog` provisions for
# the merge workflow, deliberately: it is the *same* App. What differs is
# delivery — the workflow reads them as repository secrets, and the `local`
# reviewer needs them in its own environment, on the laptop or in the job that
# runs the loop.
#
# Why an App and not the operator's own token. The pull request under review was
# opened by that token, so a review posted with it reads on the timeline as the
# author reviewing their own work — which is not a review, however good the
# comments are. A personal token also pins the reviewer to one workstation,
# where an App installation is reachable from anywhere its two environment
# variables are.
#
# Why `openssl` is checked rather than assumed. An App JWT is RS256, so minting
# one is a signature and not a string operation, and `openssl` is a third
# dependency for scripts that until now needed only `gh` and `jq`. A missing
# `openssl` surfacing as an empty signature and a 401 is the failure this check
# exists to convert into a sentence.
#
# Exit codes:
#   0  fine — for --check, the preconditions hold; otherwise stdout is the answer
#   3  UNAVAILABLE: a credential is absent, openssl is missing, or the App
#      cannot be reached or is not installed on this repository. The caller
#      treats this as a rung that is down, not as an error in the work.
#   4  usage or precondition failure in the script's own arguments

set -uo pipefail

fail() { local code=$1; shift; printf 'app-token: %s\n' "$*" >&2; exit "$code"; }

[ $# -eq 1 ] || fail 4 "usage: app-token.sh --check|--login|--token"

MODE=$1
case "$MODE" in
  --check|--login|--token) ;;
  *) fail 4 "unknown argument '$MODE'; usage: app-token.sh --check|--login|--token" ;;
esac

# ----------------------------------------------------------- preconditions ----
# Ordered cheapest first, and every one of them names what is missing rather
# than that something is. `--check` is exactly this block and nothing else, so a
# caller can ask "is this rung available?" for the cost of three lookups and no
# network at all.
command -v gh >/dev/null 2>&1      || fail 3 "gh is not on PATH"
command -v jq >/dev/null 2>&1      || fail 3 "jq is not on PATH"
command -v openssl >/dev/null 2>&1 || fail 3 "openssl is not on PATH; an App JWT is RS256-signed, so it cannot be minted without one"

[ -n "${BACKLOG_APP_ID:-}" ]  || fail 3 "BACKLOG_APP_ID is not set in the environment"
[ -n "${BACKLOG_APP_KEY:-}" ] || fail 3 "BACKLOG_APP_KEY is not set in the environment"

case "$BACKLOG_APP_ID" in
  ''|*[!0-9]*) fail 3 "BACKLOG_APP_ID must be the App's numeric ID (got '$BACKLOG_APP_ID')" ;;
esac

[ "$MODE" = --check ] && exit 0

# ------------------------------------------------------------------ key -------
# The key arrives one of two ways and both are ordinary. A PEM pasted into an
# environment variable is what a repository secret looks like when it reaches a
# job; a path is what a laptop looks like. Guessing between them by content is
# unnecessary — a path exists on disk and a PEM does not.
KEYFILE=""
CLEANUP=""
if [ -f "$BACKLOG_APP_KEY" ]; then
  KEYFILE=$BACKLOG_APP_KEY
else
  case "$BACKLOG_APP_KEY" in
    *"BEGIN"*"PRIVATE KEY"*) ;;
    *) fail 3 "BACKLOG_APP_KEY is neither a readable file nor a PEM private key" ;;
  esac
  KEYFILE=$(mktemp) || fail 3 "cannot create a temporary file for the App key"
  CLEANUP=$KEYFILE
  chmod 600 "$KEYFILE"
  printf '%s\n' "$BACKLOG_APP_KEY" >"$KEYFILE"
fi
# shellcheck disable=SC2064
[ -n "$CLEANUP" ] && trap "rm -f '$CLEANUP'" EXIT

# ------------------------------------------------------------------ jwt -------
# base64url, which is base64 with two characters swapped and the padding
# dropped. `openssl base64 -A` rather than `base64 -w0`, because the latter is a
# GNU spelling and this runs on macOS too.
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

NOW=$(date +%s)
# 60s of backdating absorbs clock skew between here and GitHub, which rejects a
# JWT issued in its future outright. Ten minutes is the maximum lifetime GitHub
# accepts; nine is under it with room for the same skew at the other end.
HEADER='{"alg":"RS256","typ":"JWT"}'
PAYLOAD=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((NOW - 60))" "$((NOW + 540))" "$BACKLOG_APP_ID")

UNSIGNED="$(printf '%s' "$HEADER" | b64url).$(printf '%s' "$PAYLOAD" | b64url)"
SIGNATURE=$(printf '%s' "$UNSIGNED" | openssl dgst -sha256 -sign "$KEYFILE" -binary 2>/dev/null | b64url) \
  || fail 3 "openssl could not sign the App JWT; check that BACKLOG_APP_KEY is the App's RSA private key"
[ -n "$SIGNATURE" ] || fail 3 "openssl produced no signature; check that BACKLOG_APP_KEY is the App's RSA private key"
JWT="$UNSIGNED.$SIGNATURE"

# `gh api -H Authorization:` rather than GH_TOKEN, because an App JWT has to be
# presented as `Bearer` and gh spells its own token `token`. gh leaves an
# Authorization header the caller supplied alone.
app_api() { gh api -H "Authorization: Bearer $JWT" "$@"; }

if [ "$MODE" = --login ]; then
  SLUG=$(app_api /app --jq .slug 2>/dev/null) || SLUG=""
  [ -n "$SLUG" ] || fail 3 "the App JWT was rejected by GET /app; check BACKLOG_APP_ID and BACKLOG_APP_KEY are the same App"
  # A bot's login is its slug plus `[bot]`, and that is the login the review
  # appears under on the timeline — which is what the review gate filters on.
  printf '%s[bot]\n' "$SLUG"
  exit 0
fi

REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) || REPO=""
[ -n "$REPO" ] || fail 3 "cannot resolve the repository slug from gh"

INSTALL_ID=$(app_api "/repos/$REPO/installation" --jq .id 2>/dev/null) || INSTALL_ID=""
case "$INSTALL_ID" in
  ''|null|*[!0-9]*) fail 3 "the App is not installed on $REPO, or its JWT was rejected; install it with Contents, Pull requests and Issues at read and write" ;;
esac

TOKEN=$(app_api --method POST "/app/installations/$INSTALL_ID/access_tokens" --jq .token 2>/dev/null) || TOKEN=""
[ -n "$TOKEN" ] && [ "$TOKEN" != null ] \
  || fail 3 "the App installation on $REPO issued no access token"

printf '%s\n' "$TOKEN"
