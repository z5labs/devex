#!/usr/bin/env bash
# Regenerate every module's committed Dagger bindings at a target engine version.
#
# This is the Dagger v1 replacement for `dagger develop -m <dir>`, which no
# longer exists: v1 regenerates through `dagger generate`, which is driven by a
# workspace's dagger.toml, and this repository's modules are still configured the
# legacy way (a dagger.json per module, which v1 reads by inference). The API
# underneath develop is still there, so the same work is one query per module:
# ModuleSource.withEngineVersion bumps the pin and generatedContextDirectory runs
# codegen, exported back over the repository it was read from.
#
# Order is leaf -> examples -> tests -> root, for the same reason `dagger
# develop` needed it: a module regenerated before its dependencies embeds their
# stale type definitions.
#
# usage: hack/regen.sh [version]      (default: the version pinned in dagger.json)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

version="${1:-}"
if [ -z "$version" ]; then
  version="$(sed -n 's/.*"engineVersion": *"\([^"]*\)".*/\1/p' dagger.json | head -1)"
fi
echo "regenerating against ${version}"

regen() {
  local dir="$1"
  [ -f "${dir}/dagger.json" ] || return 0
  printf 'regen %s\n' "$dir"
  printf '{ moduleSource(refString: "%s") { withEngineVersion(version: "%s") { generatedContextDirectory { export(path: "%s") } } } }' \
    "$dir" "$version" "$root" \
    | dagger api query --silent > /dev/null
}

for d in daggerverse/*/; do regen "${d%/}"; done
for d in daggerverse/*/examples/*/; do regen "${d%/}"; done
for d in daggerverse/*/tests/; do regen "${d%/}"; done
for d in examples/*/; do regen "${d%/}"; done
for d in examples/*/ci/; do regen "${d%/}"; done
regen .
