#!/usr/bin/env bash
#
# install-plugins_test.sh — the cases for install-plugins.sh.
#
# Offline. Each case builds a fixture repository (a marketplace catalog and a
# .claude/settings.json) and a scratch CLAUDE_CONFIG_DIR, runs the script, and
# asserts on the user settings it leaves behind. Nothing here runs `claude`.
#
# Run: .claude/hooks/install-plugins_test.sh
# Exit 0 when every case passes, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/install-plugins.sh"
[ -x "$SUT" ] || { printf 'install-plugins.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

ok() { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL %s\n       %s\n' "$1" "$2"; }

# fixture <dir> — a repository whose marketplace is `acme`, enabling two of its
# plugins, disabling a third, and enabling one from another marketplace.
fixture() {
  mkdir -p "$1/.claude-plugin" "$1/.claude"
  printf '{"name": "acme", "plugins": []}\n' >"$1/.claude-plugin/marketplace.json"
  cat >"$1/.claude/settings.json" <<'EOF'
{
  "enabledPlugins": {
    "alpha@acme": true,
    "beta@acme": true,
    "gamma@acme": false,
    "delta@elsewhere": true
  }
}
EOF
}

# run <config dir> [args...] — the script as a cloud SessionStart hook would run
# it, unless the caller overrides the environment.
run() {
  local cfg=$1; shift
  CLAUDE_CONFIG_DIR="$cfg" "$SUT" "$@"
}

printf '\nwhen it runs\n'

repo="$WORK/r1"; cfg="$WORK/c1"; fixture "$repo"
out=$(env -u CLAUDE_CODE_REMOTE CLAUDE_PROJECT_DIR="$repo" CLAUDE_CONFIG_DIR="$cfg" "$SUT"; echo "exit=$?")
if [ ! -e "$cfg/settings.json" ] && [ "$out" = "exit=0" ]; then
  ok 'outside a cloud session, the hook does nothing'
else
  bad 'outside a cloud session, the hook does nothing' "output: $out"
fi

repo="$WORK/r2"; cfg="$WORK/c2"; fixture "$repo"
env -u CLAUDE_CODE_REMOTE CLAUDE_CONFIG_DIR="$cfg" "$SUT" "$repo" >/dev/null
if [ -s "$cfg/settings.json" ]; then
  ok 'given a repository root, it runs without CLAUDE_CODE_REMOTE (setup script)'
else
  bad 'given a repository root, it runs without CLAUDE_CODE_REMOTE (setup script)' 'no settings written'
fi

printf '\nwhat it declares\n'

repo="$WORK/r3"; cfg="$WORK/c3"; fixture "$repo"
out=$(CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$repo" run "$cfg")
got=$(jq -c '{m: .extraKnownMarketplaces, p: .enabledPlugins}' "$cfg/settings.json" 2>/dev/null)
want=$(jq -nc --arg p "$repo" '{m: {acme: {source: {source: "directory", path: $p}}}, p: {"alpha@acme": true, "beta@acme": true}}')
if [ "$got" = "$want" ]; then
  ok 'the marketplace at the checkout, and exactly the plugins it enables'
else
  bad 'the marketplace at the checkout, and exactly the plugins it enables' "want $want got $got"
fi
case $out in
  *alpha@acme*beta@acme*reload-plugins*) ok 'says what it declared and how to load it now' ;;
  *) bad 'says what it declared and how to load it now' "output: $out" ;;
esac

before=$(stat -c '%Y %i' "$cfg/settings.json"); sleep 1
out=$(CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$repo" run "$cfg")
after=$(stat -c '%Y %i' "$cfg/settings.json")
if [ "$before" = "$after" ] && [ -z "$out" ]; then
  ok 'a second run leaves the file untouched and says nothing'
else
  bad 'a second run leaves the file untouched and says nothing' "before $before after $after output: $out"
fi

repo="$WORK/r4"; cfg="$WORK/c4"; fixture "$repo"; mkdir -p "$cfg"
printf '{"model": "x", "enabledPlugins": {"mine@other": true}, "extraKnownMarketplaces": {"other": {"source": {"source": "github", "repo": "o/r"}}}}\n' >"$cfg/settings.json"
CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$repo" run "$cfg" >/dev/null
got=$(jq -c '[.model, .enabledPlugins["mine@other"], .extraKnownMarketplaces.other.source.repo, .enabledPlugins["alpha@acme"]]' "$cfg/settings.json")
if [ "$got" = '["x",true,"o/r",true]' ]; then
  ok 'existing user settings are kept'
else
  bad 'existing user settings are kept' "got $got"
fi

printf '\nwhen it cannot\n'

repo="$WORK/r5"; cfg="$WORK/c5"; fixture "$repo"; mkdir -p "$cfg"
printf 'not json\n' >"$cfg/settings.json"
out=$(CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$repo" run "$cfg"; echo "exit=$?")
if [ "$(cat "$cfg/settings.json")" = "not json" ] && [[ $out == *"not a JSON object"*exit=0 ]]; then
  ok 'unreadable user settings are left alone, said, and exit 0'
else
  bad 'unreadable user settings are left alone, said, and exit 0' "output: $out"
fi

repo="$WORK/r6"; cfg="$WORK/c6"; fixture "$repo"; rm "$repo/.claude-plugin/marketplace.json"
out=$(CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$repo" run "$cfg"; echo "exit=$?")
if [ ! -e "$cfg/settings.json" ] && [[ $out == *"marketplace name"*exit=0 ]]; then
  ok 'no marketplace catalog: nothing written, said, and exit 0'
else
  bad 'no marketplace catalog: nothing written, said, and exit 0' "output: $out"
fi

repo="$WORK/r7"; cfg="$WORK/c7"; fixture "$repo"
printf '{"enabledPlugins": {"delta@elsewhere": true}}\n' >"$repo/.claude/settings.json"
out=$(CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$repo" run "$cfg"; echo "exit=$?")
if [ ! -e "$cfg/settings.json" ] && [[ $out == *"enables no plugin"*exit=0 ]]; then
  ok 'no plugin enabled from this marketplace: nothing written'
else
  bad 'no plugin enabled from this marketplace: nothing written' "output: $out"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
