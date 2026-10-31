#!/usr/bin/env bash
#
# install-dagger.sh — give a cloud session the pinned Dagger CLI, a Docker
# daemon, and an engine that can reach the network through the session's proxy.
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
#     as root with no flags runs the engine's privileged container;
#   - the engine, when the session has an agent proxy (HTTPS_PROXY). The engine
#     the CLI starts on its own knows nothing of that proxy: its git cannot
#     resolve a remote module, and nothing it runs can fetch a package. The
#     proxy listens on the host's loopback and re-terminates TLS, so this
#     starts the engine container itself, from registry.dagger.io's image at
#     the pin, on Docker's default bridge. Two things make the proxy usable
#     from there: proxy-forward.py relays the bridge gateway's PROXY_PORT to
#     the proxy, and the engine gets that address as HTTPS_PROXY, which it
#     passes to every container it runs, and the proxy's CA, which it trusts
#     and installs into them. CLAUDE_ENV_FILE then points the CLI at that
#     container, so the CLI never replaces it with one of its own.
#
#     The bridge, and not the host network, is deliberate. On the host network
#     the engine's containers reach the proxy, but not each other: a test that
#     binds a service times out dialling it.
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
#   PROXY_CA      the agent proxy's CA bundle (default /root/.ccr/ca-bundle.crt)
#   PROXY_PORT    the bridge port the proxy is relayed on (default 3129)
#   STATE_DIR     the relay's pid, target and log (default /tmp/install-dagger)
#
# Always exits 0: a session without Dagger is better than no session. A second
# run in the same session finds everything in place and says nothing. A resumed
# session gets a new proxy port and a stopped engine; the relay is pointed at
# the new port and the engine is started again, keeping its cache volume.

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
ca=${PROXY_CA:-/root/.ccr/ca-bundle.crt}
port=${PROXY_PORT:-3129}
state=${STATE_DIR:-/tmp/install-dagger}
here=$(cd "$(dirname "$0")" && pwd)

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

# session_env <line> <what> — keep <line> in CLAUDE_ENV_FILE for the rest of
# the session, once; <what> says what to do by hand when there is no such file.
session_env() {
  [ -n "${CLAUDE_ENV_FILE:-}" ] || { say "CLAUDE_ENV_FILE is not set; $2 yourself"; return; }
  grep -qxF "$1" "$CLAUDE_ENV_FILE" 2>/dev/null || printf '%s\n' "$1" >>"$CLAUDE_ENV_FILE"
}

# on_path — make BIN_DIR part of PATH for the rest of the session.
on_path() { session_env "export PATH=\"$bin:\$PATH\"" "add $bin to PATH"; }

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

# Without a proxy the engine is pulled on the first `dagger` call; with one it
# is pulled below. Either way, say now if it can't be.
if [ -n "$version" ] && ! reachable https://registry.dagger.io/v2/; then
  say "cannot reach registry.dagger.io, where the Dagger engine image is pulled from; $allow"
fi

# --- the engine -------------------------------------------------------------

proxy=${HTTPS_PROXY:-${https_proxy:-}}

# relay <bridge> <host:port> — a proxy-forward.py on <bridge>:PROXY_PORT that
# relays to <host:port>, started unless one already does exactly that.
relay() {
  local want="$1:$port $2" pid
  pid=$(cat "$state/relay.pid" 2>/dev/null)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    [ "$(cat "$state/relay.target" 2>/dev/null)" = "$want" ] && return 0
    # A resumed session's proxy moved; the old relay holds the port.
    kill "$pid" 2>/dev/null
    sleep 1
  fi
  rm -f "$state/relay.pid"
  printf '%s\n' "$want" >"$state/relay.target"
  setsid nohup python3 "$here/proxy-forward.py" "$1" "$port" "${2%:*}" "${2##*:}" "$state/relay.pid" \
    </dev/null >"$state/relay.log" 2>&1 &
  for _ in 1 2 3 4 5; do
    [ -s "$state/relay.pid" ] && return 0
    sleep 1
  done
  say "the proxy relay on $1:$port did not start; see $state/relay.log"
  return 1
}

# engine <bridge> — the engine container at the pin, using the relay as its
# proxy. A running one already configured so is left alone.
engine() {
  local name="dagger-engine-v$version" fwd="http://$1:$port" have
  # Whether it runs, on a line of its own, then its environment, one per line.
  have=$(docker inspect -f '{{println .State.Running}}{{range .Config.Env}}{{println .}}{{end}}' "$name" 2>/dev/null)
  if [ "$(head -1 <<<"$have")" = true ] && grep -qxF "HTTPS_PROXY=$fwd" <<<"$have"; then
    return 0
  fi
  docker rm -f "$name" >/dev/null 2>&1
  # The CA goes where the engine looks for extra roots: it trusts them and
  # installs them into every container it runs.
  if docker run -d --name "$name" --privileged \
    -e "HTTPS_PROXY=$fwd" -e "https_proxy=$fwd" \
    -e NO_PROXY=localhost,127.0.0.1 -e no_proxy=localhost,127.0.0.1 \
    -v "$ca:/usr/local/share/ca-certificates/agent-proxy.crt:ro" \
    -v dagger-engine-state:/var/lib/dagger \
    "registry.dagger.io/engine:v$version" >/dev/null 2>&1; then
    say "started the Dagger engine ($name) behind the session proxy, relayed on $1:$port"
  else
    say "could not start the Dagger engine container $name; try: docker run registry.dagger.io/engine:v$version"
    return 1
  fi
}

if [ -n "$version" ] && [ -n "$proxy" ] && docker info >/dev/null 2>&1; then
  target=${proxy#*://}
  target=${target%%/*}
  bridge=$(docker network inspect bridge -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' 2>/dev/null)
  if [ ! -r "$ca" ]; then
    say "no proxy CA at $ca; the Dagger engine is left to the CLI and cannot use the proxy"
  elif [ -z "$bridge" ]; then
    say "cannot read Docker's bridge gateway; the Dagger engine is left to the CLI and cannot use the proxy"
  elif ! command -v python3 >/dev/null 2>&1; then
    say "python3 is not installed, and it runs the proxy relay; the Dagger engine cannot use the proxy"
  elif mkdir -p "$state" && relay "$bridge" "$target" && engine "$bridge"; then
    session_env "export _EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://dagger-engine-v$version" \
      "export _EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://dagger-engine-v$version"
  fi
fi

exit 0
