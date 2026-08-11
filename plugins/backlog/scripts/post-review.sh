#!/usr/bin/env bash
#
# post-review.sh — post a review to a pull request under the backlog App's
# identity, so that a review produced locally lands where every other review
# lands.
#
# Usage: post-review.sh [--id-env <name>] [--key-env <name>] --preflight
#        post-review.sh [--id-env <name>] [--key-env <name>] <pr-number> <body-file> [<comments-file>]
#
# `--preflight` answers "can this rung run at all?" with no network call and no
# subagent spawned. That ordering is the point: the `local` reviewer costs a
# fresh agent and a whole diff, and finding out afterwards that the credentials
# were never there is the expensive way to learn it.
#
# `--id-env` and `--key-env` name the environment variables the reviewer App's
# credentials are read from, and are handed straight to `app-token.sh` — which
# is the only thing here that reads a credential. They come from the caller,
# which read `review.app` out of `.claude/backlog.json` once at step 0; this
# script deliberately does not read that file a second time. Two readers of one
# key can disagree, and the way they would disagree here is one of them minting
# against an App the other never checked.
#
# <body-file> holds the review's summary, as markdown. <comments-file>, when
# given, holds a JSON array of inline comments in the shape the reviews endpoint
# takes — `[{"path":"a/b.go","line":42,"side":"RIGHT","body":"…"}]`.
#
# Why the review is POSTed rather than kept in the run's transcript.
#
#   * Visibility. A review that exists only in an agent's context cannot be read
#     by anyone else with repository access, and cannot be read later.
#   * Uniformity. A posted review is a `reviewed` event on the pull request
#     timeline, which is exactly what `await-review.sh` polls. One gate then
#     covers every rung of the roster, and "was this reviewed?" stays an exit
#     code rather than something an agent asserts at the end of the longest part
#     of the cycle.
#
# The review is always a `COMMENT`, never `APPROVE` or `REQUEST_CHANGES`. An
# App approving the pull request the loop just opened would satisfy a
# required-review protection rule, which would let the loop manufacture its own
# approval; `COMMENT` says what it found and leaves the gate where it was.
#
# Exit codes:
#   0  the review was posted (or, for --preflight, the rung can run)
#   1  the review could not be posted for a reason that is not availability —
#      a rejected payload, a transient GitHub failure. This pull request goes
#      without this rung; the rung itself is NOT remembered as down.
#   3  UNAVAILABLE: the reviewer App is not configured here, openssl is missing,
#      or the App is not installed on this repository. The caller advances the
#      roster and remembers the rung for the run.
#   4  usage failure, or a reviewer credential that is configured wrong rather
#      than absent — half the reviewer pair exported, or an installation that
#      still exports only the merge App's names. Both come straight back from
#      the mint, naming the variables this run was told to read. This is NOT a
#      rung that is down: it is an environment to fix, and treating it as
#      unavailability is how a reviewer somebody configured on purpose gets
#      skipped in silence.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
MINT="$HERE/app-token.sh"

fail() { local code=$1; shift; printf 'post-review: %s\n' "$*" >&2; exit "$code"; }
note() { printf 'post-review: %s\n' "$*" >&2; }

USAGE='usage: post-review.sh [--id-env <name>] [--key-env <name>] --preflight
       post-review.sh [--id-env <name>] [--key-env <name>] <pr-number> <body-file> [<comments-file>]'

[ $# -ge 1 ] || fail 4 "$USAGE"
[ -x "$MINT" ] || fail 4 "app-token.sh is not executable at $MINT"

# The credential names are forwarded verbatim and are not interpreted here. What
# a name has to look like, and what it means for one to be missing, is
# app-token.sh's rule; restating it here would be the same rule in two places,
# free to drift in the direction that mints against the wrong App. The one thing
# checked here is that the pair arrived together, because on a rung that never
# reaches the mint nothing else would ever look.
MINT_ARGS=()
ID_ENV_SET=0
KEY_ENV_SET=0
PREFLIGHT=0
ARGS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --id-env|--key-env)
      [ $# -ge 2 ] || fail 4 "$1 needs a value; $USAGE"
      MINT_ARGS[${#MINT_ARGS[@]}]=$1
      MINT_ARGS[${#MINT_ARGS[@]}]=$2
      if [ "$1" = --id-env ]; then ID_ENV_SET=1; else KEY_ENV_SET=1; fi
      shift 2 ;;
    --id-env=*|--key-env=*)
      MINT_ARGS[${#MINT_ARGS[@]}]=$1
      if [ "${1%%=*}" = --id-env ]; then ID_ENV_SET=1; else KEY_ENV_SET=1; fi
      shift ;;
    --preflight)
      PREFLIGHT=1
      shift ;;
    --*)
      fail 4 "unknown argument '$1'; $USAGE" ;;
    *)
      ARGS[${#ARGS[@]}]=$1
      shift ;;
  esac
done

[ "$ID_ENV_SET" -eq "$KEY_ENV_SET" ] \
  || fail 4 "--id-env and --key-env name one App's two halves, so they are passed together or not at all; pass both, or neither"

# The branch is not decoration. `"${arr[@]}"` on an EMPTY array under `set -u`
# is an unbound variable on bash 3.2, which is what macOS ships — so the
# ordinary invocation, the one that passes no names at all, is exactly the one
# that would die there.
mint() { # <mode>
  if [ "${#MINT_ARGS[@]}" -gt 0 ]; then "$MINT" "${MINT_ARGS[@]}" "$1"; else "$MINT" "$1"; fi
}

if [ "$PREFLIGHT" -eq 1 ]; then
  [ "${#ARGS[@]}" -eq 0 ] || fail 4 "--preflight takes no other argument"
  mint --check || exit $?
  note "the reviewer App credentials and openssl are present; the local reviewer can run"
  exit 0
fi

[ "${#ARGS[@]}" -ge 2 ] || fail 4 "$USAGE"
[ "${#ARGS[@]}" -le 3 ] || fail 4 "$USAGE"

PR=${ARGS[0]}
BODY_FILE=${ARGS[1]}
COMMENTS_FILE=${ARGS[2]:-}

case "$PR" in ''|*[!0-9]*) fail 4 "pull request number must be an integer (got '$PR')" ;; esac
[ -f "$BODY_FILE" ] || fail 4 "no review body at $BODY_FILE"
[ -s "$BODY_FILE" ] || fail 4 "the review body at $BODY_FILE is empty; a review with nothing to say still has to say so"

command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"
command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) || REPO=""
[ -n "$REPO" ] || fail 4 "cannot resolve the repository slug from gh"

if [ -n "$COMMENTS_FILE" ]; then
  [ -f "$COMMENTS_FILE" ] || fail 4 "no inline comments at $COMMENTS_FILE"
  jq -e 'type == "array"' "$COMMENTS_FILE" >/dev/null 2>&1 \
    || fail 4 "$COMMENTS_FILE must hold a JSON array of inline comments"
fi

TOKEN=$(mint --token) || exit $?
[ -n "$TOKEN" ] || fail 3 "the App issued no installation token"

PAYLOAD=$(mktemp) || fail 1 "cannot create a temporary file for the review payload"
trap 'rm -f "$PAYLOAD" "$PAYLOAD.err"' EXIT

build_payload() { # <with-comments: 1|0>
  if [ "$1" = 1 ] && [ -n "$COMMENTS_FILE" ]; then
    jq -n --rawfile body "$BODY_FILE" --slurpfile comments "$COMMENTS_FILE" \
      '{event: "COMMENT", body: $body, comments: $comments[0]}' >"$PAYLOAD"
  else
    jq -n --rawfile body "$BODY_FILE" '{event: "COMMENT", body: $body}' >"$PAYLOAD"
  fi
}

post() {
  GH_TOKEN=$TOKEN GITHUB_TOKEN=$TOKEN \
    gh api --method POST "repos/$REPO/pulls/$PR/reviews" --input "$PAYLOAD" --jq .id
}

build_payload 1
if ID=$(post 2>"$PAYLOAD.err"); then
  printf '%s\n' "$ID"
  note "posted a COMMENT review on PR #$PR as the backlog App"
  exit 0
fi

# The whole review is rejected when any one inline comment does not land on a
# line the diff actually changed, and the reviewer subagent is working from a
# unified diff rather than from the API's position model — so this is the
# expected failure, not an exceptional one. Losing every finding because one
# line number was off by a hunk is the outcome worth avoiding; the summary
# carries the findings either way.
if [ -n "$COMMENTS_FILE" ]; then
  note "the review was rejected with its inline comments ($(tr -d '\n' <"$PAYLOAD.err" | cut -c1-200)); retrying with the summary alone"
  build_payload 0
  if ID=$(post 2>"$PAYLOAD.err"); then
    printf '%s\n' "$ID"
    note "posted a COMMENT review on PR #$PR as the backlog App, without its inline comments"
    exit 0
  fi
fi

fail 1 "could not post a review on PR #$PR: $(tr -d '\n' <"$PAYLOAD.err" | cut -c1-300)"
