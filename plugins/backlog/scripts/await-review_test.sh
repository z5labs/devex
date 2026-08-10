#!/usr/bin/env bash
#
# await-review_test.sh — the fixture corpus for await-review.sh.
#
# Three halves, all offline.
#
# **Classification.** Every case is a review the timeline can carry, fed to
# `--classify`. This is the judgment the merge label rests on, and it is the
# half with a failure already on record: Copilot answers a pull request over 300
# files with a review whose body says it could not review it, that refusal
# satisfies any `length > 0` test, and a cycle merged with no review at all. A
# refusal read as a completed review is a merge; a completed review read as a
# refusal is a needless BLOCKED. Neither is visible from the outside.
#
# **The reviewer argument.** The roster's whole premise is that a rung which
# cannot review costs a rung rather than the run, so the three outcomes have to
# be told apart: UNAVAILABLE (exit 3, advance and remember), REFUSED (exit 1,
# stop — the rung looked at the work and said something true about it), and
# nothing-yet (exit 2, ask again, and do not retire a reviewer that was merely
# slow). The cases below drive the whole script against a `gh` that answers from
# files, so the login filter is exercised rather than described.
#
# **The App identity.** The `local` rung waits on a review posted under the
# backlog App's login, which it discovers by minting a JWT. Absent credentials
# must be exit 3 at zero network cost — a fallthrough, not a crash — and present
# ones must produce a login the timeline filter then uses. The key is generated
# at run time; there is no key material in this file.
#
# Run: plugins/backlog/scripts/await-review_test.sh
# Exit 0 when every case matches, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/await-review.sh"
[ -x "$SUT" ] || { printf 'await-review.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

pass=0
fail=0

ok()   { pass=$((pass + 1)); printf '  ok   %-58s [%s]\n' "$1" "$2"; }
bad()  { fail=$((fail + 1)); printf '  FAIL %-58s %s\n' "$1" "$2"; }

SCRATCH=$(mktemp -d) || { printf 'cannot create a scratch directory\n' >&2; exit 1; }
trap 'rm -rf "$SCRATCH"' EXIT
mkdir -p "$SCRATCH/bin" "$SCRATCH/fx" "$SCRATCH/cfg/.claude"

# ---------------------------------------------------------- classification ----
#
# Classification runs in a directory this test owns, because a `bot:` rung's
# refusal wording comes from `.claude/backlog.json` — read cwd-relative, exactly
# as select-issue.sh reads it. The default is no config at all, which is what a
# repository that has never named a bot looks like.

refusals() { # <refusals object, or nothing to remove the config>
  if [ $# -eq 0 ]; then
    rm -f "$SCRATCH/cfg/.claude/backlog.json"
    return
  fi
  printf '{"review":{"reviewers":["copilot"],"refusals":%s}}\n' "$1" \
    >"$SCRATCH/cfg/.claude/backlog.json"
}
refusals

# check <name> <reviewer> <expected exit> <review json>
check() {
  local name=$1 reviewer=$2 want=$3 json=$4 got out
  out=$(cd "$SCRATCH/cfg" && printf '%s' "$json" | "$SUT" --classify "$reviewer" 2>&1)
  got=$?
  if [ "$got" = "$want" ]; then
    ok "$name" "exit $got"
  else
    bad "$name" "want exit $want got $got: $(printf '%s' "$out" | tr '\n' ' ')"
  fi
}

printf '\ncompleted — the only outcome that may label a pull request\n'

check 'a review with findings' copilot 0 \
'{"state":"COMMENTED","body":"Two issues below, both minor."}'

check 'a review that generated no comments' copilot 0 \
'{"state":"COMMENTED","body":"Copilot reviewed 3 out of 3 changed files and generated no comments."}'

# An approval is still a completed review. The cycle never asks for one and the
# local rung never posts one, but a human approving alongside the bot must not
# turn into a different verdict.
check 'an approving review' copilot 0 \
'{"state":"APPROVED","body":"Looks right."}'

check 'a review with an empty body' copilot 0 \
'{"state":"COMMENTED","body":""}'

# The local rung is classified by the same rules, which is what keeps one gate
# over the whole roster.
check 'a local review with findings' local 0 \
'{"state":"COMMENTED","body":"The retry loop swallows the error."}'

printf '\nrefused — BLOCKED, and the roster does NOT advance\n'

# The one that was missed in the wild, verbatim.
check 'the 300-file refusal' copilot 1 \
'{"state":"COMMENTED","body":"Copilot wasn'"'"'t able to review this pull request because it exceeds the maximum number of files (300)."}'

# GitHub has rendered the apostrophe both ways, and a classifier that only knows
# the ASCII one merges the other.
check 'the same refusal with a typographic apostrophe' copilot 1 \
'{"state":"COMMENTED","body":"Copilot wasn’t able to review this pull request because it exceeds the maximum number of files (300)."}'

check 'the refusal spelled out in full' copilot 1 \
'{"state":"COMMENTED","body":"Copilot was not able to review this pull request."}'

# The local reviewer is told to decline in the same words, so that one
# classifier covers both rungs the plugin ships rather than one per rung.
check 'a local reviewer declining in the same words' local 1 \
'{"state":"COMMENTED","body":"I was not able to review this pull request: the diff exceeds what fits in one context."}'

# Prose that merely mentions reviewing is not a refusal. A classifier loose
# enough to catch this one BLOCKS on ordinary review comments.
check 'a comment that merely discusses reviewing' copilot 0 \
'{"state":"COMMENTED","body":"I was able to review this quickly; the change is small."}'

printf '\nescalation — a bot whose refusal wording the plugin does not know\n'

# The whole reason the generic rung was held back. A different bot refuses in
# words of its own, and a classifier that only knows Copilot's would return that
# refusal as a completed review — which is a green light to label and merge.
# Neither of these may be exit 0, and the ordinary review is the case that says
# so: escalation is a property of the RUNG, not of the body.
COD='{"state":"COMMENTED","body":"Actionable comments posted: 2"}'
COD_DECLINE='{"state":"COMMENTED","body":"Review skipped: the diff is too large for CodeRabbit to process."}'

refusals
check 'an unknown bots ordinary review escalates' 'bot:coderabbitai[bot]' 5 "$COD"
check 'an unknown bots refusal escalates too'     'bot:coderabbitai[bot]' 5 "$COD_DECLINE"

# The escape hatch: an operator who has SEEN this bot decline supplies the
# wording, and the rung then classifies exactly as copilot does — both ways.
refusals '{"bot:coderabbitai[bot]":"[Rr]eview skipped"}'
check 'a configured pattern that matches is a refusal' 'bot:coderabbitai[bot]' 1 "$COD_DECLINE"
check 'a configured pattern that does not match completes' 'bot:coderabbitai[bot]' 0 "$COD"

# A pattern keyed to another rung is not this rung's. Escalation has to be per
# rung, or one bot's configured wording would quietly vouch for every other.
refusals '{"bot:gemini-code-assist[bot]":"[Rr]eview skipped"}'
check 'another rungs pattern does not classify this one' 'bot:coderabbitai[bot]' 5 "$COD_DECLINE"

# REGRESSION: the pattern is operator-supplied, so one that opens with a dash
# is parsed by grep as OPTIONS rather than as a pattern. Without `--` the body
# is never tested at all, and a rung the operator configured on purpose stops
# classifying anything.
refusals '{"bot:coderabbitai[bot]":"-- review skipped"}'
check 'a pattern opening with a dash is still a pattern' 'bot:coderabbitai[bot]' 1 \
'{"state":"COMMENTED","body":"Result -- review skipped, the diff is too large."}'
check 'and it still lets an ordinary review complete' 'bot:coderabbitai[bot]' 0 "$COD"

# A pattern grep cannot compile must never fall through as "did not match" —
# that reads a refusal as a completed review, which is the merge this whole
# check exists to prevent. select-issue.sh rejects it at step 1; reaching here
# means the config changed under the run.
refusals '{"bot:coderabbitai[bot]":"*("}'
check 'a pattern that will not compile is not a pass' 'bot:coderabbitai[bot]' 4 "$COD_DECLINE"

# `copilot` is sugar for its login, so the desugared spelling has to behave
# identically — built-in wording, not something the operator has to supply.
refusals
check 'the desugared copilot rung keeps the built-in wording' 'bot:copilot-pull-request-reviewer[bot]' 1 \
'{"state":"COMMENTED","body":"Copilot wasn'"'"'t able to review this pull request because it exceeds the maximum number of files (300)."}'
check 'the desugared copilot rung still completes' 'bot:copilot-pull-request-reviewer[bot]' 0 \
'{"state":"COMMENTED","body":"Copilot reviewed 3 out of 3 changed files and generated no comments."}'

printf '\nnothing to classify\n'

check 'an empty review' copilot 2 ''

printf '\nusage\n'

# usage_check <name> <expected exit> <args...>
usage_check() {
  local name=$1 want=$2; shift 2
  "$SUT" "$@" >/dev/null 2>&1 </dev/null
  local got=$?
  if [ "$got" = "$want" ]; then ok "$name" "exit $got"; else bad "$name" "want exit $want got $got"; fi
}

usage_check 'no arguments' 4
usage_check 'a pull request number that is not a number' 4 not-a-number
usage_check 'more arguments than the usage takes' 4 12 copilot extra
usage_check 'an unknown rung' 4 12 coderabbit
# A bare prefix names no bot. Accepting it would build a timeline filter out of
# an empty login, which matches every reviewer there has ever been.
usage_check 'a bot rung with no login' 4 12 bot:
usage_check '--classify rejects a bot rung with no login' 4 --classify bot:
# `none` is a roster decision, not a wait. Waiting on it would be waiting on
# nobody, and the five minutes would look exactly like a slow reviewer.
usage_check 'none is not a reviewer to wait on' 4 12 none
usage_check '--classify needs a reviewer' 4 --classify
usage_check '--classify rejects an unknown rung' 4 --classify coderabbit

# --------------------------------------------------------------- the wait -----
printf '\nthe wait, against a gh that answers from files\n'

# The stub answers the six calls the script makes, and really runs the `--jq` it
# was given for the timeline — the login filter is the thing under test, so it
# must not be short-circuited by a stub that returns the answer directly.
cat >"$SCRATCH/bin/gh" <<'STUB'
#!/usr/bin/env bash
q="" path="" post=0 prev=""
for a in "$@"; do
  [ "$prev" = --jq ] && q=$a
  case "$a" in
    --method) post=1 ;;
    repos/*/timeline|repos/*/requested_reviewers|/users/*|/app|/repos/*/installation|/app/installations/*/access_tokens) path=$a ;;
  esac
  prev=$a
done
jqf() { if [ -n "$q" ]; then jq -r "$q" "$1"; else cat "$1"; fi; }
case "$1 $2" in
  'repo view') printf 'z5labs/devex\n'; exit 0 ;;
  'pr view')   printf 'PR_kwNODUMMY\n'; exit 0 ;;
  'api graphql')
    [ -f "$FX/graphql" ] || exit 1
    cat "$FX/graphql"; exit 0 ;;
esac
case "$path" in
  */timeline)             jqf "$FX/timeline" ;;
  */requested_reviewers)
    [ "$post" = 1 ] && exit 0
    jqf "$FX/requested" ;;
  /users/*)               [ -f "$FX/botid" ] || exit 1; jqf "$FX/botid" ;;
  /app)                   printf 'backlog-bot\n' ;;
  /repos/*/installation)  printf '4242\n' ;;
  */access_tokens)        printf 'ghs_stubbed\n' ;;
  *) printf 'stub gh: unexpected call: %s\n' "$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$SCRATCH/bin/gh"

printf '{"users":[],"teams":[]}\n' >"$SCRATCH/fx/requested"

reviewed() { # <login> <submitted_at> <body>
  jq -nc --arg l "$1" --arg t "$2" --arg b "$3" \
    '{event:"reviewed", submitted_at:$t, state:"COMMENTED", body:$b, user:{login:$l}}'
}
timeline() { printf '[%s]\n' "$(printf '%s,' "$@" | sed 's/,$//')" >"$SCRATCH/fx/timeline"; }

# w_run <expected exit> <args...>  — one poll of zero seconds, so a wait that
# is going to time out does so immediately.
w_run() {
  W_OUT=$(cd "$SCRATCH" && PATH="$SCRATCH/bin:$PATH" FX="$SCRATCH/fx" \
    BACKLOG_REVIEW_POLL_COUNT=1 BACKLOG_REVIEW_POLL_SECONDS=0 \
    "$SUT" "$@" 2>&1)
  W_RC=$?
}

# checkw <name> <expected exit> <args...>
checkw() {
  local name=$1 want=$2; shift 2
  w_run "$@"
  if [ "$W_RC" = "$want" ]; then
    ok "$name" "exit $W_RC"
  else
    bad "$name" "want exit $want got $W_RC: $(printf '%s' "$W_OUT" | tr '\n' ' ' | cut -c1-160)"
  fi
}

rm -f "$SCRATCH/fx/graphql" "$SCRATCH/fx/botid"

# A review already on the timeline is classified without requesting another,
# which is what makes re-running after an exit 2 safe.
timeline "$(reviewed 'copilot-pull-request-reviewer[bot]' '2026-01-01T00:00:00Z' 'one nit below')"
checkw 'a completed review already on the timeline' 0 12 copilot

timeline "$(reviewed 'Copilot' '2026-01-01T00:00:00Z' 'one nit below')"
checkw 'the login filter is case-insensitive' 0 12 copilot

timeline "$(reviewed 'copilot-pull-request-reviewer[bot]' '2026-01-01T00:00:00Z' "Copilot wasn't able to review this pull request because it exceeds the maximum number of files (300).")"
checkw 'a refusal on the timeline stops the cycle' 1 12 copilot

# The newest review wins, so an old refusal beside a newer completed review is
# not misread — and neither is the reverse, which is the direction that merges.
timeline \
  "$(reviewed 'copilot-pull-request-reviewer[bot]' '2026-01-01T00:00:00Z' "Copilot wasn't able to review this pull request because it exceeds the maximum number of files (300).")" \
  "$(reviewed 'copilot-pull-request-reviewer[bot]' '2026-01-02T00:00:00Z' 'reviewed after a rebase; one nit')"
checkw 'a newer completed review supersedes an older refusal' 0 12 copilot

timeline \
  "$(reviewed 'copilot-pull-request-reviewer[bot]' '2026-01-02T00:00:00Z' "Copilot wasn't able to review this pull request because it exceeds the maximum number of files (300).")" \
  "$(reviewed 'copilot-pull-request-reviewer[bot]' '2026-01-01T00:00:00Z' 'reviewed before the vendored tree landed')"
checkw 'a newer refusal supersedes an older completed review' 1 12 copilot

# Without the login filter the repository owner glancing at the pull request
# ends the wait, and the merge label is then applied on a review that never
# arrived. This is the case that says the filter is still there.
timeline "$(reviewed 'carsonderr' '2026-01-01T00:00:00Z' 'nice work')"
checkw 'a human review does not end the wait' 3 12 copilot

# Nothing on the timeline and the request refused outright: the rung told us it
# is down, in a second or two. That is exit 3 — advance the roster and remember
# it — and it is the case an exhausted quota or a disabled organisation hits.
timeline
checkw 'a request that cannot be placed is unavailable' 3 12 copilot

# The request took, and nothing has arrived yet. Silence is NOT unavailability:
# a slow reviewer looks identical from here, and retiring it for the run on this
# evidence is how a working reviewer gets dropped for three hours.
printf '{"users":[{"login":"copilot-pull-request-reviewer[bot]"}],"teams":[]}\n' >"$SCRATCH/fx/requested"
timeline
checkw 'an accepted request with nothing posted yet is not a verdict' 2 12 copilot

# The REST fallback is reached when the GraphQL mutation does not take, and it
# is the path that only works with the full `...[bot]` login.
printf '{"users":[],"teams":[]}\n' >"$SCRATCH/fx/requested"
printf '{"node_id":"BOT_kwNODUMMY"}\n' >"$SCRATCH/fx/botid"
printf '{"data":{"requestReviews":{"pullRequest":{"reviewRequests":{"nodes":[{"requestedReviewer":{"__typename":"Bot","login":"copilot-pull-request-reviewer"}}]}}}}}\n' >"$SCRATCH/fx/graphql"
timeline
checkw 'a request the mutation placed leaves the wait pending' 2 12 copilot

# ------------------------------------------------------- the generic rung -----
printf '\na bot:<login> rung, through the same wait\n'

rm -f "$SCRATCH/fx/graphql" "$SCRATCH/fx/botid"
printf '{"users":[],"teams":[]}\n' >"$SCRATCH/fx/requested"

# The wait runs with cwd at $SCRATCH, so this is the config it reads.
bot_refusals() { # <refusals object, or nothing>
  if [ $# -eq 0 ]; then
    rm -f "$SCRATCH/.claude/backlog.json"
    return
  fi
  mkdir -p "$SCRATCH/.claude"
  printf '{"review":{"reviewers":["bot:coderabbitai[bot]"],"refusals":%s}}\n' "$1" \
    >"$SCRATCH/.claude/backlog.json"
}
bot_refusals

# Not installed on the repository: the login resolves to nothing and the request
# cannot be placed. That is unavailability — a rung costs a rung, not the run.
timeline
checkw 'an uninstalled bot login is unavailable' 3 12 'bot:coderabbitai[bot]'

# A login that is a person. `requestReviews` takes botIds, so this can never be
# requested as a bot — and the REST fallback would otherwise request a review
# from a HUMAN under a rung the operator wrote as a bot.
printf '{"type":"User","node_id":"U_kwNODUMMY"}\n' >"$SCRATCH/fx/botid"
timeline
checkw 'a login that is not a bot is unavailable' 3 12 'bot:coderabbitai[bot]'

# A review that landed, from a bot the plugin has never seen. Both spellings of
# the login filter the same events, because the `[bot]` suffix is dropped before
# the match is built.
printf '{"type":"Bot","node_id":"BOT_kwNODUMMY"}\n' >"$SCRATCH/fx/botid"
timeline "$(reviewed 'coderabbitai[bot]' '2026-01-01T00:00:00Z' 'Actionable comments posted: 2')"
checkw 'a landed review with no configured wording escalates' 5 12 'bot:coderabbitai[bot]'
checkw 'the login spelled without [bot] filters the same events' 5 12 'bot:coderabbitai'

# Configured, and it classifies both ways from then on.
bot_refusals '{"bot:coderabbitai[bot]":"[Rr]eview skipped"}'
checkw 'a configured rung completes on an ordinary review' 0 12 'bot:coderabbitai[bot]'

timeline "$(reviewed 'coderabbitai[bot]' '2026-01-01T00:00:00Z' 'Review skipped: the diff is too large.')"
checkw 'a configured rung still refuses in its own words' 1 12 'bot:coderabbitai[bot]'

# The login filter is per rung here too: another bot's review is not this
# rung's, and without the filter one review would satisfy two rungs. The request
# is already on record, so what is left is plainly "not yet" rather than
# "could not be requested".
printf '{"users":[{"login":"coderabbitai[bot]"}],"teams":[]}\n' >"$SCRATCH/fx/requested"
timeline "$(reviewed 'copilot-pull-request-reviewer[bot]' '2026-01-01T00:00:00Z' 'one nit')"
checkw 'another bots review does not satisfy this rung' 2 12 'bot:coderabbitai[bot]'

bot_refusals
rm -f "$SCRATCH/fx/botid"
printf '{"users":[],"teams":[]}\n' >"$SCRATCH/fx/requested"

# ------------------------------------------------------------ the App ---------
printf '\nthe local rung and its App identity\n'

rm -f "$SCRATCH/fx/graphql" "$SCRATCH/fx/botid"
printf '{"users":[],"teams":[]}\n' >"$SCRATCH/fx/requested"

# Credentials absent is a fallthrough, not a crash, and it costs no network call
# at all — which is the whole reason the roster can afford to try this rung
# first on a repository that has never configured the App.
timeline
W_OUT=$(cd "$SCRATCH" && PATH="$SCRATCH/bin:$PATH" FX="$SCRATCH/fx" \
  BACKLOG_REVIEW_POLL_COUNT=1 BACKLOG_REVIEW_POLL_SECONDS=0 \
  BACKLOG_APP_ID= BACKLOG_APP_KEY= "$SUT" 12 local 2>&1)
W_RC=$?
if [ "$W_RC" = 3 ]; then ok 'the local rung with no App credentials is unavailable' 'exit 3'
else bad 'the local rung with no App credentials is unavailable' "want exit 3 got $W_RC: $(printf '%s' "$W_OUT" | tr '\n' ' ' | cut -c1-160)"; fi

if command -v openssl >/dev/null 2>&1; then
  # A real RSA key, generated here rather than committed. The App JWT is
  # RS256-signed, so a stub cannot stand in for the signature step.
  openssl genrsa -out "$SCRATCH/app.pem" 2048 >/dev/null 2>&1

  # app_run <expected exit> <name>
  app_run() {
    local want=$1 name=$2
    W_OUT=$(cd "$SCRATCH" && PATH="$SCRATCH/bin:$PATH" FX="$SCRATCH/fx" \
      BACKLOG_REVIEW_POLL_COUNT=1 BACKLOG_REVIEW_POLL_SECONDS=0 \
      BACKLOG_APP_ID=12345 BACKLOG_APP_KEY="$SCRATCH/app.pem" "$SUT" 12 local 2>&1)
    W_RC=$?
    if [ "$W_RC" = "$want" ]; then
      ok "$name" "exit $W_RC"
    else
      bad "$name" "want exit $want got $W_RC: $(printf '%s' "$W_OUT" | tr '\n' ' ' | cut -c1-160)"
    fi
  }

  # The stubbed `GET /app` answers `backlog-bot`, so the login the timeline is
  # filtered on is `backlog-bot[bot]` — discovered, never hard-coded, because
  # the App is the operator's and its slug is not this plugin's to know.
  timeline "$(reviewed 'backlog-bot[bot]' '2026-01-01T00:00:00Z' 'the retry loop swallows the error')"
  app_run 0 'a local review by the App is a completed review'

  timeline "$(reviewed 'backlog-bot[bot]' '2026-01-01T00:00:00Z' 'I was not able to review this pull request.')"
  app_run 1 'a local reviewer that declines still blocks'

  # Copilot's review is not the local rung's review. Two rungs on one pull
  # request is ordinary — the first went silent — and a filter that matched any
  # bot would let the silent rung's absence be satisfied by the other one.
  timeline "$(reviewed 'copilot-pull-request-reviewer[bot]' '2026-01-01T00:00:00Z' 'one nit')"
  app_run 2 'another bot review does not satisfy the local rung'

  # Nothing posted: the local rung has no request step, so this is plainly "not
  # yet" rather than "could not be requested".
  timeline
  app_run 2 'the local rung with nothing posted yet is not a verdict'
else
  printf '  skip openssl is not on PATH; the App identity cases need it\n'
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
