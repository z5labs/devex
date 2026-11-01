#!/usr/bin/env bash
#
# await-review.sh — request a review from one rung of the roster, wait for it,
# and say what landed.
#
# Usage: await-review.sh [--id-env <name>] [--key-env <name>] <pr-number> [<reviewer>]
#        await-review.sh --classify <reviewer>   (stdin: a review as JSON)
#
# <reviewer> is `copilot`, `local` or `bot:<login>`, defaulting to `copilot`.
# `none` is not a reviewer to wait on — a roster that reaches it merges
# unreviewed, which is a decision the cycle makes rather than a wait it
# performs.
#
# `copilot` is sugar for `bot:copilot-pull-request-reviewer[bot]`, and writing
# the desugared form gets identical behaviour: that login is the one bot this
# plugin KNOWS, so it carries a built-in timeline filter, a built-in refusal
# wording and a built-in check run to read its verdict from. Every other login
# is a bot the plugin has never seen.
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
#     returns 200 and does nothing; the REST form works only with the full
#     `...[bot]` login, which is the only form this script ever sends.
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
# `--id-env` and `--key-env` name the environment variables the reviewer App's
# credentials come from, and matter to the `local` rung alone — it is the only
# one whose login has to be asked of the App rather than written down. They are
# handed to `app-token.sh` untouched. They come from the caller, which read
# `review.app` out of `.claude/backlog.json` once at step 0: this script already
# reads that file for `review.refusals`, and reading the credential names from
# it as well would let the rung wait on a login minted from one App while
# `post-review.sh` posted under another.
#
# What the reviewer argument changed, and what it did not. Nothing about the
# shape above is Copilot-specific — request, wait on the timeline, classify what
# landed — so the login, the request and the decline wording are per-rung data
# and the rest is shared. The `local` rung has no request step at all: its
# review is produced by a subagent and POSTed by `post-review.sh` before this
# script is called, so here it is a wait and a classification and nothing else.
#
# Every GitHub call here is REST under `repos/{owner}/{repo}/...`, which is what
# a Claude Code cloud session's GitHub proxy serves. The request used to go
# through GraphQL's `requestReviews` first, after looking the bot up at
# `GET /users/{login}` for its node ID and its account type; the proxy refuses
# GraphQL, and refuses `users/...` as outside the session's repository. So the
# request is the REST one alone, and whether a login is a Bot is read from the
# login itself: every Bot's login ends `[bot]`, and a person's login cannot hold
# a bracket at all. That is checked before anything is read or requested, so a
# login without it can never reach the request as a person — which the account
# lookup used to guard against, and which a failed lookup let through.
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
# And a fourth, for Copilot alone: ERRORED. Copilot answers a run that crashed
# with a review too — "Copilot encountered an error and was unable to review
# this pull request" — and that body matched neither the refusal wording nor
# anything else, so it was returned as a completed review: four pull requests in
# a row on z5labs/humus (#747-#750) exited 0 on it. Widening the refusal pattern
# would be the wrong fix twice over. An error says nothing about the work, so it
# must not stop the cycle the way a refusal does; and the wording is Copilot's
# to change — its ordinary review body has already changed shape once, from
# "## Pull request overview" in August 2026 to `<!-- ccr-overview-v2 -->` in
# October — so every new phrasing would be another merge on a review that never
# happened.
#
# So Copilot's verdict is read from STRUCTURE rather than prose. Copilot code
# review runs as a GitHub Actions workflow, and each run leaves a check run
# named `copilot-pull-request-reviewer` on the commit it reviewed: `success`
# beside every real review observed, `failure` beside every error review, and
# completed within seconds of the review it produced. The review is matched to
# the run that produced it by commit and by time, and that run's conclusion is
# the verdict. An error is retried once — the error body itself invites it, and
# on devex #366 a real review followed an error on the same commit — and an
# error that survives the retry is exit 6: unavailable FOR THIS PULL REQUEST,
# which advances the roster without retiring Copilot for the run. The refusal
# wording is still matched first, because a refusal must stop the cycle however
# its run concluded.
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
#      happened. For copilot it is also what a review with NO
#      `copilot-pull-request-reviewer` check run on its commit gets: the
#      structure the verdict is read from is missing — renamed, or never
#      created — and guessing from the body is the defect the check replaced
#   6  ERRORED, copilot only: Copilot's own run failed on this commit, and
#      failed again after one re-request. stdout is the review body. Advance the
#      roster, and do NOT remember the rung: it is unavailable for this pull
#      request, not for the run — on the run that found #747-#750, Copilot
#      reviewed the pull requests either side of them normally

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
# 1 when the rung is Copilot under either spelling, which is the only rung whose
# verdict has a check run to be read from.
REVIEWER_COPILOT=0
# What the last classification left undecided: `pending` (wait on) or `retry`
# (re-request, then wait on). Empty after anything that exits.
VERDICT=""

# The one bot this plugin knows, and the one refusal wording it has ever
# observed. Both are built in, and neither is configurable: `copilot` is not a
# login the operator chose, and its wording is not something they have to have
# seen for the gate to work.
COPILOT_LOGIN='copilot-pull-request-reviewer[bot]'
# A refusal satisfies any `length > 0` test, which is exactly how it was missed.
# The apostrophe is matched loosely because GitHub has rendered it both as ' and
# as a typographic quote — and up to three of any character, not one, because
# the typographic quote is three bytes of UTF-8 and grep counts bytes under the
# C or POSIX locale. A Claude Code cloud session runs in exactly that locale, and
# `.?` there let "wasn’t able to review" through as a completed review.
COPILOT_REFUSAL="was(n.{0,3}t| not) able to review"
# The check run Copilot's Actions workflow leaves on the commit it reviewed.
# Its name is not a documented contract either, but a rename surfaces as "no
# check run" — exit 5, a human reads it — where a reworded body slipped through
# as exit 0.
COPILOT_CHECK=copilot-pull-request-reviewer
# How far apart a run's completion and its review may land and still be one
# event. Observed: the review 2s before to 4s after. Wide enough for a slow
# GitHub, narrow enough that an earlier run on the same commit — the review
# before a re-request — is never mistaken for this review's.
COPILOT_SLACK_SECONDS=120

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
      REVIEWER=copilot;  REVIEWER_REFUSAL=$COPILOT_REFUSAL; REVIEWER_COPILOT=1 ;;
    local)
      REVIEWER=local;    REVIEWER_REFUSAL=$COPILOT_REFUSAL ;;
    "bot:$COPILOT_LOGIN")
      REVIEWER="$1";     REVIEWER_REFUSAL=$COPILOT_REFUSAL; REVIEWER_COPILOT=1 ;;
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
      REVIEWER_LOGIN=$(mint --login) || exit $?
      [ -n "$REVIEWER_LOGIN" ] || fail 3 "the backlog App reported no login; the local rung cannot be waited on"
      REVIEWER_MATCH=$(regex_escape "$REVIEWER_LOGIN")
      ;;
    *)
      REVIEWER_KIND=bot
      case "$1" in
        copilot) REVIEWER_LOGIN=$COPILOT_LOGIN ;;
        *)       REVIEWER_LOGIN=${1#bot:} ;;
      esac
      # A Bot's login ends `[bot]`; a person's GitHub login is letters, digits
      # and hyphens, and cannot hold a bracket. So a login without the suffix is
      # not a Bot, and this rung is unavailable before anything is read or
      # requested — a REST request under that login would ask a PERSON for the
      # review the roster says a bot gives. Unavailable rather than a usage
      # error, as a login that resolved to a person always was.
      case "$REVIEWER_LOGIN" in
        ?*'[bot]') ;;
        *) fail 3 "'$REVIEWER_LOGIN' is not a Bot's login -- a Bot's login ends in [bot], and a person's cannot hold a bracket -- so no review can be requested from it as a bot. This rung is unavailable: advance the roster and remember it. If it is a bot, name it in review.reviewers as bot:$REVIEWER_LOGIN[bot]" ;;
      esac
      if [ "$REVIEWER_LOGIN" = "$COPILOT_LOGIN" ]; then
        REVIEWER_MATCH='copilot'
      else
        # The `[bot]` suffix is dropped before escaping rather than matched,
        # because the timeline has carried both spellings for the same bot and
        # the match is an unanchored substring test. The login sent to GitHub
        # keeps it: REST accepts a bot in the full form only.
        REVIEWER_MATCH=$(regex_escape "${REVIEWER_LOGIN%"[bot]"}")
      fi
      ;;
  esac
}

# What Copilot's own runs say about the review in front of us. Input is the
# review with `.checks` — every `copilot-pull-request-reviewer` check run on the
# review's commit, as {status, conclusion, started_at, completed_at} — and the
# answer is one word:
#
#   unknown    no check run at all, or no timestamp to match one by
#   pending    a run is still going, or a run newer than this review's has
#              succeeded and its review has not reached the timeline yet (the
#              check has been seen completing 2s BEFORE its review)
#   completed  the run that produced this review succeeded
#   retry      it failed, or no run produced it at all (devex #366: the error
#              review posted before any run started), and this commit has had
#              at most one run — so no re-request has been tried yet
#   errored    the same, after a re-request
#
# "The run that produced it" is the run that started no later than the review
# and completed nearest it, within COPILOT_SLACK_SECONDS. Both conditions are
# load bearing: without the first, a re-request's run (started after an error
# review) would vouch for the error; without the second, the run behind an
# earlier review on the same commit would.
#
# The retry is counted in check runs rather than remembered, so it is the same
# answer on every re-entry of this script and across workers.
copilot_verdict() { # <review json with .checks>
  printf '%s' "$1" | jq -r --argjson slack "$COPILOT_SLACK_SECONDS" '
    def ts: if . == null or . == "" then null
            else sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 end;
    def abs: if . < 0 then -. else . end;
    (.submitted_at | ts) as $t
    | (.checks // []) as $runs
    | if ($runs | length) == 0 or $t == null then "unknown"
      elif any($runs[]; .status != "completed") then "pending"
      else
        ([ $runs[]
           | select((.started_at | ts) != null and (.completed_at | ts) != null)
           | select((.started_at | ts) <= $t)
           | . + {gap: ((.completed_at | ts) - $t | abs)}
           | select(.gap <= $slack) ]
         | min_by(.gap)) as $producer
        | if $producer.conclusion == "success" then "completed"
          elif any($runs[]; .conclusion == "success"
                   and ((.started_at | ts) // 0) > (($producer.started_at | ts) // $t))
            then "pending"
          elif ($runs | length) < 2 then "retry"
          else "errored" end
      end' 2>/dev/null
}

classify() { # <review json>
  local state body rc verdict
  VERDICT=""
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

  # Not refused, so for Copilot the question left is whether a review happened
  # at all, and the body is not asked. Returning (rather than exiting) is how
  # the two undecided answers reach the caller, which owns the request and the
  # wait.
  if [ "$REVIEWER_COPILOT" = 1 ]; then
    verdict=$(copilot_verdict "$1")
    case "$verdict" in
      completed) ;;
      pending)
        VERDICT=pending
        note "Copilot's review run on PR #$PR has not settled yet"
        return 0 ;;
      retry)
        VERDICT=retry
        note "Copilot's review run on PR #$PR did not produce a review; one re-request is due"
        return 0 ;;
      errored)
        printf '%s\n' "$body"
        fail 6 "$REVIEWER ERRORED on PR #$PR: its own review run failed, and failed again after a re-request, so nothing reviewed the work. This rung is unavailable for THIS pull request only: advance the roster, and do NOT record it as unavailable for the run" ;;
      *)
        printf '%s\n' "$body"
        fail 5 "a $REVIEWER review landed on PR #$PR with no $COPILOT_CHECK check run on its commit to say whether it completed. That check is what this gate reads Copilot's verdict from, so it cannot tell a review from an error. Read the review above: do not label the pull request, and do not advance the roster" ;;
    esac
  fi

  printf '%s review completed [%s]\n%s\n' "$REVIEWER" "$state" "$body"
  exit 0
}

# The classification seam, so the rules above are pinned by fixtures rather than
# by a live pull request. Everything else in this script is polling and a bound.
#
# A copilot review on stdin carries `submitted_at` and `.checks` — the check runs
# the live path fetches for it. Both undecided answers are exit 2 here, because
# this form requests nothing: `retry` is the point at which the live wait would
# re-request, and the message says which of the two it was.
if [ "${1:-}" = --classify ]; then
  [ $# -eq 2 ] || fail 4 "usage: await-review.sh --classify <reviewer>  (a review as JSON on stdin)"
  command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"
  PR=0
  resolve_refusal "$2"
  INPUT=$(cat)
  [ -n "$INPUT" ] || fail 2 "nothing to classify"
  classify "$INPUT"
  fail 2 "not settled: $VERDICT"
fi

USAGE='usage: await-review.sh [--id-env <name>] [--key-env <name>] <pr-number> [<reviewer>]'

# Forwarded verbatim to the mint, which is the only reader of a credential here
# and owns every rule about what a name may look like. The pair arriving
# together is checked here rather than there, because on every rung but `local`
# the mint is never called and nothing else would ever look.
MINT_ARGS=()
ID_ENV_SET=0
KEY_ENV_SET=0
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
    # Reaching here means `--classify` was not the first argument, which is the
    # only place it is accepted. Saying so beats "unknown argument", which is
    # what it would otherwise get: classification reads no credential, so the
    # names have no business on that form and there is nothing to reorder.
    --classify)
      fail 4 "--classify must be the first argument, and takes no credential names -- classification reads a review on stdin and no credential at all" ;;
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

[ "${#ARGS[@]}" -ge 1 ] || fail 4 "$USAGE"
[ "${#ARGS[@]}" -le 2 ] || fail 4 "$USAGE"
PR=${ARGS[0]}
case "$PR" in ''|*[!0-9]*) fail 4 "pull request number must be an integer (got '$PR')" ;; esac

command -v gh >/dev/null 2>&1 || fail 4 "gh is not on PATH"
command -v jq >/dev/null 2>&1 || fail 4 "jq is not on PATH"

# From the origin remote, confirmed over REST; repo.sh says why when it cannot.
REPO=$("$HERE/repo.sh") || fail 4 "cannot resolve the repository; see the repo: line above"
REPO=${REPO%%$'\t'*}
[ -n "$REPO" ] || fail 4 "cannot resolve the repository; repo.sh printed no slug"

resolve_reviewer "${ARGS[1]:-copilot}"

# --------------------------------------------------------------- reading ------
# The most recent review by this rung, as {state, body, submitted_at,
# commit_id}, or nothing. The last two are what Copilot's check runs are
# matched to it by.
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
       | {state: (.state // ""), body: (.body // ""),
          submitted_at: (.submitted_at // ""), commit_id: (.commit_id // "")}' \
    2>/dev/null
}

# The review, with every Copilot check run on the commit it reviewed attached as
# `.checks`. `filter=all` because the default, `latest`, keeps one run per check
# suite — and the failed run before a re-request is the evidence the retry is
# counted by. A read that fails is not an empty list: answered as one, it would
# be exit 5 on a network blip, so it returns 1 and the caller waits instead.
with_checks() { # <review json>
  local sha runs
  sha=$(printf '%s' "$1" | jq -r '.commit_id // ""' 2>/dev/null)
  if [ -z "$sha" ]; then
    printf '%s' "$1"
    return 0
  fi
  runs=$(gh api "repos/$REPO/commits/$sha/check-runs?check_name=$COPILOT_CHECK&filter=all&per_page=100" \
           --jq '[.check_runs[]? | {status, conclusion, started_at, completed_at}]' 2>/dev/null) \
    || return 1
  printf '%s' "$runs" | jq -e 'type == "array"' >/dev/null 2>&1 || return 1
  printf '%s' "$1" | jq -c --argjson runs "$runs" '. + {checks: $runs}'
}

# classify, with what it needs fetched first. Exits on every verdict but the two
# undecided ones, which come back in VERDICT.
settle() { # <review json>
  local review=$1
  if [ "$REVIEWER_COPILOT" = 1 ]; then
    if ! review=$(with_checks "$1"); then
      VERDICT=pending
      note "could not read the $COPILOT_CHECK check runs for PR #$PR; treating the review as not settled yet"
      return 0
    fi
  fi
  classify "$review"
}

EXISTING=$(latest_review)
if [ -n "$EXISTING" ]; then
  note "a $REVIEWER review is already on PR #$PR; not requesting another"
  settle "$EXISTING"
fi

# ------------------------------------------------------------- requesting -----
# Only the rungs that have someone to ask. The local reviewer posts its own
# review through post-review.sh before this script is reached, so there is
# nothing to request and a request step would have nothing to fail on.
#
# Whether this rung is currently requested, read two ways, either of which is
# enough. `GET /pulls/{n}/requested_reviewers` lists people and teams, and does
# NOT list a bot: on z5labs/devex #482 it answered `{"users":[],"teams":[]}`
# while the timeline carried `review_requested` for Copilot (`type: Bot`), and
# #474 reads the same. The old request path never noticed, because the GraphQL
# mutation's own response was what confirmed it; with the request over REST,
# reading back only that endpoint called every placed Copilot request a failure.
#
# So the timeline is read as well: this rung is requested while its most recent
# `review_requested` is newer than its most recent `reviewed` and
# `review_request_removed`. A review ends a request, which is also what tells a
# re-request already placed after an errored review — still outstanding — from
# one that has not been placed yet.
requested() {
  gh api "repos/$REPO/pulls/$PR/requested_reviewers" \
    --jq '[.users[]?.login] + [.teams[]?.slug] | join(" ")' 2>/dev/null \
  | grep -qiE -- "$REVIEWER_MATCH" && return 0

  gh api --paginate "repos/$REPO/issues/$PR/timeline" \
    --jq '.[] | select(.event == "review_requested" or .event == "review_request_removed" or .event == "reviewed")
          | {event, at: (.created_at // .submitted_at // ""),
             login: (.requested_reviewer.login // .user.login // "")}' 2>/dev/null \
  | jq -s -e --arg match "$REVIEWER_MATCH" '
      map(select(.login | test($match; "i"))) | sort_by(.at) | last
      | . != null and .event == "review_requested"' >/dev/null 2>&1
}

# `POST /pulls/{n}/requested_reviewers`, with the full `...[bot]` login — the
# only form REST accepts for a bot. A bare `reviewers[]=Copilot` returns 200
# while doing nothing at all, and the wait below would then time out on a review
# that was never requested; that is also why the answer is not the POST's status
# but `requested`, which reads the pull request back. A bot that is not installed
# on the repository is refused here, reads back as not requested, and is
# unavailable. The login was checked for its `[bot]` suffix before this point, so
# nothing here can ask a person.
request_review() {
  gh api --method POST "repos/$REPO/pulls/$PR/requested_reviewers" \
    -f "reviewers[]=$REVIEWER_LOGIN" >/dev/null 2>&1 || true
  requested
}

# The one re-request an errored Copilot run earns. Copilot leaves the requested
# reviewers when it posts its error review (observed on humus #747-#750), so
# "still requested" means this re-request has already been placed and its run
# has not appeared yet — which is what stops every poll in between from placing
# another. A re-request that cannot be placed is exit 6, not 3: Copilot was
# working a minute ago on this very pull request, so nothing here says it is
# down for the run.
retry_review() {
  if requested; then
    note "$REVIEWER is already re-requested on PR #$PR"
    return 0
  fi
  note "Copilot's review run on PR #$PR failed; re-requesting once, as its error review invites"
  request_review \
    || fail 6 "$REVIEWER ERRORED on PR #$PR and the re-request could not be placed. This rung is unavailable for THIS pull request only: advance the roster, and do NOT record it as unavailable for the run"
}

if [ "$REVIEWER_KIND" = local ]; then
  note "the local rung posts its own review; nothing to request on PR #$PR"
elif [ "$VERDICT" = pending ]; then
  note "waiting on Copilot's run for the review already on PR #$PR; not requesting another"
elif [ "$VERDICT" = retry ]; then
  retry_review
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
  [ -n "$REVIEW" ] || continue
  settle "$REVIEW"
  [ "$VERDICT" = retry ] && retry_review
done

[ -n "$VERDICT" ] \
  && fail 2 "a $REVIEWER review is on PR #$PR, but Copilot's review run had not settled after $((POLL_COUNT * POLL_SECONDS))s; re-run to keep waiting. Do not retire this rung on it"
fail 2 "no $REVIEWER review on PR #$PR after $((POLL_COUNT * POLL_SECONDS))s; re-run to keep waiting. Silence is not unavailability: a slow reviewer looks exactly like this, so do not retire this rung on it"
