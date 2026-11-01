#!/usr/bin/env bash
gh issue list --label story --json number
# GraphQL is not available from Claude Code sessions: a comment that names the 403 guards nothing,
status=$?  # and neither does a trailing [ "${CLAUDE_CODE_REMOTE:-}" = true ],
echo "nor a message that says GraphQL is not available from Claude Code" >&2
exit "$status"
