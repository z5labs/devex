#!/usr/bin/env bash
#
# install-plugins_test.sh — this repository's copy of install-plugins.sh.
#
# The script ships with the backlog plugin as
# plugins/backlog/assets/install-plugins.sh, which `backlog:setup-backlog` copies
# into other repositories. This copy keeps its path because existing cloud
# environments' setup scripts call it here. The two must stay byte-identical: a
# fix made to one and not the other would reach either this repository's sessions
# or everyone else's, not both. So this fails on any difference, then runs the
# asset's cases against this copy.
#
# Run: .claude/hooks/install-plugins_test.sh
# Exit 0 when the copies match and every case passes, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
COPY="$HERE/install-plugins.sh"
ROOT=$(cd "$HERE/../.." && pwd)
ASSET="$ROOT/plugins/backlog/assets/install-plugins.sh"
SUITE="$ROOT/plugins/backlog/scripts/install-plugins_test.sh"

for f in "$COPY" "$ASSET" "$SUITE"; do
  [ -f "$f" ] || { printf 'missing %s\n' "$f" >&2; exit 1; }
done

if ! cmp -s "$COPY" "$ASSET"; then
  printf '  FAIL .claude/hooks/install-plugins.sh has drifted from plugins/backlog/assets/install-plugins.sh\n'
  diff -u "$ASSET" "$COPY" | sed 's/^/       /'
  printf '\n       Change the asset, then copy it over this file.\n'
  exit 1
fi
printf '  ok   .claude/hooks/install-plugins.sh is identical to the backlog asset\n'

exec "$SUITE" "$COPY"
