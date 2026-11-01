#!/usr/bin/env bash
gh api repos/{owner}/{repo}/pulls --jq '.[].number'   # REST, not gh pr list
cat <<USAGE
usage: comments.sh — labels the pull request; the merge workflow runs gh pr merge.
USAGE
bash <<'SCRIPT'
gh pr list --json number
SCRIPT
