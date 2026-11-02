#!/usr/bin/env bash
#
# install-plugins.sh — make a Claude Code cloud session load the plugins a
# repository enables.
#
# A repository's .claude/settings.json names its plugins: `extraKnownMarketplaces`
# declares where they come from and `enabledPlugins` turns them on. A cloud
# session ignores both. Adding a marketplace a repository declares needs the
# workspace trust dialog, and a cloud session never shows one
# (https://code.claude.com/docs/en/plugins/loading). User settings need no trust,
# so this writes the same declaration there instead:
#
#   - every marketplace .claude/settings.json declares, with the source it
#     declares. A relative `directory` or `file` path is anchored at the checkout,
#     because user settings do not live in the repository;
#   - when the repository is itself a marketplace (it has
#     .claude-plugin/marketplace.json), that marketplace as a `directory` source
#     at the checkout, in place of any source the settings give it, so its
#     plugins load from the branch the session is on — a pull request that
#     changes a plugin runs the changed plugin, with no version bump and no
#     `claude plugin install`;
#   - every plugin .claude/settings.json enables from one of those marketplaces.
#     That file stays the one list: enabling a plugin there is enough.
#
# When it runs matters. Claude Code fixes its plugin set at startup, so plugins
# declared by the SessionStart hook may reach the session only after
# `/reload-plugins`. The environment's setup script runs before Claude Code
# starts, so calling this from there loads them from the first prompt.
#
# Writing the settings is enough for a `directory` marketplace, whose plugins load
# in place. It is not enough for one Claude Code has to fetch: observed with
# Claude Code 2.1.316 on a fresh configuration, the first start registers a
# `github` marketplace that settings declare, but installs none of its plugins
# until the next start, and a cloud container is fresh every session. So from the
# setup script, outside a running Claude Code, this also registers each `github`,
# `git` or `url` marketplace that a plugin is enabled from, with
# `claude plugin marketplace add`, and installs those plugins with
# `claude plugin install`. The session then starts with them installed. A
# SessionStart hook does not do this: it runs inside a starting session, which
# owns those files.
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
  mode=setup
else
  [ "${CLAUDE_CODE_REMOTE:-}" = "true" ] || exit 0
  root=${CLAUDE_PROJECT_DIR:-}
  mode=hook
fi

[ -n "$root" ] && [ -d "$root" ] || { say "no repository root (got '${root}'); plugins not declared"; exit 0; }
root=$(cd "$root" && pwd)

command -v jq >/dev/null 2>&1 || { say "jq is not installed; plugins not declared"; exit 0; }

catalog="$root/.claude-plugin/marketplace.json"
project="$root/.claude/settings.json"

# The repository's own marketplace, when it is one.
self=
if [ -e "$catalog" ]; then
  self=$(jq -er '.name | strings | select(length > 0)' "$catalog" 2>/dev/null) || {
    say "cannot read the marketplace name from $catalog; treating $root as not being a marketplace"
    self=
  }
fi

jq -e 'type == "object"' "$project" >/dev/null 2>&1 \
  || { say "cannot read $project as a JSON object; plugins not declared"; exit 0; }

# The plan: the marketplaces to declare, the plugins to enable, and the declared
# entries that carry no source to copy.
plan=$(jq -ec --arg self "$self" --arg root "$root" '
  def anchored:
    if (.source.source == "directory" or .source.source == "file")
       and (.source.path | type) == "string"
       and (.source.path | startswith("/") | not)
    then .source.path = ($root + "/" + (.source.path | ltrimstr("./")))
    else . end;
  (.extraKnownMarketplaces | if type == "object" then . else {} end) as $declared
  | (($declared | with_entries(select((.value | type) == "object" and (.value.source | type) == "object"))
                | map_values(anchored))
     + (if $self == "" then {} else {($self): {source: {source: "directory", path: $root}}} end)) as $markets
  | {
      markets: $markets,
      unusable: [$declared | to_entries[]
                 | select((.value | type) != "object" or (.value.source | type) != "object") | .key],
      plugins: [.enabledPlugins | if type == "object" then to_entries[] else empty end
                | select(.value == true) | .key
                | select(test("@") and $markets[sub("^.*@"; "")] != null)]
    }
' "$project" 2>/dev/null) || { say "cannot read the marketplaces and plugins in $project; plugins not declared"; exit 0; }

markets=$(jq -c '.markets' <<<"$plan")
plugins=$(jq -c '.plugins' <<<"$plan")

while IFS= read -r name; do
  [ -n "$name" ] && say "skipping marketplace $name: $project gives it no source"
done < <(jq -r '.unusable[]' <<<"$plan")

[ "$markets" != "{}" ] || { say "$root is not a marketplace and $project declares none; nothing to declare"; exit 0; }
[ "$plugins" != "[]" ] || {
  say "$project enables no plugin from $(jq -r 'keys | join(", ")' <<<"$markets"); nothing to declare"
  exit 0
}

dir=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
settings="$dir/settings.json"
current='{}'
if [ -s "$settings" ]; then
  current=$(jq -e 'if type == "object" then . else error("not an object") end' "$settings" 2>/dev/null) \
    || { say "$settings is not a JSON object; left as it is, plugins not declared"; exit 0; }
fi

merged=$(jq --argjson markets "$markets" --argjson plugins "$plugins" '
  .extraKnownMarketplaces = ((.extraKnownMarketplaces // {}) + $markets)
  | reduce $plugins[] as $p (.; .enabledPlugins[$p] = true)
' <<<"$current" 2>/dev/null) || { say "could not merge into $settings; plugins not declared"; exit 0; }

# Unchanged means untouched: a rewrite would bump the file and could prompt a
# running session to reload for nothing.
if [ "$(jq -S . <<<"$current")" != "$(jq -S . <<<"$merged")" ]; then
  mkdir -p "$dir" || { say "cannot create $dir; plugins not declared"; exit 0; }
  tmp=$(mktemp "$dir/.settings.json.XXXXXX") || { say "cannot write in $dir; plugins not declared"; exit 0; }
  if printf '%s\n' "$merged" >"$tmp" && mv "$tmp" "$settings"; then
    from=$(jq -r 'to_entries | map("\(.key) (" + ([.value.source.source, (.value.source.repo // .value.source.url // .value.source.path // .value.source.package // empty)] | map(tostring) | join(" ")) + ")") | join(", ")' <<<"$markets")
    if [ "$mode" = hook ]; then
      say "declared $(jq -r 'join(", ")' <<<"$plugins") from $from; run /reload-plugins to load them in this session"
    else
      say "declared $(jq -r 'join(", ")' <<<"$plugins") from $from"
    fi
  else
    rm -f "$tmp"
    say "cannot write $settings; plugins not declared"
    exit 0
  fi
fi

# Registering ahead of startup is for the setup script alone. Inside a running
# Claude Code — a SessionStart hook, or this run by hand from a session — the
# session owns the plugin files, and Claude Code registers what the settings
# declare when it next starts.
[ "$mode" = setup ] && [ "${CLAUDECODE:-}" != "1" ] || exit 0

# One line per marketplace that a plugin is enabled from and that Claude Code
# would have to fetch: its name, its source type, then the source as
# `claude plugin marketplace add` takes it, or nothing when the declared source has
# more to it than that command can express (a `ref`, a `path`, sparse paths,
# another source type). The column that can be empty comes last, because a tab is
# whitespace to `read` and two in a row would collapse into one.
pending=$(jq -r --argjson plugins "$plugins" '
  to_entries[]
  | .key as $m
  | select(any($plugins[]; sub("^.*@"; "") == $m))
  | select(.value.source.source != "directory")
  | .value.source as $s
  | [$m,
     ($s.source | tostring),
     (if $s.source == "github" and ($s.repo | type) == "string" and (($s | keys) - ["source", "repo"]) == [] then $s.repo
      elif ($s.source == "git" or $s.source == "url") and ($s.url | type) == "string" and (($s | keys) - ["source", "url"]) == [] then $s.url
      else "" end)]
  | @tsv
' <<<"$markets")
[ -n "$pending" ] || exit 0

if ! command -v claude >/dev/null 2>&1; then
  say "claude is not on PATH, so these are not registered before Claude Code starts: $(cut -f1 <<<"$pending" | paste -sd, - | sed 's/,/, /g'); the session registers them itself, and their plugins may need /reload-plugins there"
  exit 0
fi

# claude_cli <args...> — the CLI, bounded, with no terminal to wait on.
claude_cli() {
  if command -v timeout >/dev/null 2>&1; then
    timeout 180 claude "$@" </dev/null 2>&1
  else
    claude "$@" </dev/null 2>&1
  fi
}

while IFS=$'\t' read -r name kind source; do
  [ -n "$name" ] || continue
  if [ -z "$source" ]; then
    say "cannot register $name before Claude Code starts: \`claude plugin marketplace add\` cannot take its $kind source as declared; the session registers it itself, and its plugins may need /reload-plugins there"
    continue
  fi
  if ! out=$(claude_cli plugin marketplace add "$source" --scope user); then
    say "could not register $name from $source before Claude Code starts: $(tail -n 1 <<<"$out"); the session registers it itself, and its plugins may need /reload-plugins there"
    continue
  fi
  installed=()
  while IFS= read -r plugin; do
    if out=$(claude_cli plugin install "$plugin" --scope user); then
      installed+=("$plugin")
    else
      say "could not install $plugin before Claude Code starts: $(tail -n 1 <<<"$out")"
    fi
  done < <(jq -r --arg m "$name" '.[] | select(sub("^.*@"; "") == $m)' <<<"$plugins")
  if [ "${#installed[@]}" -gt 0 ]; then
    say "registered $name and installed $(IFS=,; printf '%s' "${installed[*]}" | sed 's/,/, /g') before Claude Code starts"
  fi
done <<<"$pending"
exit 0
