#!/usr/bin/env bash
#
# install-plugins.sh — make a cloud session load this repository's own plugins.
#
# .claude/settings.json already declares them: `extraKnownMarketplaces` names this
# repository as the z5labs-devex marketplace and `enabledPlugins` turns its
# plugins on. A cloud session ignores both. Adding a marketplace a repository
# declares needs the workspace trust dialog, and a cloud session never shows one
# (https://code.claude.com/docs/en/plugins/loading). User settings need no trust,
# so this writes the same declaration there instead:
#
#   - the marketplace, as a `directory` source at this checkout, so the plugins
#     load in place from the branch the session is on — a pull request that
#     changes a plugin runs the changed plugin, with no version bump and no
#     `claude plugin install`;
#   - every plugin from that marketplace that .claude/settings.json enables. That
#     file stays the one list: enabling a plugin there is enough.
#
# When it runs matters. Claude Code fixes its plugin set at startup, so plugins
# declared by the SessionStart hook reach the session only after `/reload-plugins`
# or on the next start. The environment's setup script runs before Claude Code
# starts, so calling this from there loads them from the first prompt; the README
# gives the line. The hook is the fallback for environments without it.
#
# Usage:
#   install-plugins.sh             the SessionStart hook: does nothing unless
#                                  CLAUDE_CODE_REMOTE is true; the repository is
#                                  CLAUDE_PROJECT_DIR
#   install-plugins.sh <repo-root> a cloud environment's setup script, which runs
#                                  before CLAUDE_CODE_REMOTE is set
#
# Writes ${CLAUDE_CONFIG_DIR:-~/.claude}/settings.json, merging into what is there,
# and only when something changes. Always exits 0: a session without the plugins
# is better than no session.

set -uo pipefail

say() { printf 'install-plugins: %s\n' "$*"; }

if [ $# -ge 1 ]; then
  root=$1
else
  [ "${CLAUDE_CODE_REMOTE:-}" = "true" ] || exit 0
  root=${CLAUDE_PROJECT_DIR:-}
fi

[ -n "$root" ] && [ -d "$root" ] || { say "no repository root (got '${root}'); plugins not declared"; exit 0; }
root=$(cd "$root" && pwd)

command -v jq >/dev/null 2>&1 || { say "jq is not installed; plugins not declared"; exit 0; }

catalog="$root/.claude-plugin/marketplace.json"
project="$root/.claude/settings.json"
name=$(jq -er '.name' "$catalog" 2>/dev/null) || { say "cannot read the marketplace name from $catalog; plugins not declared"; exit 0; }
plugins=$(jq -ec --arg suffix "@$name" \
  '[.enabledPlugins // {} | to_entries[] | select(.value == true and (.key | endswith($suffix))) | .key]' \
  "$project" 2>/dev/null) || { say "cannot read enabledPlugins from $project; plugins not declared"; exit 0; }
[ "$plugins" != "[]" ] || { say "$project enables no plugin from $name; nothing to declare"; exit 0; }

dir=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
settings="$dir/settings.json"
current='{}'
if [ -s "$settings" ]; then
  current=$(jq -e 'if type == "object" then . else error("not an object") end' "$settings" 2>/dev/null) \
    || { say "$settings is not a JSON object; left as it is, plugins not declared"; exit 0; }
fi

merged=$(jq --arg name "$name" --arg path "$root" --argjson plugins "$plugins" '
  .extraKnownMarketplaces[$name] = {source: {source: "directory", path: $path}}
  | reduce $plugins[] as $p (.; .enabledPlugins[$p] = true)
' <<<"$current") || { say "could not merge into $settings; plugins not declared"; exit 0; }

# Unchanged means untouched: a rewrite would bump the file and could prompt a
# running session to reload for nothing.
if [ "$(jq -S . <<<"$current")" = "$(jq -S . <<<"$merged")" ]; then
  exit 0
fi

mkdir -p "$dir" || { say "cannot create $dir; plugins not declared"; exit 0; }
tmp=$(mktemp "$dir/.settings.json.XXXXXX") || { say "cannot write in $dir; plugins not declared"; exit 0; }
if printf '%s\n' "$merged" >"$tmp" && mv "$tmp" "$settings"; then
  say "declared $(jq -r 'join(", ")' <<<"$plugins") from $root; run /reload-plugins to load them in this session"
else
  rm -f "$tmp"
  say "cannot write $settings; plugins not declared"
fi
exit 0
