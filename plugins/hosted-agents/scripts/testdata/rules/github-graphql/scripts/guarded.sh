#!/usr/bin/env bash
# Projects v2 exists only in GraphQL, which a Claude Code cloud session's proxy
# refuses, so this read names that restriction instead of failing blind.
if ! gh api graphql -F owner="$1" -F number="$2" -f query="$QUERY" 2>"$ERR"; then
  if grep -qiE 'GraphQL is not available from Claude Code' "$ERR"; then
    echo "project-scoped selection is local-only in a Claude Code cloud session" >&2
    exit 4
  fi
  exit 1
fi
