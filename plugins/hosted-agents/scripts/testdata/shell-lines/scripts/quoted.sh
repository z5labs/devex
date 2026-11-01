#!/usr/bin/env bash
timeout 600 bash -c "gh pr list --json number"
ssh build-host "gh issue list"
eval "gh repo view --json name"
curl -fsS -H "Authorization: token $GITHUB_TOKEN" https://api.github.com/user
