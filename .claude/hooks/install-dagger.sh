#!/usr/bin/env bash
#
# install-dagger.sh — give a cloud session the pinned Dagger CLI and a Docker
# daemon for its engine.
#
# A cloud session starts with no `dagger` on its PATH and no container runtime
# running, so `dagger check` — the first verify command in .claude/backlog.json —
# cannot run. This fixes both:
#
#   - the CLI, at the version pinned by the `dagger-version` default in
#     .github/workflows/change-aware-ci.yml. That default is the one pin for the
#     CLI and the engine; update-dagger.yml rewrites it, and this reads it with
#     the same extraction, so a bump reaches cloud sessions with no change here.
#     It installs with https://dl.dagger.io/dagger/install.sh, as CI does, into
#     BIN_DIR, which it puts on PATH for the rest of the session through
#     CLAUDE_ENV_FILE;
#   - the daemon. The sandbox ships dockerd but runs nothing, and dockerd started
#     as root with no flags runs the engine's privileged container. The engine
#     image comes from registry.dagger.io, the CLI's default; nothing here
#     overrides the image or the runner host.
#
# The hosts are the environment's to allow, not the repository's: a network
# policy that denies dl.dagger.io or registry.dagger.io is named in one line so
# it can be added under Network access → Allowed domains
# (https://code.claude.com/docs/en/cloud-environments#network-access).
#
# Usage: install-dagger.sh — the SessionStart hook. Does nothing unless
# CLAUDE_CODE_REMOTE is true; the repository is CLAUDE_PROJECT_DIR.
#
# Environment, for tests and odd sandboxes:
#   BIN_DIR       where the CLI goes (default ~/.local/bin)
#   DOCKERD_LOG   dockerd's log (default /tmp/dockerd.log)
#   DOCKERD_WAIT  seconds to wait for `docker info` (default 30)
#   DOCKER_RUN_DIR  dockerd's exec root (default /var/run/docker)
#
# Always exits 0: a session without Dagger is better than no session. A second
# run in the same session finds both in place and says nothing.

set -uo pipefail

say() { printf 'install-dagger: %s\n' "$*"; }

[ "${CLAUDE_CODE_REMOTE:-}" = "true" ] || exit 0

root=${CLAUDE_PROJECT_DIR:-}
[ -n "$root" ] && [ -d "$root" ] || { say "no repository root (got '${root}'); Dagger not installed"; exit 0; }

workflow="$root/.github/workflows/change-aware-ci.yml"
bin=${BIN_DIR:-$HOME/.local/bin}
log=${DOCKERD_LOG:-/tmp/dockerd.log}
wait=${DOCKERD_WAIT:-30}
run=${DOCKER_RUN_DIR:-/var/run/docker}

allow="allow it under the environment's Network access → Allowed domains"

# reachable <url> — the host answered over HTTP at all. An egress proxy that
# denies a host fails the CONNECT, which curl reports as no status (000).
reachable() {
  local code
  code=$(curl -sS -o /dev/null --max-time 10 -w '%{http_code}' "$1" 2>/dev/null)
  [ -n "$code" ] && [ "$code" != "000" ] && [ "$code" != "403" ]
}

# --- the CLI ----------------------------------------------------------------

# The extraction update-dagger.yml uses for the same line.
version=$(sed -n -E '/^      dagger-version:/,/^      [a-z-]+:/ s/.*default:[[:space:]]*"([^"]+)".*/\1/p' \
  "$workflow" 2>/dev/null | head -1)

# on_path — make BIN_DIR part of PATH for the rest of the session, once.
on_path() {
  local line="export PATH=\"$bin:\$PATH\""
  [ -n "${CLAUDE_ENV_FILE:-}" ] || { say "CLAUDE_ENV_FILE is not set; add $bin to PATH yourself"; return; }
  grep -qxF "$line" "$CLAUDE_ENV_FILE" 2>/dev/null || printf '%s\n' "$line" >>"$CLAUDE_ENV_FILE"
}

# installed — the version a `dagger` reports, preferring BIN_DIR's.
installed() {
  local d
  for d in "$bin/dagger" "$(command -v dagger 2>/dev/null)"; do
    [ -n "$d" ] && [ -x "$d" ] || continue
    "$d" version 2>/dev/null | awk '{print $2; exit}'
    return
  done
}

if [ -z "$version" ]; then
  say "cannot read the dagger-version default from $workflow; Dagger not installed"
else
  have=$(installed)
  if [ "$have" = "v$version" ]; then
    [ -x "$bin/dagger" ] && on_path
  else
    script=$(mktemp)
    if ! curl -fsSL --max-time 30 https://dl.dagger.io/dagger/install.sh -o "$script" 2>/dev/null; then
      say "cannot download https://dl.dagger.io/dagger/install.sh; if dl.dagger.io is blocked, $allow"
    elif ! mkdir -p "$bin" || ! DAGGER_VERSION="$version" BIN_DIR="$bin" sh "$script" >/dev/null 2>&1; then
      say "the Dagger $version install failed; it downloads from dl.dagger.io, so if that is blocked, $allow"
    elif [ "$("$bin/dagger" version 2>/dev/null | awk '{print $2; exit}')" != "v$version" ]; then
      say "installed Dagger to $bin, but it does not report v$version"
    else
      on_path
      say "installed Dagger $version to $bin"
    fi
    rm -f "$script"
  fi
fi

# --- the daemon -------------------------------------------------------------

if ! docker info >/dev/null 2>&1; then
  if ! command -v dockerd >/dev/null 2>&1; then
    say "no Docker daemon answers and dockerd is not installed; the Dagger engine cannot start"
  else
    # A resumed session keeps the previous container's files but not its
    # processes. dockerd then reads containerd's old pid file, takes containerd
    # for running, and exits; with no dockerd alive, that file is stale.
    pgrep -x dockerd >/dev/null 2>&1 || rm -f "$run/containerd/containerd.pid"
    # Detached and with every descriptor redirected, so the hook's exit neither
    # waits on dockerd nor takes it down.
    setsid nohup dockerd </dev/null >"$log" 2>&1 &
    up=
    for i in $(seq 0 "$wait"); do
      docker info >/dev/null 2>&1 && { up=1; break; }
      [ "$i" -lt "$wait" ] && sleep 1
    done
    if [ -n "$up" ]; then
      say "started dockerd (log: $log)"
    else
      say "dockerd did not answer within ${wait}s; see $log"
    fi
  fi
fi

# --- the engine's registry --------------------------------------------------

# The engine is pulled on the first `dagger` call, not here; say now if it can't be.
if [ -n "$version" ] && ! reachable https://registry.dagger.io/v2/; then
  say "cannot reach registry.dagger.io, where the Dagger engine image is pulled from; $allow"
fi

exit 0
