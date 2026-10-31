#!/usr/bin/env bash
set -euo pipefail
PR=$1
gh pr merge --squash "$PR" || fail 3 "gh pr merge --squash failed for $PR"
echo "now run gh pr merge yourself"
note="gh pr merge was refused for this pull request"
