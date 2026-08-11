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
#   BACKLOG_REVIEW_APP_ID   the reviewer App's numeric ID
#   BACKLOG_REVIEW_APP_KEY  its RSA private key — either the PEM text itself, or
#                           a path to a file holding it
#
# Why these are not the merge workflow's `BACKLOG_APP_*`, and why they are never
# filled in from it. The two credentials may well be one App — that is legal and
# sometimes what an operator wants — but they are not equally exposed, and the
# grant follows the exposure rather than the identity.
#
# The merge credential lives in GitHub's encrypted secret store and is read by a
# `pull_request_target` job that checks out nothing. It needs Contents, Pull
# requests and Issues at write, because it merges, deletes branches and closes
# linked issues.
#
# The reviewer credential lives in the process environment of an unattended
# agent that runs arbitrary shell — a key anything in that process can print
# with `env`. The only installation-scoped call the whole rung makes is
# `POST /repos/{owner}/{repo}/pulls/{pr}/reviews`, so what it needs is Pull
# requests: write and nothing else; `--login` and the installation lookup are
# JWT endpoints and need no installation grant at all. Handing that key Contents
# and Issues at write buys nothing and risks the default branch.
#
# So there is deliberately NO fallback from the reviewer names to the merge
# ones. A fallback would keep every existing installation working while silently
# leaving the union grant on the exposed key, with the separation written down
# nowhere — it would make the safe configuration the one you opt into. Reuse
# stays entirely legal and is expressed by exporting both pairs, which is a
# decision a reader can recover from the environment. `BACKLOG_APP_ID` and
# `BACKLOG_APP_KEY` are read here for exactly one purpose — to tell a machine
# that has never configured this rung apart from one that predates the split —
# and are never used as a credential.
#
# Two things follow beyond least privilege. Rotation becomes independent: a
# laptop key that leaks is rotated without stopping the merge workflow. And the
# timeline stops showing one bot review a pull request and then merge it, so the
# separation of duties is something a reader can see rather than something the
# `COMMENT`-only rule enforces from inside.
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
#   3  UNAVAILABLE: the rung is not configured on this machine at all, openssl
#      is missing, or the App cannot be reached or is not installed on this
#      repository. The caller treats this as a rung that is down, not as an
#      error in the work.
#   4  usage failure, or a credential that is CONFIGURED WRONG rather than
#      absent — half a pair, or the merge pair present with no reviewer pair.
#      Neither is a rung being down, and reporting either as exit 3 would let a
#      mistake and a migration both read as "this machine has no App".

set -uo pipefail

fail() { local code=$1; shift; printf 'app-token: %s\n' "$*" >&2; exit "$code"; }

[ $# -eq 1 ] || fail 4 "usage: app-token.sh --check|--login|--token"

MODE=$1
case "$MODE" in
  --check|--login|--token) ;;
  *) fail 4 "unknown argument '$MODE'; usage: app-token.sh --check|--login|--token" ;;
esac

# ----------------------------------------------------------- preconditions ----
# Every one of them names what is missing rather than that something is.
# `--check` is exactly this block and nothing else, so a caller can ask "is this
# rung available?" for the cost of a few lookups and no network at all.
#
# The credential state is read before the tool lookups, and not only because an
# environment lookup is the cheaper of the two. A misconfigured pair is exit 4
# and a missing `openssl` is exit 3, so checking openssl first would let a
# machine without it swallow the migration failure below and go on reporting the
# rung as merely down — which is precisely the silence this split exists to end.
APP_ID=${BACKLOG_REVIEW_APP_ID:-}
APP_KEY=${BACKLOG_REVIEW_APP_KEY:-}

# Half a credential. Nobody exports one of a pair on purpose, so this can only
# be a typo or a half-finished migration, and presenting it as the rung being
# unavailable would advance the roster past a reviewer somebody meant to have.
if [ -n "$APP_ID" ] && [ -z "$APP_KEY" ]; then
  fail 4 "BACKLOG_REVIEW_APP_ID is set but BACKLOG_REVIEW_APP_KEY is not; half a credential is a mistake rather than an unconfigured rung, so set the key too (the PEM text, or a path to it)"
fi
if [ -z "$APP_ID" ] && [ -n "$APP_KEY" ]; then
  fail 4 "BACKLOG_REVIEW_APP_KEY is set but BACKLOG_REVIEW_APP_ID is not; half a credential is a mistake rather than an unconfigured rung, so set the reviewer App's numeric ID too"
fi

if [ -z "$APP_ID" ]; then
  # Neither reviewer name, but the merge credential is sitting right there. That
  # is not a machine without an App, it is a machine configured before the
  # reviewer got its own credential — so it fails, once, carrying the sentence
  # that fixes it rather than quietly dropping a rung the operator still has.
  if [ -n "${BACKLOG_APP_ID:-}" ] || [ -n "${BACKLOG_APP_KEY:-}" ]; then
    fail 4 "BACKLOG_REVIEW_APP_ID and BACKLOG_REVIEW_APP_KEY are unset while the merge credential (BACKLOG_APP_ID/BACKLOG_APP_KEY) is present. The merge credential is deliberately not substituted: it grants Contents, Pull requests and Issues at write, and this key lives in the loop's process environment where the reviewer needs only Pull requests: write. Export BACKLOG_REVIEW_APP_ID and BACKLOG_REVIEW_APP_KEY — pointing them at the same App if that is what you want, which then says so in the environment instead of being inferred here"
  fi
  # Neither pair anywhere: the rung is simply not configured on this machine.
  # This one stays exit 3 and stays free, because it is the answer that lets a
  # roster of ["copilot", "local", "none"] reach `none` instead of blocking.
  fail 3 "BACKLOG_REVIEW_APP_ID and BACKLOG_REVIEW_APP_KEY are not set in the environment"
fi

case "$APP_ID" in
  ''|*[!0-9]*) fail 3 "BACKLOG_REVIEW_APP_ID must be the App's numeric ID (got '$APP_ID')" ;;
esac

command -v gh >/dev/null 2>&1      || fail 3 "gh is not on PATH"
command -v jq >/dev/null 2>&1      || fail 3 "jq is not on PATH"
command -v openssl >/dev/null 2>&1 || fail 3 "openssl is not on PATH; an App JWT is RS256-signed, so it cannot be minted without one"

[ "$MODE" = --check ] && exit 0

# ------------------------------------------------------------------ key -------
# The key arrives one of two ways and both are ordinary. A PEM pasted into an
# environment variable is what a repository secret looks like when it reaches a
# job; a path is what a laptop looks like. Guessing between them by content is
# unnecessary — a path exists on disk and a PEM does not.
KEYFILE=""
CLEANUP=""
if [ -f "$APP_KEY" ]; then
  KEYFILE=$APP_KEY
else
  case "$APP_KEY" in
    *"BEGIN"*"PRIVATE KEY"*) ;;
    *) fail 3 "BACKLOG_REVIEW_APP_KEY is neither a readable file nor a PEM private key" ;;
  esac
  KEYFILE=$(mktemp) || fail 3 "cannot create a temporary file for the App key"
  CLEANUP=$KEYFILE
  chmod 600 "$KEYFILE"
  printf '%s\n' "$APP_KEY" >"$KEYFILE"
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
PAYLOAD=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((NOW - 60))" "$((NOW + 540))" "$APP_ID")

UNSIGNED="$(printf '%s' "$HEADER" | b64url).$(printf '%s' "$PAYLOAD" | b64url)"
SIGNATURE=$(printf '%s' "$UNSIGNED" | openssl dgst -sha256 -sign "$KEYFILE" -binary 2>/dev/null | b64url) \
  || fail 3 "openssl could not sign the App JWT; check that BACKLOG_REVIEW_APP_KEY is the reviewer App's RSA private key"
[ -n "$SIGNATURE" ] || fail 3 "openssl produced no signature; check that BACKLOG_REVIEW_APP_KEY is the reviewer App's RSA private key"
JWT="$UNSIGNED.$SIGNATURE"

# `gh api -H Authorization:` rather than GH_TOKEN, because an App JWT has to be
# presented as `Bearer` and gh spells its own token `token`. gh leaves an
# Authorization header the caller supplied alone.
app_api() { gh api -H "Authorization: Bearer $JWT" "$@"; }

if [ "$MODE" = --login ]; then
  SLUG=$(app_api /app --jq .slug 2>/dev/null) || SLUG=""
  [ -n "$SLUG" ] || fail 3 "the App JWT was rejected by GET /app; check BACKLOG_REVIEW_APP_ID and BACKLOG_REVIEW_APP_KEY are the same App"
  # A bot's login is its slug plus `[bot]`, and that is the login the review
  # appears under on the timeline — which is what the review gate filters on.
  printf '%s[bot]\n' "$SLUG"
  exit 0
fi

REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) || REPO=""
[ -n "$REPO" ] || fail 3 "cannot resolve the repository slug from gh"

INSTALL_ID=$(app_api "/repos/$REPO/installation" --jq .id 2>/dev/null) || INSTALL_ID=""
case "$INSTALL_ID" in
  ''|null|*[!0-9]*) fail 3 "the reviewer App is not installed on $REPO, or its JWT was rejected; install it with Pull requests at read and write, which is all this rung calls" ;;
esac

TOKEN=$(app_api --method POST "/app/installations/$INSTALL_ID/access_tokens" --jq .token 2>/dev/null) || TOKEN=""
[ -n "$TOKEN" ] && [ "$TOKEN" != null ] \
  || fail 3 "the App installation on $REPO issued no access token"

printf '%s\n' "$TOKEN"
