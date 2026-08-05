#!/usr/bin/env bash
#
# await-review.sh — request Copilot's review, wait for it, and say what landed.
#
# Usage: await-review.sh <pr-number>
#
# Why this is a script and not three numbered steps.
#
# "Did Copilot review this?" looks like one question and is two, and the cycle
# used to answer them in places far enough apart that the second could be
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
# Four findings are folded in here, each of which cost a debugging session:
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
#     end the wait. The repository owner glancing at the pull request while
#     Copilot was still working would satisfy an unfiltered loop.
#
# Idempotent: a Copilot review that has already landed is classified and
# returned immediately, without requesting another. Re-running after a timeout
# resumes the wait.
#
# Exit codes:
#   0  a review completed — it left comments, or reported that it generated none
#   1  the most recent Copilot review DECLINED the work; stdout is its body
#   2  timed out waiting for a review
#   3  the review could not be requested (Copilot code review not enabled here)
#   4  usage or precondition failure

set -uo pipefail

# Five minutes per call, deliberately: the caller waits by BLOCKING on this
# script, so the bound has to fit inside a single foreground call rather than
# span turns. Copilot routinely takes longer than that; exit 2 is the answer,
# and the caller runs it again in the same turn.
POLL_COUNT=${BACKLOG_REVIEW_POLL_COUNT:-20}
POLL_SECONDS=${BACKLOG_REVIEW_POLL_SECONDS:-15}
BOT_LOGIN='copilot-pull-request-reviewer[bot]'

fail() { local code=$1; shift; printf 'await-review: %s\n' "$*" >&2; exit "$code"; }
note() { printf 'await-review: %s\n' "$*" >&2; }

[ $# -eq 1 ] || fail 4 "usage: await-review.sh <pr-number>"
PR=$1
case "$PR" in ''|*[!0-9]*) fail 4 "pull request number must be an integer (got '$PR')" ;; esac

command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"
command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) \
  || fail 4 "cannot resolve the repository slug from gh"
[ -n "$REPO" ] || fail 4 "cannot resolve the repository slug from gh"

# --------------------------------------------------------------- reading ------
# The most recent Copilot review, as {state, body}, or nothing.
#
# `jq -s` slurps the per-page objects back into one array before sorting.
# Sorting inside --jq would sort each page separately and yield the last review
# OF THE LAST PAGE rather than the last review.
latest_review() {
  gh api --paginate "repos/$REPO/issues/$PR/timeline" \
    --jq '.[] | select(.event=="reviewed") | select(.user.login | test("copilot";"i"))' \
    2>/dev/null \
  | jq -s 'sort_by(.submitted_at) | last // empty | {state: (.state // ""), body: (.body // "")}' \
    2>/dev/null
}

# A decline satisfies any `length > 0` test, which is exactly how it was missed.
# The apostrophe is matched loosely because GitHub has rendered it both as ' and
# as a typographic quote.
declined() {
  printf '%s' "$1" | grep -qiE "was(n.?t| not) able to review"
}

classify() { # <review json>
  local state body
  state=$(printf '%s' "$1" | jq -r '.state // ""')
  body=$(printf '%s' "$1" | jq -r '.body // ""')
  if declined "$body"; then
    printf '%s\n' "$body"
    fail 1 "Copilot DECLINED to review PR #$PR; do not label it"
  fi
  printf 'copilot review completed [%s]\n%s\n' "$state" "$body"
  exit 0
}

EXISTING=$(latest_review)
if [ -n "$EXISTING" ]; then
  note "a Copilot review is already on PR #$PR; not requesting another"
  classify "$EXISTING"
fi

# ------------------------------------------------------------- requesting -----
# The bot's node ID is looked up by login rather than hard-coded, so a change on
# GitHub's side surfaces here as a failed lookup instead of a silent no-op.
requested() {
  gh api "repos/$REPO/pulls/$PR/requested_reviewers" \
    --jq '[.users[]?.login] + [.teams[]?.slug] | join(" ")' 2>/dev/null \
  | grep -qi copilot
}

request_review() {
  local pr_id bot_id
  pr_id=$(gh pr view "$PR" --repo "$REPO" --json id --jq .id 2>/dev/null) || pr_id=""
  bot_id=$(gh api "/users/$BOT_LOGIN" --jq .node_id 2>/dev/null) || bot_id=""

  if [ -n "$pr_id" ] && [ -n "$bot_id" ]; then
    if gh api graphql -f query='
      mutation($pr:ID!, $bot:ID!) {
        requestReviews(input: {pullRequestId: $pr, botIds: [$bot], union: true}) {
          pullRequest { reviewRequests(first:10) { nodes {
            requestedReviewer { __typename ... on Bot { login } } } } }
        }
      }' -f pr="$pr_id" -f bot="$bot_id" 2>/dev/null | grep -qi copilot; then
      note "review requested via the GraphQL requestReviews mutation"
      return 0
    fi
    note "the GraphQL mutation did not take; falling back to REST"
  fi

  # REST works only with the full bot login. A bare `reviewers[]=Copilot`
  # returns 200 while doing nothing at all, and the wait below would then time
  # out on a review that was never requested.
  gh api --method POST "repos/$REPO/pulls/$PR/requested_reviewers" \
    -f "reviewers[]=$BOT_LOGIN" >/dev/null 2>&1 || true

  requested
}

if requested; then
  note "Copilot is already a requested reviewer on PR #$PR"
elif request_review; then
  note "Copilot is now a requested reviewer on PR #$PR"
else
  fail 3 "could not request a Copilot review on PR #$PR; Copilot code review may not be enabled for this organisation"
fi

# ---------------------------------------------------------------- waiting -----
for _ in $(seq 1 "$POLL_COUNT"); do
  sleep "$POLL_SECONDS"
  REVIEW=$(latest_review)
  [ -n "$REVIEW" ] && classify "$REVIEW"
done

fail 2 "no Copilot review on PR #$PR after $((POLL_COUNT * POLL_SECONDS))s; re-run to keep waiting"
