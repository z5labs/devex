#!/usr/bin/env bash
#
# await-review.sh — request a review from one rung of the roster, wait for it,
# and say what landed.
#
# Usage: await-review.sh <pr-number> [<reviewer>]
#        await-review.sh --classify <reviewer>   (stdin: a review as JSON)
#
# <reviewer> is `copilot` or `local`, defaulting to `copilot`. `none` is not a
# reviewer to wait on — a roster that reaches it merges unreviewed, which is a
# decision the cycle makes rather than a wait it performs.
#
# Why this is a script and not three numbered steps.
#
# "Did the reviewer review this?" looks like one question and is two, and the
# cycle used to answer them in places far enough apart that the second could be
# skipped. The wait was a polling loop that exited as soon as a `reviewed` event
# appeared; whether that review had DECLINED the work — "Copilot wasn't able to
# review this pull request because it exceeds the maximum number of files (300)"
# — was a separate command sixty lines further down, under its own heading, that
# an agent had to remember to run. It has already been missed once in the wild:
# a vendored test suite pushed a pull request past the file limit, Copilot
# declined, the naive `length > 0` check passed, and the cycle merged with no
# review at all.
#
# The merge label is the cycle's assertion that a review completed. That
# assertion should be an exit code, not a thing remembered at the end of the
# longest part of the run.
#
# Findings folded in here, each of which cost a debugging session:
#
#   * Copilot is a Bot, not a User. `reviewers[]=Copilot` on the REST endpoint
#     returns 200 and does nothing; the GraphQL mutation takes `botIds`, and the
#     REST form works only with the full `...[bot]` login.
#   * The wait polls the TIMELINE, not `pulls/<pr>/reviews`. The reviews
#     endpoint has been seen empty for forty minutes after Copilot had in fact
#     submitted, so polling it times out on a pull request that was reviewed.
#   * `--paginate`, because the timeline returns thirty events per page and a
#     pull request with a few pushes and check runs pushes the `reviewed` event
#     off page one — where an unpaginated read reports nothing.
#   * The login filter, because a `reviewed` event from anyone would otherwise
#     end the wait. The repository owner glancing at the pull request while the
#     reviewer was still working would satisfy an unfiltered loop.
#
# What the reviewer argument changed, and what it did not. Nothing about the
# shape above is Copilot-specific — request, wait on the timeline, classify what
# landed — so the login, the request and the decline wording are per-rung data
# and the rest is shared. The `local` rung has no request step at all: its
# review is produced by a subagent and POSTed by `post-review.sh` before this
# script is called, so here it is a wait and a classification and nothing else.
#
# UNAVAILABILITY versus REFUSAL, which is the distinction the roster turns on.
# A rung that reviewed the pull request and refused it has said something true
# about the work, and advancing past it to a reviewer with different limits is
# how unreviewable work reaches the default branch. So a refusal is exit 1 and
# stops the cycle. Only a rung that cannot review at all — the request refused,
# credentials absent, nothing posted inside the bound — advances the roster.
#
# Exit codes:
#   0  a review COMPLETED — it left comments, or reported that it generated none
#   1  the most recent review REFUSED the work; stdout is its body. BLOCKED:
#      do not advance the roster and do not label the pull request
#   2  nothing arrived yet. Not a verdict — call it again
#   3  UNAVAILABLE, synchronously: the rung refused the request, or its
#      preconditions are absent. Advance the roster, and remember this rung for
#      the rest of the run — it told us it was down, in a second or two
#   4  usage or precondition failure in the script's own arguments

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
MINT="$HERE/app-token.sh"

# Five minutes per call, deliberately: the caller waits by BLOCKING on this
# script, so the bound has to fit inside a single foreground call rather than
# span turns. A reviewer routinely takes longer than that; exit 2 is the answer,
# and the caller runs it again in the same turn.
POLL_COUNT=${BACKLOG_REVIEW_POLL_COUNT:-20}
POLL_SECONDS=${BACKLOG_REVIEW_POLL_SECONDS:-15}

fail() { local code=$1; shift; printf 'await-review: %s\n' "$*" >&2; exit "$code"; }
note() { printf 'await-review: %s\n' "$*" >&2; }

# ------------------------------------------------------------ the rungs -------
# Per-rung data, and only the data. `REVIEWER_MATCH` is a case-insensitive
# regular expression against the `reviewed` event's login, never an equality
# test: Copilot has posted under more than one spelling, and matching loosely
# on a name no human account uses is safer than matching exactly on one of them.
REVIEWER=""
REVIEWER_LOGIN=""
REVIEWER_MATCH=""

resolve_reviewer() { # <reviewer>
  case "$1" in
    copilot)
      REVIEWER=copilot
      REVIEWER_LOGIN='copilot-pull-request-reviewer[bot]'
      REVIEWER_MATCH='copilot'
      ;;
    local)
      REVIEWER=local
      # The local rung IS the App, so its login has to come from the App rather
      # than from a constant here. That lookup needs the credentials and
      # openssl, which makes "the App is not configured in this environment"
      # exit 3 before any waiting happens — a fallthrough, not a crash, and at
      # the cost of one API call rather than five minutes.
      REVIEWER_LOGIN=$("$MINT" --login) || exit $?
      [ -n "$REVIEWER_LOGIN" ] || fail 3 "the backlog App reported no login; the local rung cannot be waited on"
      REVIEWER_MATCH=$(printf '%s' "$REVIEWER_LOGIN" | sed 's/[][\\.^$*+?(){}|]/\\&/g')
      ;;
    none)
      fail 4 "'none' is not a reviewer to wait on; a roster that reaches none merges unreviewed"
      ;;
    *)
      fail 4 "unknown reviewer '$1'; the rungs are copilot and local"
      ;;
  esac
}

# A refusal satisfies any `length > 0` test, which is exactly how it was missed.
# The apostrophe is matched loosely because GitHub has rendered it both as ' and
# as a typographic quote.
#
# The wording is Copilot's, and it is matched for every rung on purpose: the
# `local` reviewer is told to use the same sentence when it declines, so that
# one classifier covers the roster. That is also why a generic `bot:<login>`
# rung is deliberately out of scope — an unrecognised refusal from an unknown
# bot would classify as a completed review and merge.
refused() {
  printf '%s' "$1" | grep -qiE "was(n.?t| not) able to review"
}

classify() { # <review json>
  local state body
  state=$(printf '%s' "$1" | jq -r '.state // ""' 2>/dev/null)
  body=$(printf '%s' "$1" | jq -r '.body // ""' 2>/dev/null)
  if refused "$body"; then
    printf '%s\n' "$body"
    fail 1 "$REVIEWER REFUSED to review PR #$PR; do not label it, and do not advance the roster"
  fi
  printf '%s review completed [%s]\n%s\n' "$REVIEWER" "$state" "$body"
  exit 0
}

# The classification seam, so the rules above are pinned by fixtures rather than
# by a live pull request. Everything else in this script is polling and a bound.
if [ "${1:-}" = --classify ]; then
  [ $# -eq 2 ] || fail 4 "usage: await-review.sh --classify <reviewer>  (a review as JSON on stdin)"
  command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"
  PR=0
  case "$2" in
    copilot) REVIEWER=copilot ;;
    local)   REVIEWER=local ;;
    none)    fail 4 "'none' is not a reviewer to wait on; a roster that reaches none merges unreviewed" ;;
    *)       fail 4 "unknown reviewer '$2'; the rungs are copilot and local" ;;
  esac
  INPUT=$(cat)
  [ -n "$INPUT" ] || fail 2 "nothing to classify"
  classify "$INPUT"
fi

[ $# -ge 1 ] || fail 4 "usage: await-review.sh <pr-number> [<reviewer>]"
[ $# -le 2 ] || fail 4 "usage: await-review.sh <pr-number> [<reviewer>]"
PR=$1
case "$PR" in ''|*[!0-9]*) fail 4 "pull request number must be an integer (got '$PR')" ;; esac

command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"
command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) \
  || fail 4 "cannot resolve the repository slug from gh"
[ -n "$REPO" ] || fail 4 "cannot resolve the repository slug from gh"

resolve_reviewer "${2:-copilot}"

# --------------------------------------------------------------- reading ------
# The most recent review by this rung, as {state, body}, or nothing.
#
# `jq -s` slurps the per-page objects back into one array before sorting.
# Sorting inside --jq would sort each page separately and yield the last review
# OF THE LAST PAGE rather than the last review.
#
# The login filter lives in the second jq rather than in gh's `--jq`, because
# `gh api --jq` takes no `--arg` and the filter is now per-rung data. Splicing a
# login into the filter text would put `backlog-bot[bot]`'s brackets inside a jq
# string literal, where `\[` is not a legal escape and the whole expression
# fails — silently, as "no review yet", which is the one failure mode a wait
# cannot tell from a slow reviewer.
latest_review() {
  gh api --paginate "repos/$REPO/issues/$PR/timeline" \
    --jq '.[] | select(.event=="reviewed")' \
    2>/dev/null \
  | jq -s --arg match "$REVIEWER_MATCH" \
      'map(select(.user.login // "" | test($match;"i")))
       | sort_by(.submitted_at) | last // empty
       | {state: (.state // ""), body: (.body // "")}' \
    2>/dev/null
}

EXISTING=$(latest_review)
if [ -n "$EXISTING" ]; then
  note "a $REVIEWER review is already on PR #$PR; not requesting another"
  classify "$EXISTING"
fi

# ------------------------------------------------------------- requesting -----
# Only the rungs that have someone to ask. The local reviewer posts its own
# review through post-review.sh before this script is reached, so there is
# nothing to request and a request step would have nothing to fail on.
requested() {
  gh api "repos/$REPO/pulls/$PR/requested_reviewers" \
    --jq '[.users[]?.login] + [.teams[]?.slug] | join(" ")' 2>/dev/null \
  | grep -qiE "$REVIEWER_MATCH"
}

# The bot's node ID is looked up by login rather than hard-coded, so a change on
# GitHub's side surfaces here as a failed lookup instead of a silent no-op.
request_review() {
  local pr_id bot_id
  pr_id=$(gh pr view "$PR" --repo "$REPO" --json id --jq .id 2>/dev/null) || pr_id=""
  bot_id=$(gh api "/users/$REVIEWER_LOGIN" --jq .node_id 2>/dev/null) || bot_id=""

  if [ -n "$pr_id" ] && [ -n "$bot_id" ]; then
    if gh api graphql -f query='
      mutation($pr:ID!, $bot:ID!) {
        requestReviews(input: {pullRequestId: $pr, botIds: [$bot], union: true}) {
          pullRequest { reviewRequests(first:10) { nodes {
            requestedReviewer { __typename ... on Bot { login } } } } }
        }
      }' -f pr="$pr_id" -f bot="$bot_id" 2>/dev/null | grep -qiE "$REVIEWER_MATCH"; then
      note "review requested from $REVIEWER via the GraphQL requestReviews mutation"
      return 0
    fi
    note "the GraphQL mutation did not take; falling back to REST"
  fi

  # REST works only with the full bot login. A bare `reviewers[]=Copilot`
  # returns 200 while doing nothing at all, and the wait below would then time
  # out on a review that was never requested.
  gh api --method POST "repos/$REPO/pulls/$PR/requested_reviewers" \
    -f "reviewers[]=$REVIEWER_LOGIN" >/dev/null 2>&1 || true

  requested
}

if [ "$REVIEWER" = local ]; then
  note "the local rung posts its own review; nothing to request on PR #$PR"
elif requested; then
  note "$REVIEWER is already a requested reviewer on PR #$PR"
elif request_review; then
  note "$REVIEWER is now a requested reviewer on PR #$PR"
else
  fail 3 "could not request a $REVIEWER review on PR #$PR; Copilot code review may not be enabled for this organisation. This rung is unavailable: advance the roster and remember it"
fi

# ---------------------------------------------------------------- waiting -----
for _ in $(seq 1 "$POLL_COUNT"); do
  sleep "$POLL_SECONDS"
  REVIEW=$(latest_review)
  [ -n "$REVIEW" ] && classify "$REVIEW"
done

fail 2 "no $REVIEWER review on PR #$PR after $((POLL_COUNT * POLL_SECONDS))s; re-run to keep waiting. Silence is not unavailability: a slow reviewer looks exactly like this, so do not retire this rung on it"
