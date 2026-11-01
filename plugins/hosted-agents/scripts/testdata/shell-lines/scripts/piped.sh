#!/usr/bin/env bash
printf '%s\n' "$body" | gh issue comment "$1" --body-file -
echo "$query" | gh api graphql --input -
echo "starting" && gh pr merge --squash "$1"
printf '%s\n' "$(gh issue list --json number)"
