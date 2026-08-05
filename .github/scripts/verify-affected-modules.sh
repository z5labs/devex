#!/usr/bin/env bash
#
# verify-affected-modules.sh — run the checks CI would run for this branch's
# daggerverse changes, from inside a worktree.
#
# The second half of `verify` in .claude/backlog.json. The first half is
# `dagger check`, the root module's three checks, which the planner always runs
# and never memoizes. This is everything else a worktree can reproduce.
#
# Why not just ask the planner. `.github/workflows/ci.yml` gets its whole matrix
# from one `dagger -m daggerverse/workspace-ci call plan`, and that is the
# authority on which legs a change needs. It reads the change set out of a real
# `.git` directory — and in a git worktree `.git` is a file pointing elsewhere,
# so the planner cannot see the history at all. The backlog cycle runs only in
# worktrees. That leaves this approximation.
#
# What it does NOT reproduce, stated so nobody discovers it as a surprise: the
# planner's dependency closure. A change to a module that some *other* module
# depends on runs that other module's checks in CI and not here. Adding closure
# would mean a second planner to keep in agreement with the first, which is the
# thing workspace-ci exists to avoid.
#
# Why this is a file rather than a string in backlog.json: a command the loop
# runs unattended has to match a permission rule, and a rule cannot match a
# command that opens with a shell assignment. It also gets to carry these
# comments.

set -uo pipefail

# The default branch is resolved rather than named. A rename must not leave this
# diffing against a branch that no longer exists.
default_branch=$(gh repo view --json defaultBranchRef --jq .defaultBranchRef.name) || exit 1
base="origin/${default_branch}"

# `cut -f1,2` collapses daggerverse/<m>/... and daggerverse/<m>/tests/... to the
# same daggerverse/<m>, so a change to either runs both suites below.
modules=$(git diff --name-only "${base}...HEAD" -- 'daggerverse/*' | cut -d/ -f1,2 | sort -u)

if [ -z "$modules" ]; then
  echo "verify-affected-modules: no daggerverse module changed against ${base}"
  exit 0
fi

for module in $modules; do
  echo "verify-affected-modules: ${module}"
  # A module that declares no checks of its own exits 0 with an empty check
  # list, so this is safe to run unconditionally.
  dagger -m "$module" check || exit 1
  if [ -d "${module}/tests" ]; then
    dagger -m "${module}/tests" check || exit 1
  fi
done
