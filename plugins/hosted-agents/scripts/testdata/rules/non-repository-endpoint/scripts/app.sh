#!/usr/bin/env bash
# GET /app and accounts by login are refused in a Claude Code cloud session, so
# the lookup is optional there.
if [ "${CLAUDE_CODE_REMOTE:-}" = true ]; then
  echo "the App lookup is unavailable in a Claude Code cloud session" >&2
  exit 3
fi
gh api /app --jq .slug
gh api "/users/$LOGIN" --jq .type
app_api() { gh api -H "Authorization: Bearer $JWT" "$@"; }
app_api --method POST "/app/installations/$1/access_tokens" --jq .token
