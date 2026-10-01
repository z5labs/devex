#!/usr/bin/env bash
#
# verify-affected-modules.sh — run the checks CI would run for this branch's
# daggerverse changes, from inside a worktree.
#
# The second half of `verify` in .claude/backlog.json. The first half is
# `dagger check`, which at the workspace root runs the root module's one check
# (`ci:generated`) and the Go SDK's (`dagger-go-sdk:stale`) — the two the planner
# always runs and never memoizes. This is everything else a worktree can
# reproduce.
#
# Why not just ask the planner. `.github/workflows/change-aware-ci.yml` gets its
# whole matrix from one `dagger -m daggerverse/workspace-ci call plan`, and that is the
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

# module_name prints the name a module's dagger-module.toml gives it, without
# loading the module: the top-level `name = "..."`, before the first table header.
module_name() {
  awk '/^\[/ { exit } /^name[[:space:]]*=/ { sub(/^name[[:space:]]*=[[:space:]]*"/, ""); sub(/".*$/, ""); print; exit }' "$1/dagger-module.toml"
}

# is_module reports whether a directory is a module: it holds a dagger-module.toml,
# the only module config this repository has used since #443.
is_module() {
  [ -f "$1/dagger-module.toml" ]
}

# check_module runs every check the module at $1 declares, and only those.
#
# `--module` is what keeps it to those: without it the CLI also selects the root
# module's checks and those of every module dagger.toml installs, the Go SDK's
# `stale` among them. And since Dagger
# v1.0.0-beta.15 `dagger check` fails when it selects nothing, so a module that
# declares no checks of its own is listed first and skipped, the same way a
# coarse leg in .github/workflows/change-aware-ci.yml does it. A module that
# cannot even be listed fails.
check_module() {
  local dir="$1" name links
  name=$(module_name "$dir")
  if [ -z "$name" ]; then
    echo "verify-affected-modules: cannot read the module name of ${dir}" >&2
    return 1
  fi
  links=$(dagger -m "$dir" check -l --module "$name" -f link) || return 1
  if [ -z "$links" ]; then
    echo "verify-affected-modules: ${dir} declares no checks of its own"
    return 0
  fi
  dagger -m "$dir" check --module "$name"
}

for module in $modules; do
  # Not everything two path segments deep under daggerverse/ is a module.
  # daggerverse/CLAUDE.md is a file, and `cut -f1,2` leaves it looking
  # exactly like daggerverse/<m>; passing it to `dagger -m` fails with an
  # lstat error on a module config inside a regular file, which reads as a
  # broken module rather than as a path that was never one. A change to
  # the notes file alone would fail this whole script.
  if ! is_module "$module"; then
    echo "verify-affected-modules: skipping ${module} (not a module)"
    continue
  fi
  echo "verify-affected-modules: ${module}"
  check_module "$module" || exit 1
  if is_module "${module}/tests"; then
    check_module "${module}/tests" || exit 1
  fi
done
