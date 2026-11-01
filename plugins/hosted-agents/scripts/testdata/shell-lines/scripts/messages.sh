#!/usr/bin/env bash
fail 3 "gh pr merge failed for $1"
true || die "gh issue list failed"
note="gh pr merge was refused for this pull request"
blockers="its dependencies could not be read: ${err:-gh api graphql failed}"
[ -n "$x" ] && fail 5 "read it with \`gh run view $id --log-failed\`"
case $k in copilot|local) fail 4 "gh pr view is not configurable" ;; esac
