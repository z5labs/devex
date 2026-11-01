#!/usr/bin/env bash
gh api "repos/{owner}/{repo}/pulls/$1/requested_reviewers" --jq '.users[].login'
gh api search/issues -f q="repo:acme/app is:open" --jq .total_count
gh api "/users/$LOGIN" --jq .type
gh api /app --jq .slug
curl -fsS https://api.github.com/orgs/acme/members
