#!/usr/bin/env bash
#
# repo.sh — the GitHub repository this checkout is, resolved over REST.
#
# Usage: repo.sh                 # prints "<owner>/<name>\t<default-branch>"
#        repo.sh --parse <url>   # prints "<owner>/<name>", offline
#
# Every script in this plugin used to ask `gh repo view`, and so did the skills.
# `gh repo view` is GraphQL-backed, and a Claude Code cloud session's GitHub
# proxy answers GraphQL with HTTP 403 "GitHub GraphQL is not available from
# Claude Code sessions" — so the cycle stopped at its first GitHub call there,
# while every REST route under `repos/{owner}/{repo}/...` was served. This is the
# one place the answer now comes from, over a route both kinds of session serve.
#
# Two sources, in this order, and neither is configured:
#
#   * `git remote get-url origin` names the repository, in whichever form the
#     clone used — `https://github.com/<owner>/<name>(.git)`, the scp-like
#     `git@github.com:<owner>/<name>.git`, or `ssh://git@github.com/...`. A
#     scheme URL is read by its last two path segments, so a remote served
#     through a local git proxy (`http://proxy@127.0.0.1:8080/git/<owner>/<name>`)
#     reads the same as one pointing at github.com.
#   * `GET /repos/{owner}/{name}` confirms it. Its `full_name` is the slug
#     printed, not the parsed one: GitHub redirects a renamed or transferred
#     repository's old name to its new one, so a clone made before a rename
#     resolves to the name the repository has now, and every later call is made
#     against that. Its `default_branch` is the default branch.
#
# The host is not read. The plugin talks to github.com through `gh`, and `gh api`
# goes to github.com unless GH_HOST says otherwise; a remote on another host is
# parsed exactly the same way and confirmed against whatever host `gh` is set to.
#
# A local path, or a `file://` URL, is refused rather than read by its last two
# directory names. Those name a directory, not a GitHub repository, and guessing
# a slug out of one would point every call at whichever repository happened to
# share the directory's name.
#
# Exit codes:
#   0  stdout is the answer
#   4  not inside a git checkout, no `origin` remote, a remote that does not
#      parse (the message names it, with any credential in it masked), or the
#      repository could not be read from GitHub

set -uo pipefail

fail() { local code=$1; shift; printf 'repo: %s\n' "$*" >&2; exit "$code"; }

# A remote URL can carry a credential — `https://x-access-token:<token>@github.com/...`
# is how CI checkouts are commonly written. Any message naming the URL masks the
# user-info part, so a failure never prints a token into a transcript.
mask() { printf '%s' "$1" | sed -E 's#^([A-Za-z][A-Za-z0-9+.-]*://)[^/@]*@#\1***@#'; }

# Prints "<owner>/<name>" or returns 1. An owner opens with a letter or digit
# and goes on in letters, digits, `-` and `_` (the last for managed-user
# logins); a name is letters, digits, `.`, `_` and `-`, and is never `.` or `..`.
parse() { # <url>
  local url=$1 path owner name host

  case "$url" in
    file://*|/*|./*|../*) return 1 ;;
    *://*)
      path=${url#*://}       # drop the scheme
      path=${path#*/}        # drop user-info, host and port: everything to the first /
      [ "$path" != "${url#*://}" ] || return 1
      ;;
    *)
      # scp-like, `[user@]host:path`, which is what git reads any remote with a
      # colon and no slash before it as.
      host=${url%%:*}
      [ "$host" != "$url" ] || return 1
      case "$host" in */*|'') return 1 ;; esac
      path=${url#*:}
      ;;
  esac

  path=${path%%[?#]*}        # a query or fragment is not part of the path
  path=${path%/}
  path=${path%.git}
  path=${path%/}

  name=${path##*/}
  path=${path%/*}
  [ "$path" != "$name" ] || return 1   # fewer than two segments
  owner=${path##*/}

  [[ $owner =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]] || return 1
  [[ $name =~ ^[A-Za-z0-9._-]+$ ]] || return 1
  case "$name" in .|..) return 1 ;; esac

  printf '%s/%s\n' "$owner" "$name"
}

if [ "${1:-}" = --parse ]; then
  [ $# -eq 2 ] || fail 4 "usage: repo.sh --parse <url>"
  parse "$2" || fail 4 "cannot read a GitHub <owner>/<name> out of the remote '$(mask "$2")'; expected https://github.com/<owner>/<name>(.git) or git@github.com:<owner>/<name>.git"
  exit 0
fi

[ $# -eq 0 ] || fail 4 "usage: repo.sh | repo.sh --parse <url>"

command -v git >/dev/null 2>&1 || fail 4 "git is not on PATH"
command -v gh  >/dev/null 2>&1 || fail 4 "gh is not on PATH"

git rev-parse --git-dir >/dev/null 2>&1 || fail 4 "not inside a git repository"
URL=$(git remote get-url origin 2>/dev/null) \
  || fail 4 "this checkout has no 'origin' remote, so there is no GitHub repository to resolve; add one with git remote add origin <url>"

SLUG=$(parse "$URL") \
  || fail 4 "cannot read a GitHub <owner>/<name> out of the origin remote '$(mask "$URL")'; expected https://github.com/<owner>/<name>(.git) or git@github.com:<owner>/<name>.git"

# stderr is kept, because the reason is the whole message: a repository not
# attached to a cloud session, a token without access, and a name that does not
# exist are all a failed read here, and each has a different remedy.
ERR=$(mktemp) || fail 4 "cannot create a temporary file for the repository read"
trap 'rm -f "$ERR"' EXIT

OUT=$(gh api "repos/$SLUG" --jq '"\(.full_name // "")\t\(.default_branch // "")"' 2>"$ERR") \
  || fail 4 "cannot read repos/$SLUG from GitHub (from the origin remote '$(mask "$URL")'): $(tr '\n' ' ' <"$ERR" | sed 's/ *$//')"

IFS=$'\t' read -r FULL BRANCH <<<"$OUT"
[ -n "${FULL:-}" ] && [ -n "${BRANCH:-}" ] \
  || fail 4 "GET repos/$SLUG answered without a full_name and a default_branch"

printf '%s\t%s\n' "$FULL" "$BRANCH"
