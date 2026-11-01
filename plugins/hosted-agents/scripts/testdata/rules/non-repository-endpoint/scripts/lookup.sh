#!/usr/bin/env bash
gh api "repos/{owner}/{repo}/pulls/$1/requested_reviewers" --jq '.users[].login'
gh api search/issues -f q="repo:acme/app is:open" --jq .total_count
gh api "/users/$LOGIN" --jq .type
gh api /app --jq .slug
curl -fsS https://api.github.com/orgs/acme/members
# In a cloud session these are not permitted through this proxy: sessions are bound to their configured repositories.
status=$?  # [ "${CLAUDE_CODE_REMOTE:-}" = true ] in a comment guards nothing
exit "$status"
