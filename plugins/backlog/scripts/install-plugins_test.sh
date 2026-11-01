#!/usr/bin/env bash
#
# install-plugins_test.sh — the cases for assets/install-plugins.sh.
#
# Offline. Each case builds a fixture repository (a .claude/settings.json, and a
# marketplace catalog when the repository is meant to be one) and a scratch
# CLAUDE_CONFIG_DIR, runs the script, and asserts on the user settings it leaves
# behind and on what it asked `claude` to do. `claude` is a fake on PATH that
# records its arguments; nothing here runs the real CLI or touches the network.
#
# Run: plugins/backlog/scripts/install-plugins_test.sh [script]
# The script under test defaults to the asset; .claude/hooks/install-plugins_test.sh
# passes this repository's hook copy instead.
# Exit 0 when every case passes, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT=${1:-$HERE/../assets/install-plugins.sh}
[ -x "$SUT" ] || { printf 'install-plugins.sh is not executable at %s\n' "$SUT" >&2; exit 1; }
SUT=$(cd "$(dirname "$SUT")" && pwd)/$(basename "$SUT")
command -v jq >/dev/null 2>&1 || { printf 'jq is not on PATH\n' >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

ok() { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL %s\n       %s\n' "$1" "$2"; }
check() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want $3 got $2"; fi; }

# The fake `claude`: appends its arguments to $CLAUDE_CALLS, one call per line,
# and exits with $FAKE_CLAUDE_EXIT (default 0) after printing a line to say so.
FAKE="$WORK/fakebin"
mkdir -p "$FAKE"
cat >"$FAKE/claude" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${CLAUDE_CALLS:?}"
if [ "${FAKE_CLAUDE_EXIT:-0}" != 0 ]; then
  printf 'fake claude: refused %s\n' "$*"
  exit "$FAKE_CLAUDE_EXIT"
fi
printf 'fake claude: ok\n'
EOF
chmod +x "$FAKE/claude"

# A PATH with the tools the script uses and no `claude` at all.
NOCLAUDE="$WORK/noclaude"
mkdir -p "$NOCLAUDE"
for tool in bash env jq mktemp mv rm mkdir cat tail cut paste sed timeout; do
  real=$(command -v "$tool") && ln -s "$real" "$NOCLAUDE/$tool"
done

# self <dir> — a repository that is the marketplace `acme`, enabling two of its
# plugins, disabling a third, and enabling one from an undeclared marketplace.
self() {
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

# declares <dir> — a repository that is not a marketplace and declares two: `ext`
# from GitHub, with one plugin enabled, and `spare`, with none.
declares() {
  mkdir -p "$1/.claude"
  cat >"$1/.claude/settings.json" <<'EOF'
{
  "extraKnownMarketplaces": {
    "ext": {"source": {"source": "github", "repo": "o/ext"}},
    "spare": {"source": {"source": "git", "url": "https://example.com/spare.git"}}
  },
  "enabledPlugins": {
    "backlog@ext": true,
    "other@ext": false,
    "delta@elsewhere": true
  }
}
EOF
}

# hook <repo> <config dir> — the script as a cloud SessionStart hook runs it.
hook() {
  CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$1" CLAUDE_CONFIG_DIR="$2" CLAUDECODE=1 \
    CLAUDE_CALLS="$2.calls" PATH="$FAKE:$PATH" "$SUT"
}

# setup <repo> <config dir> — the script as an environment's setup script runs
# it: a repository root as the argument, no Claude Code running.
setup() {
  env -u CLAUDE_CODE_REMOTE -u CLAUDECODE CLAUDE_CONFIG_DIR="$2" \
    CLAUDE_CALLS="$2.calls" PATH="$FAKE:$PATH" "$SUT" "$1"
}

calls() { cat "$1.calls" 2>/dev/null; }
user() { jq -c "$2" "$1/settings.json" 2>/dev/null; }

printf '\nwhen it runs\n'

repo="$WORK/r1"; cfg="$WORK/c1"; self "$repo"
out=$(env -u CLAUDE_CODE_REMOTE CLAUDE_PROJECT_DIR="$repo" CLAUDE_CONFIG_DIR="$cfg" "$SUT"; echo "exit=$?")
if [ ! -e "$cfg/settings.json" ] && [ "$out" = "exit=0" ]; then
  ok 'outside a cloud session, the hook does nothing'
else
  bad 'outside a cloud session, the hook does nothing' "output: $out"
fi

repo="$WORK/r2"; cfg="$WORK/c2"; self "$repo"
setup "$repo" "$cfg" >/dev/null
if [ -s "$cfg/settings.json" ]; then
  ok 'given a repository root, it runs without CLAUDE_CODE_REMOTE (setup script)'
else
  bad 'given a repository root, it runs without CLAUDE_CODE_REMOTE (setup script)' 'no settings written'
fi

printf '\na repository that is the marketplace\n'

repo="$WORK/r3"; cfg="$WORK/c3"; self "$repo"
out=$(hook "$repo" "$cfg")
got=$(user "$cfg" '{m: .extraKnownMarketplaces, p: .enabledPlugins}')
want=$(jq -nc --arg p "$repo" '{m: {acme: {source: {source: "directory", path: $p}}}, p: {"alpha@acme": true, "beta@acme": true}}')
check 'the marketplace at the checkout, and exactly the plugins it enables' "$got" "$want"
case $out in
  *alpha@acme*beta@acme*reload-plugins*) ok 'says what it declared and how to load it now' ;;
  *) bad 'says what it declared and how to load it now' "output: $out" ;;
esac

before=$(stat -c '%Y %i' "$cfg/settings.json"); sleep 1
out=$(hook "$repo" "$cfg")
after=$(stat -c '%Y %i' "$cfg/settings.json")
if [ "$before" = "$after" ] && [ -z "$out" ]; then
  ok 'a second run leaves the file untouched and says nothing'
else
  bad 'a second run leaves the file untouched and says nothing' "before $before after $after output: $out"
fi

repo="$WORK/r4"; cfg="$WORK/c4"; self "$repo"; mkdir -p "$cfg"
printf '{"model": "x", "enabledPlugins": {"mine@other": true}, "extraKnownMarketplaces": {"other": {"source": {"source": "github", "repo": "o/r"}}}}\n' >"$cfg/settings.json"
hook "$repo" "$cfg" >/dev/null
check 'existing user settings are kept' \
  "$(user "$cfg" '[.model, .enabledPlugins["mine@other"], .extraKnownMarketplaces.other.source.repo, .enabledPlugins["alpha@acme"]]')" \
  '["x",true,"o/r",true]'

repo="$WORK/r5"; cfg="$WORK/c5"; self "$repo"
jq '.extraKnownMarketplaces = {acme: {source: {source: "github", repo: "z/acme"}}}' "$repo/.claude/settings.json" >"$repo/s" && mv "$repo/s" "$repo/.claude/settings.json"
setup "$repo" "$cfg" >/dev/null
check 'its own catalog wins over a declaration of the same name: the checkout, not the remote' \
  "$(user "$cfg" '.extraKnownMarketplaces.acme.source')" \
  "$(jq -nc --arg p "$repo" '{source: "directory", path: $p}')"
check 'a directory marketplace is not registered ahead of startup' "$(calls "$cfg")" ''

printf '\na repository that declares a marketplace\n'

repo="$WORK/r6"; cfg="$WORK/c6"; declares "$repo"
out=$(hook "$repo" "$cfg")
check 'each declared source is copied as declared' \
  "$(user "$cfg" '.extraKnownMarketplaces')" \
  '{"ext":{"source":{"source":"github","repo":"o/ext"}},"spare":{"source":{"source":"git","url":"https://example.com/spare.git"}}}'
check 'exactly the plugins it enables from them' "$(user "$cfg" '.enabledPlugins')" '{"backlog@ext":true}'
case $out in
  *backlog@ext*"github o/ext"*reload-plugins*) ok 'says what it declared, with the source' ;;
  *) bad 'says what it declared, with the source' "output: $out" ;;
esac
check 'the hook never runs claude' "$(calls "$cfg")" ''

repo="$WORK/r7"; cfg="$WORK/c7"; mkdir -p "$repo/.claude"
cat >"$repo/.claude/settings.json" <<'EOF'
{
  "extraKnownMarketplaces": {
    "near": {"source": {"source": "directory", "path": "./tools/market"}},
    "abs": {"source": {"source": "directory", "path": "/opt/market"}}
  },
  "enabledPlugins": {"p@near": true, "q@abs": true}
}
EOF
hook "$repo" "$cfg" >/dev/null
check 'a relative directory source is anchored at the checkout, an absolute one kept' \
  "$(user "$cfg" '[.extraKnownMarketplaces.near.source.path, .extraKnownMarketplaces.abs.source.path]')" \
  "$(jq -nc --arg p "$repo/tools/market" '[$p, "/opt/market"]')"

repo="$WORK/r8"; cfg="$WORK/c8"; declares "$repo"
jq '.extraKnownMarketplaces.broken = {"url": "nowhere"} | .enabledPlugins["x@broken"] = true' "$repo/.claude/settings.json" >"$repo/s" && mv "$repo/s" "$repo/.claude/settings.json"
out=$(hook "$repo" "$cfg")
if [ "$(user "$cfg" '.extraKnownMarketplaces | has("broken")')" = false ] \
  && [ "$(user "$cfg" '.enabledPlugins | has("x@broken")')" = false ] \
  && [[ $out == *"skipping marketplace broken"* ]]; then
  ok 'a declared marketplace with no source is skipped, and said'
else
  bad 'a declared marketplace with no source is skipped, and said' "output: $out settings: $(user "$cfg" .)"
fi

printf '\nregistering ahead of startup (the setup script)\n'

repo="$WORK/r9"; cfg="$WORK/c9"; declares "$repo"
out=$(setup "$repo" "$cfg")
check 'the github marketplace is added and its enabled plugin installed, into user scope' \
  "$(calls "$cfg")" \
  $'plugin marketplace add o/ext --scope user\nplugin install backlog@ext --scope user'
case $out in
  *"registered ext and installed backlog@ext"*) ok 'says what it registered' ;;
  *) bad 'says what it registered' "output: $out" ;;
esac
if [[ $out != *reload-plugins* ]]; then
  ok 'the setup script is not told to /reload-plugins'
else
  bad 'the setup script is not told to /reload-plugins' "output: $out"
fi

rm -f "$cfg.calls"
setup "$repo" "$cfg" >/dev/null
check 'a second setup run registers again, which the CLI treats as a no-op' "$(calls "$cfg" | wc -l | tr -d ' ')" 2

repo="$WORK/r10"; cfg="$WORK/c10"; declares "$repo"
CLAUDECODE=1 CLAUDE_CONFIG_DIR="$cfg" CLAUDE_CALLS="$cfg.calls" PATH="$FAKE:$PATH" "$SUT" "$repo" >/dev/null
check 'inside a running Claude Code, a repository root still registers nothing' "$(calls "$cfg")" ''
check '... but still declares' "$(user "$cfg" '.enabledPlugins')" '{"backlog@ext":true}'

repo="$WORK/r11"; cfg="$WORK/c11"; declares "$repo"
out=$(FAKE_CLAUDE_EXIT=1 setup "$repo" "$cfg"; echo "exit=$?")
if [ "$(user "$cfg" '.enabledPlugins')" = '{"backlog@ext":true}' ] \
  && [[ $out == *"could not register ext from o/ext"*"refused plugin marketplace add"*exit=0 ]]; then
  ok 'a refused registration is said, the settings stay written, and exit 0'
else
  bad 'a refused registration is said, the settings stay written, and exit 0' "output: $out"
fi

repo="$WORK/r12"; cfg="$WORK/c12"; declares "$repo"
out=$(env -u CLAUDE_CODE_REMOTE -u CLAUDECODE CLAUDE_CONFIG_DIR="$cfg" PATH="$NOCLAUDE" "$SUT" "$repo"; echo "exit=$?")
if [ "$(user "$cfg" '.enabledPlugins')" = '{"backlog@ext":true}' ] && [[ $out == *"claude is not on PATH"*ext*exit=0 ]]; then
  ok 'no claude on PATH: declared anyway, said, and exit 0'
else
  bad 'no claude on PATH: declared anyway, said, and exit 0' "output: $out"
fi

repo="$WORK/r13"; cfg="$WORK/c13"; declares "$repo"
jq '.extraKnownMarketplaces.ext.source.ref = "v1"' "$repo/.claude/settings.json" >"$repo/s" && mv "$repo/s" "$repo/.claude/settings.json"
out=$(setup "$repo" "$cfg")
if [ -z "$(calls "$cfg")" ] && [ "$(user "$cfg" '.extraKnownMarketplaces.ext.source.ref')" = '"v1"' ] \
  && [[ $out == *"cannot register ext"*github* ]]; then
  ok 'a source the CLI cannot take as declared (a ref) is copied, not registered, and said'
else
  bad 'a source the CLI cannot take as declared (a ref) is copied, not registered, and said' "calls: $(calls "$cfg") output: $out"
fi

printf '\nwhen it cannot\n'

repo="$WORK/r14"; cfg="$WORK/c14"; self "$repo"; mkdir -p "$cfg"
printf 'not json\n' >"$cfg/settings.json"
out=$(hook "$repo" "$cfg"; echo "exit=$?")
if [ "$(cat "$cfg/settings.json")" = "not json" ] && [[ $out == *"not a JSON object"*exit=0 ]]; then
  ok 'unreadable user settings are left alone, said, and exit 0'
else
  bad 'unreadable user settings are left alone, said, and exit 0' "output: $out"
fi

repo="$WORK/r15"; cfg="$WORK/c15"; self "$repo"; rm "$repo/.claude-plugin/marketplace.json"
out=$(hook "$repo" "$cfg"; echo "exit=$?")
if [ ! -e "$cfg/settings.json" ] && [[ $out == *"not a marketplace"*"declares none"*exit=0 ]]; then
  ok 'neither a marketplace nor declaring one: nothing written, said, and exit 0'
else
  bad 'neither a marketplace nor declaring one: nothing written, said, and exit 0' "output: $out"
fi

repo="$WORK/r16"; cfg="$WORK/c16"; self "$repo"
printf '{"enabledPlugins": {"delta@elsewhere": true}}\n' >"$repo/.claude/settings.json"
out=$(hook "$repo" "$cfg"; echo "exit=$?")
if [ ! -e "$cfg/settings.json" ] && [[ $out == *"enables no plugin from acme"*exit=0 ]]; then
  ok 'no plugin enabled from any of its marketplaces: nothing written'
else
  bad 'no plugin enabled from any of its marketplaces: nothing written' "output: $out"
fi

repo="$WORK/r17"; cfg="$WORK/c17"; declares "$repo"; printf '{"enabledPlugins": ' >"$repo/.claude/settings.json"
out=$(setup "$repo" "$cfg"; echo "exit=$?")
if [ ! -e "$cfg/settings.json" ] && [ -z "$(calls "$cfg")" ] && [[ $out == *"cannot read"*settings.json*exit=0 ]]; then
  ok 'unreadable repository settings: nothing written, nothing run, said, and exit 0'
else
  bad 'unreadable repository settings: nothing written, nothing run, said, and exit 0' "output: $out"
fi

repo="$WORK/r18"; cfg="$WORK/c18"; self "$repo"; printf '{"plugins": []}\n' >"$repo/.claude-plugin/marketplace.json"
jq '.extraKnownMarketplaces = {acme: {source: {source: "github", repo: "z/acme"}}}' "$repo/.claude/settings.json" >"$repo/s" && mv "$repo/s" "$repo/.claude/settings.json"
out=$(hook "$repo" "$cfg"; echo "exit=$?")
if [ "$(user "$cfg" '.extraKnownMarketplaces.acme.source.repo')" = '"z/acme"' ] && [[ $out == *"cannot read the marketplace name"*exit=0 ]]; then
  ok 'a catalog with no name: said, and the declared source is used instead'
else
  bad 'a catalog with no name: said, and the declared source is used instead' "output: $out"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
