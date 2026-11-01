# gh-guard.sh — sourced as the first line of every stub `gh` in this directory's
# `*_test.sh` suites. Not executable on its own.
#
# A Claude Code cloud session's GitHub proxy answers GraphQL with HTTP 403 "GitHub
# GraphQL is not available from Claude Code sessions", and `gh repo view`,
# `gh issue`, `gh pr`, `gh label list`, `gh project`, `gh search` and
# `gh release list` are all GraphQL-backed. This guard answers each of them the
# way that proxy does, so a script that drifts back onto one fails its suite
# offline rather than a cloud session's first iteration.
#
# A refusal alone would not be enough. Most of the scripts read a failed `gh` as
# "unreadable", and several of their tests expect exactly that answer, so a
# regression could exit the way the case wanted while making the call it must
# not make. Every refused call is therefore also appended to $GH_GUARD_LOG, and
# each suite ends with a case asserting that file is empty.
#
# The one sanctioned GraphQL read is `select-issue.sh`'s project scope, which no
# cloud session can serve over any API. A suite that is exercising it sets
# GH_GUARD_GRAPHQL=allow for that call, and the guard lets `gh api graphql`
# through to the stub behind it. Nothing else is ever let through.

gh_guard() {
  local why="" a
  case "${1:-}" in
    issue|pr|project|search) why="gh $1" ;;
    repo)    [ "${2:-}" = view ] && why="gh repo view" ;;
    label)   [ "${2:-}" = list ] && why="gh label list" ;;
    release) [ "${2:-}" = list ] && why="gh release list" ;;
    api)
      for a in "$@"; do
        case "$a" in graphql|/graphql|*api.github.com/graphql) why="gh api graphql" ;; esac
      done ;;
  esac
  [ -n "$why" ] || return 0
  [ "$why" = "gh api graphql" ] && [ "${GH_GUARD_GRAPHQL:-}" = allow ] && return 0

  [ -n "${GH_GUARD_LOG:-}" ] && printf '%s\n' "$*" >>"$GH_GUARD_LOG"
  printf '{"message":"GitHub GraphQL is not available from Claude Code sessions; use the REST API (gh api repos/{owner}/{repo}/...)."}'
  printf 'gh: GitHub GraphQL is not available from Claude Code sessions; use the REST API (gh api repos/{owner}/{repo}/...). (HTTP 403)\n' >&2
  exit 1
}

gh_guard "$@"
