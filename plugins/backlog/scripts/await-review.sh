#!/usr/bin/env bash
#
# await-review.sh — request a review from one rung of the roster, wait for it,
# and say what landed.
#
# Usage: await-review.sh <pr-number> [<reviewer>]
#        await-review.sh --classify <reviewer>   (stdin: a review as JSON)
#
# <reviewer> is `copilot`, `local` or `bot:<login>`, defaulting to `copilot`.
# `none` is not a reviewer to wait on — a roster that reaches it merges
# unreviewed, which is a decision the cycle makes rather than a wait it
# performs.
#
# `copilot` is sugar for `bot:copilot-pull-request-reviewer[bot]`, and writing
# the desugared form gets identical behaviour: that login is the one bot this
# plugin KNOWS, so it carries a built-in timeline filter and a built-in refusal
# wording. Every other login is a bot the plugin has never seen.
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
# the login not a bot or not installed, credentials absent, nothing posted
# inside the bound — advances the roster.
#
# And a third thing a landed review can be: UNCLASSIFIABLE. A refusal is
# recognised by its WORDING, and the only wording this plugin has ever observed
# is Copilot's. A different bot refuses in words of its own, which match nothing
# here — so a naive classifier would return its refusal as a completed review
# and the cycle would label and merge on it. That is the same defect the decline
# check was added for, reintroduced for every bot the plugin has not been
# taught. So a review from a bot whose refusal wording is unknown is never
# exit 0: it is exit 5, and a human reads it. An operator who has run that bot
# and seen it decline supplies the wording in `.claude/backlog.json` as
# `review.refusals["bot:<login>"]`, and that rung then classifies exactly as
# copilot does.
#
# Exit codes:
#   0  a review COMPLETED — it left comments, or reported that it generated none
#   1  the most recent review REFUSED the work; stdout is its body. BLOCKED:
#      do not advance the roster and do not label the pull request
#   2  nothing arrived yet. Not a verdict — call it again
#   3  UNAVAILABLE, synchronously: the request could not be placed, the login is
#      not a bot or is not installed on the repository, or the rung's
#      preconditions are absent. Advance the roster, and remember this rung for
#      the rest of the run — it told us it was down, in a second or two.
#      Distinct from exit 1, which is the rung REFUSING the work
#   4  usage or precondition failure in the script's own arguments — or, for the
#      `local` rung, a reviewer credential that is configured wrong rather than
#      absent, returned by the mint. That is an environment to fix and not a
#      rung that is down, so it does not advance the roster
#   5  ESCALATION: a review landed from a rung whose refusal wording this plugin
#      does not know, so it may be a refusal and the script cannot tell. stdout
#      is the review body. The caller treats it exactly as exit 1 — do not
#      label, do not advance the roster, stop and report — and it is a separate
#      code because the run has learned something different: not "the bot
#      refused" but "the bot may have refused and the plugin cannot tell". A
#      report that conflates the two teaches its reader a refusal that never
#      happened

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
REVIEWER_KIND=""
REVIEWER_LOGIN=""
REVIEWER_MATCH=""
REVIEWER_REFUSAL=""

# The one bot this plugin knows, and the one refusal wording it has ever
# observed. Both are built in, and neither is configurable: `copilot` is not a
# login the operator chose, and its wording is not something they have to have
# seen for the gate to work.
COPILOT_LOGIN='copilot-pull-request-reviewer[bot]'
# A refusal satisfies any `length > 0` test, which is exactly how it was missed.
# The apostrophe is matched loosely because GitHub has rendered it both as ' and
# as a typographic quote.
COPILOT_REFUSAL="was(n.?t| not) able to review"

# Read cwd-relative, exactly as select-issue.sh reads it, and optional: absent
# or unparseable means no rung carries a configured wording, which is the safe
# direction — an unknown bot escalates rather than completing.
CFG=.claude/backlog.json

regex_escape() { printf '%s' "$1" | sed 's/[][\\.^$*+?(){}|]/\\&/g'; }

# Anything but a non-empty string reads as "not configured", which escalates.
# select-issue.sh has already refused those shapes at step 1; treating them as
# absent here rather than as a pattern is the same choice made twice, in the
# direction that cannot merge.
configured_refusal() { # <rung>
  [ -f "$CFG" ] || return 0
  jq -r --arg rung "$1" \
    '.review.refusals[$rung] | if type == "string" then . else "" end' \
    "$CFG" 2>/dev/null
}

# Everything classification needs, and nothing else: no network call, no App
# token, no cwd beyond the config. `--classify` calls this alone, which is what
# keeps the fixture corpus offline.
#
# The refusal wording is Copilot's, and it is matched for the `local` rung on
# purpose: that reviewer is told to decline in the same sentence, so one
# classifier covers both rungs the plugin ships. A `bot:<login>` rung gets its
# wording from the operator or gets none, and none means escalation.
resolve_refusal() { # <reviewer>
  case "$1" in
    copilot)
      REVIEWER=copilot;  REVIEWER_REFUSAL=$COPILOT_REFUSAL ;;
    local)
      REVIEWER=local;    REVIEWER_REFUSAL=$COPILOT_REFUSAL ;;
    "bot:$COPILOT_LOGIN")
      REVIEWER="$1";     REVIEWER_REFUSAL=$COPILOT_REFUSAL ;;
    bot:?*)
      REVIEWER="$1";     REVIEWER_REFUSAL=$(configured_refusal "$1") ;;
    none)
      fail 4 "'none' is not a reviewer to wait on; a roster that reaches none merges unreviewed" ;;
    *)
      fail 4 "unknown reviewer '$1'; the rungs are copilot, local and bot:<login>" ;;
  esac
}

# The rest of the per-rung data: who to ask, and whose `reviewed` events count.
resolve_reviewer() { # <reviewer>
  resolve_refusal "$1"
  case "$1" in
    local)
      REVIEWER_KIND=local
      # The local rung IS the App, so its login has to come from the App rather
      # than from a constant here. That lookup needs the reviewer credentials
      # and openssl, which makes "the App is not configured in this
      # environment" exit 3 before any waiting happens — a fallthrough, not a
      # crash, and at the cost of one API call rather than five minutes. A
      # credential that is present but configured wrong comes back as exit 4
      # through the same line, and is deliberately not the same answer.
      REVIEWER_LOGIN=$("$MINT" --login) || exit $?
      [ -n "$REVIEWER_LOGIN" ] || fail 3 "the backlog App reported no login; the local rung cannot be waited on"
      REVIEWER_MATCH=$(regex_escape "$REVIEWER_LOGIN")
      ;;
    *)
      REVIEWER_KIND=bot
      case "$1" in
        copilot) REVIEWER_LOGIN=$COPILOT_LOGIN ;;
        *)       REVIEWER_LOGIN=${1#bot:} ;;
      esac
      if [ "$REVIEWER_LOGIN" = "$COPILOT_LOGIN" ]; then
        REVIEWER_MATCH='copilot'
      else
        # The `[bot]` suffix is dropped before escaping rather than matched,
        # because the timeline has carried both spellings for the same bot and
        # the match is an unanchored substring test. So `bot:coderabbitai[bot]`
        # and `bot:coderabbitai` filter identically, and only the login sent to
        # GitHub — which REST accepts in the full form only — differs.
        REVIEWER_MATCH=$(regex_escape "${REVIEWER_LOGIN%"[bot]"}")
        [ -n "$REVIEWER_MATCH" ] || REVIEWER_MATCH=$(regex_escape "$REVIEWER_LOGIN")
      fi
      ;;
  esac
}

classify() { # <review json>
  local state body rc
  state=$(printf '%s' "$1" | jq -r '.state // ""' 2>/dev/null)
  body=$(printf '%s' "$1" | jq -r '.body // ""' 2>/dev/null)

  # A bot whose refusal wording is unknown. Silence here would be a merge: its
  # decline matches nothing, falls through to the success path and is returned
  # as a completed review.
  if [ -z "$REVIEWER_REFUSAL" ]; then
    printf '%s\n' "$body"
    fail 5 "$REVIEWER reviewed PR #$PR, and this plugin does not know how that bot words a refusal -- so it cannot tell a completed review from a declined one. Read the review above: do not label the pull request, and do not advance the roster. Once you have seen this bot decline, put its wording in $CFG as review.refusals[\"$REVIEWER\"] and it classifies exactly as copilot does"
  fi

  # `--` because the pattern is operator-supplied: one that opens with a dash
  # is parsed as options by GNU grep, which answers 2 and never tests the body
  # at all. A pattern that cannot run is a refusal nobody looked for.
  printf '%s' "$body" | grep -qiE -- "$REVIEWER_REFUSAL"
  rc=$?
  case "$rc" in
    0)
      printf '%s\n' "$body"
      fail 1 "$REVIEWER REFUSED to review PR #$PR; do not label it, and do not advance the roster" ;;
    1) ;;
    # grep answers 2 for a pattern it cannot compile, and a broken pattern read
    # as "did not match" is a refusal classified as a completed review. Never
    # let that fall through: select-issue.sh rejects an unusable pattern at
    # step 1, so reaching here means the config changed under the run.
    *)
      fail 4 "review.refusals[\"$REVIEWER\"] in $CFG is not a usable POSIX extended regular expression; nothing can be classified against it" ;;
  esac

  printf '%s review completed [%s]\n%s\n' "$REVIEWER" "$state" "$body"
  exit 0
}

# The classification seam, so the rules above are pinned by fixtures rather than
# by a live pull request. Everything else in this script is polling and a bound.
if [ "${1:-}" = --classify ]; then
  [ $# -eq 2 ] || fail 4 "usage: await-review.sh --classify <reviewer>  (a review as JSON on stdin)"
  command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"
  PR=0
  resolve_refusal "$2"
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
  | grep -qiE -- "$REVIEWER_MATCH"
}

# The bot's node ID is looked up by login rather than hard-coded, so a change on
# GitHub's side surfaces here as a failed lookup instead of a silent no-op. The
# account TYPE comes back from the same call, because `requestReviews` takes
# `botIds` and a login that is not a Bot cannot be passed to it — while the REST
# fallback would happily request a REVIEW FROM A PERSON under that login. So a
# login that resolves to anything but a Bot is unavailable before the fallback
# is reached. A lookup that fails outright says nothing about the type and is
# left to the fallback, exactly as it was.
request_review() {
  local pr_id account acct_type bot_id
  pr_id=$(gh pr view "$PR" --repo "$REPO" --json id --jq .id 2>/dev/null) || pr_id=""
  account=$(gh api "/users/$REVIEWER_LOGIN" --jq '"\(.type // "")|\(.node_id // "")"' 2>/dev/null) || account=""
  acct_type=${account%%|*}
  bot_id=${account#*|}

  if [ -n "$acct_type" ] && [ "$acct_type" != Bot ]; then
    fail 3 "$REVIEWER_LOGIN is a $acct_type on GitHub, not a Bot, so no review can be requested from it as one. This rung is unavailable: advance the roster and remember it"
  fi

  if [ -n "$pr_id" ] && [ -n "$bot_id" ]; then
    if gh api graphql -f query='
      mutation($pr:ID!, $bot:ID!) {
        requestReviews(input: {pullRequestId: $pr, botIds: [$bot], union: true}) {
          pullRequest { reviewRequests(first:10) { nodes {
            requestedReviewer { __typename ... on Bot { login } } } } }
        }
      }' -f pr="$pr_id" -f bot="$bot_id" 2>/dev/null | grep -qiE -- "$REVIEWER_MATCH"; then
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

if [ "$REVIEWER_KIND" = local ]; then
  note "the local rung posts its own review; nothing to request on PR #$PR"
elif requested; then
  note "$REVIEWER is already a requested reviewer on PR #$PR"
elif request_review; then
  note "$REVIEWER is now a requested reviewer on PR #$PR"
else
  fail 3 "could not request a $REVIEWER review on PR #$PR; $REVIEWER_LOGIN may not be installed on this repository, and for copilot the organisation may not have code review enabled. This rung is unavailable: advance the roster and remember it"
fi

# ---------------------------------------------------------------- waiting -----
for _ in $(seq 1 "$POLL_COUNT"); do
  sleep "$POLL_SECONDS"
  REVIEW=$(latest_review)
  [ -n "$REVIEW" ] && classify "$REVIEW"
done

fail 2 "no $REVIEWER review on PR #$PR after $((POLL_COUNT * POLL_SECONDS))s; re-run to keep waiting. Silence is not unavailability: a slow reviewer looks exactly like this, so do not retire this rung on it"
