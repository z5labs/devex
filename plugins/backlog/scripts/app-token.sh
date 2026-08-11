#!/usr/bin/env bash
#
# app-token.sh — mint a GitHub App installation token, and say plainly when it
# cannot be minted rather than failing somewhere inside a pipeline.
#
# Usage: app-token.sh [--id-env <name>] [--key-env <name>] --check
#        app-token.sh [--id-env <name>] [--key-env <name>] --login
#        app-token.sh [--id-env <name>] [--key-env <name>] --token
#
#        --check   preconditions only; makes no network call
#        --login   the App's bot login, `<slug>[bot]`
#        --token   an installation token for this repository
#
# Environment, by default:
#   BACKLOG_REVIEW_APP_ID   the reviewer App's numeric ID
#   BACKLOG_REVIEW_APP_KEY  its RSA private key — either the PEM text itself, or
#                           a path to a file holding it
#
# Why the two names are an argument rather than a constant. A GitHub App is
# installed per account, not globally, so an operator who owns repositories in
# more than one account owns more than one reviewer App — while one process
# environment holds one pair of variables. Hardcoding the names meant those Apps
# could not be exported side by side: working a repository in the other account
# began with re-exporting the pair from memory, and forgetting was not loud.
# `GET /repos/{owner}/{repo}/installation` answers "the App is not installed on
# $REPO", which is exit 3, which is UNAVAILABLE, which the roster reads as a rung
# that is down — so the wrong account's App is indistinguishable from an outage,
# on the path whose end is a merge with no review at all.
#
# So each repository names its own pair in `.claude/backlog.json` as
# `review.app.idEnv`/`review.app.keyEnv`, its caller passes them here, and every
# account's credentials are exported once and stay exported. The names are
# NAMES, not values — nothing secret reaches a tracked file.
#
# The merge side has no such problem and is deliberately untouched:
# `assets/auto-merge.yaml` reads `secrets.BACKLOG_APP_ID`, and repository secrets
# are already scoped per repository, so one name resolves to a different App in
# each account with no collision to resolve.
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
#      absent — half a pair of names, half a pair of values, or the merge pair
#      present with no reviewer pair. None of the three is a rung being down, and
#      reporting any of them as exit 3 would let a mistake and a migration both
#      read as "this machine has no App".

set -uo pipefail

fail() { local code=$1; shift; printf 'app-token: %s\n' "$*" >&2; exit "$code"; }

USAGE='usage: app-token.sh [--id-env <name>] [--key-env <name>] --check|--login|--token'

# The names this falls back to, and the ones `assets/backlog.schema.json`
# documents as the default of `review.app.idEnv`/`review.app.keyEnv`. A caller
# that names neither gets exactly the behaviour this script had before the pair
# became configurable, which is what keeps a repository with no `review.app`
# block working untouched.
ID_ENV=BACKLOG_REVIEW_APP_ID
KEY_ENV=BACKLOG_REVIEW_APP_KEY
ID_ENV_SET=0
KEY_ENV_SET=0
MODE=""

# A legal POSIX environment variable name, checked before the name is used to
# read one. `${!name}` on anything else is a bash "bad substitution", which
# would surface this as a crash inside a pipeline rather than as the sentence
# that says which name is wrong — and the whole reason the names are
# configurable is that a wrong one must never be mistaken for a rung being down.
check_env_name() { # <flag> <name>
  case "$2" in
    '') fail 4 "$1 needs a non-empty variable name" ;;
    [!A-Za-z_]*|*[!A-Za-z0-9_]*)
      fail 4 "$1 names '$2', which is not a legal environment variable name; a name is a letter or underscore followed by letters, digits or underscores" ;;
  esac
}

while [ $# -gt 0 ]; do
  case "$1" in
    --id-env|--key-env)
      [ $# -ge 2 ] || fail 4 "$1 needs a value; $USAGE"
      check_env_name "$1" "$2"
      if [ "$1" = --id-env ]; then ID_ENV=$2; ID_ENV_SET=1; else KEY_ENV=$2; KEY_ENV_SET=1; fi
      shift 2 ;;
    --id-env=*|--key-env=*)
      check_env_name "${1%%=*}" "${1#*=}"
      if [ "${1%%=*}" = --id-env ]; then ID_ENV=${1#*=}; ID_ENV_SET=1; else KEY_ENV=${1#*=}; KEY_ENV_SET=1; fi
      shift ;;
    --check|--login|--token)
      [ -z "$MODE" ] || fail 4 "$MODE and $1 are two modes; ask for one. $USAGE"
      MODE=$1
      shift ;;
    *)
      fail 4 "unknown argument '$1'; $USAGE" ;;
  esac
done

[ -n "$MODE" ] || fail 4 "$USAGE"

# Half a pair of NAMES, which is the same mistake as half a pair of values one
# step earlier: a configured id beside a defaulted key reads one App's ID
# against another App's private key, and the JWT that produces is rejected by
# GitHub with nothing in the message pointing at the mismatch. So the pair moves
# together or not at all.
if [ "$ID_ENV_SET" -eq 1 ] && [ "$KEY_ENV_SET" -eq 0 ]; then
  fail 4 "--id-env names '$ID_ENV' but --key-env was not given, so the key would still be read from $KEY_ENV -- one App's ID against another App's key. Pass both, or neither"
fi
if [ "$KEY_ENV_SET" -eq 1 ] && [ "$ID_ENV_SET" -eq 0 ]; then
  fail 4 "--key-env names '$KEY_ENV' but --id-env was not given, so the ID would still be read from $ID_ENV -- one App's key against another App's ID. Pass both, or neither"
fi

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
#
# Read indirectly, through the names the caller gave. Every message below names
# the variable that was actually read and never the default: an operator whose
# config says `Z5LABS_REVIEW_APP_ID` and who is told `BACKLOG_REVIEW_APP_ID is
# not set` has been sent to look at the wrong thing, and will export the wrong
# variable to fix it.
APP_ID=${!ID_ENV:-}
APP_KEY=${!KEY_ENV:-}

# Half a credential. Nobody exports one of a pair on purpose, so this can only
# be a typo or a half-finished migration, and presenting it as the rung being
# unavailable would advance the roster past a reviewer somebody meant to have.
if [ -n "$APP_ID" ] && [ -z "$APP_KEY" ]; then
  fail 4 "$ID_ENV is set but $KEY_ENV is not; half a credential is a mistake rather than an unconfigured rung, so set the key too (the PEM text, or a path to it)"
fi
if [ -z "$APP_ID" ] && [ -n "$APP_KEY" ]; then
  fail 4 "$KEY_ENV is set but $ID_ENV is not; half a credential is a mistake rather than an unconfigured rung, so set the reviewer App's numeric ID too"
fi

if [ -z "$APP_ID" ]; then
  # Neither reviewer name, but the merge credential is sitting right there. That
  # is not a machine without an App, it is a machine configured before the
  # reviewer got its own credential — so it fails, once, carrying the sentence
  # that fixes it rather than quietly dropping a rung the operator still has.
  if [ -n "${BACKLOG_APP_ID:-}" ] || [ -n "${BACKLOG_APP_KEY:-}" ]; then
    fail 4 "$ID_ENV and $KEY_ENV are unset while the merge credential (BACKLOG_APP_ID/BACKLOG_APP_KEY) is present. The merge credential is deliberately not substituted: it grants Contents, Pull requests and Issues at write, and this key lives in the loop's process environment where the reviewer needs only Pull requests: write. Export $ID_ENV and $KEY_ENV — pointing them at the same App if that is what you want, which then says so in the environment instead of being inferred here"
  fi
  # Neither pair anywhere: the rung is simply not configured on this machine.
  # This one stays exit 3 and stays free, because it is the answer that lets a
  # roster of ["copilot", "local", "none"] reach `none` instead of blocking.
  fail 3 "$ID_ENV and $KEY_ENV are not set in the environment"
fi

case "$APP_ID" in
  ''|*[!0-9]*) fail 3 "$ID_ENV must be the App's numeric ID (got '$APP_ID')" ;;
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
    *) fail 3 "$KEY_ENV is neither a readable file nor a PEM private key" ;;
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
  || fail 3 "openssl could not sign the App JWT; check that $KEY_ENV is the reviewer App's RSA private key"
[ -n "$SIGNATURE" ] || fail 3 "openssl produced no signature; check that $KEY_ENV is the reviewer App's RSA private key"
JWT="$UNSIGNED.$SIGNATURE"

# `gh api -H Authorization:` rather than GH_TOKEN, because an App JWT has to be
# presented as `Bearer` and gh spells its own token `token`. gh leaves an
# Authorization header the caller supplied alone.
app_api() { gh api -H "Authorization: Bearer $JWT" "$@"; }

if [ "$MODE" = --login ]; then
  SLUG=$(app_api /app --jq .slug 2>/dev/null) || SLUG=""
  [ -n "$SLUG" ] || fail 3 "the App JWT was rejected by GET /app; check $ID_ENV and $KEY_ENV are the same App"
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
