#!/usr/bin/env bash
#
# install-dagger_test.sh — the cases for install-dagger.sh.
#
# Offline. Each case builds a fixture repository holding a change-aware-ci.yml,
# and a stub directory that shadows `curl`, `docker`, `dockerd` and, where the
# case needs one, `dagger`. Every stub records its arguments to a calls file, so
# a case asserts on what the script asked for as well as what it said. Nothing
# here touches the network or a real daemon. The engine cases also shadow
# `python3`, whose stub stands in for the proxy relay by sleeping under the pid
# it records.
#
# Run: .claude/hooks/install-dagger_test.sh
# Exit 0 when every case passes, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/install-dagger.sh"
[ -x "$SUT" ] || { printf 'install-dagger.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

WORK=$(mktemp -d)
# A relay stub outlives the hook that started it, as the real relay does.
trap 'for p in "$WORK"/*/state/relay.pid; do [ -s "$p" ] && kill "$(cat "$p")" 2>/dev/null; done; rm -rf "$WORK"' EXIT

pass=0
fail=0

ok() { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL %s\n       %s\n' "$1" "$2"; }

# workflow <dir> <version> — a change-aware-ci.yml pinning <version>, shaped like
# the real one: the default sits below a multi-line description, and the input
# after it has a default of its own that must not be read instead.
workflow() {
  mkdir -p "$1/.github/workflows"
  cat >"$1/.github/workflows/change-aware-ci.yml" <<EOF
on:
  workflow_call:
    inputs:
      dagger-version:
        description: >-
          The Dagger CLI and engine version. A prerelease is spelled the way the
          release is, with no leading v.
        type: string
        default: "$2"
      go-version:
        type: string
        default: "1.25"
EOF
}

# stubs <dir> [dagger-version] — curl, docker and dockerd that log and succeed;
# docker answers `info`. With a version, a `dagger` reporting it as the real CLI
# does: `dagger v<version> (<engine image>) <platform>`.
stubs() {
  local dir=$1 calls=$1/calls
  mkdir -p "$dir"
  : >"$calls"
  for cmd in curl docker dockerd; do
    printf '#!/bin/sh\necho "%s $*" >>"%s"\n' "$cmd" "$calls" >"$dir/$cmd"
  done
  # curl -w '%{http_code}' prints the status, as a reachable registry would.
  printf 'case "$*" in *http_code*) printf 401 ;; esac\n' >>"$dir/curl"
  if [ $# -ge 2 ]; then
    printf '#!/bin/sh\necho "dagger $*" >>"%s"\necho "dagger v%s (registry.dagger.io/engine:v%s) linux/amd64"\n' \
      "$calls" "$2" "$2" >"$dir/dagger"
  fi
  chmod +x "$dir"/*
}

# run <repo> <stubs> [proxy] — the script as a cloud SessionStart hook runs it,
# with only the stubs and the base system on PATH. The session's proxy is
# <proxy>, or none: this file's own session may well have one.
run() {
  local repo=$1 dir=$2 proxy=${3:-}
  env -u HTTPS_PROXY -u https_proxy ${proxy:+HTTPS_PROXY=$proxy} \
    CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$repo" CLAUDE_ENV_FILE="$dir/env" \
    BIN_DIR="$dir/bin" DOCKERD_LOG="$dir/dockerd.log" DOCKERD_WAIT=1 DOCKER_RUN_DIR="$dir/run" \
    PROXY_CA="$dir/ca.crt" PROXY_PORT=3129 STATE_DIR="$dir/state" \
    PATH="$dir:/usr/bin:/bin" "$SUT"
}

installs() { grep -q 'dl.dagger.io' "$1/calls"; }

printf '\nwhen it runs\n'

repo="$WORK/r1"; dir="$WORK/s1"; workflow "$repo" 1.0.0-beta.15; stubs "$dir"
out=$(env -u CLAUDE_CODE_REMOTE CLAUDE_PROJECT_DIR="$repo" PATH="$dir:/usr/bin:/bin" "$SUT"; echo "exit=$?")
if [ "$out" = "exit=0" ] && [ ! -s "$dir/calls" ]; then
  ok 'outside a cloud session, it does nothing'
else
  bad 'outside a cloud session, it does nothing' "output: $out calls: $(cat "$dir/calls")"
fi

printf '\nthe pin\n'

# installer <stubs> — a curl whose install script does what the real one does:
# `sh` runs it with DAGGER_VERSION and BIN_DIR, and it writes a CLI reporting
# that version into BIN_DIR.
installer() {
  local dir=$1
  cat >"$dir/curl" <<EOF
#!/bin/sh
echo "curl \$*" >>"$dir/calls"
case "\$*" in *http_code*) printf 401; exit 0 ;; esac
while [ \$# -gt 0 ]; do [ "\$1" = -o ] && out=\$2; shift; done
cat >"\$out" <<'INSTALL'
echo "install \$DAGGER_VERSION \$BIN_DIR" >>"$dir/calls"
printf '#!/bin/sh\necho "dagger v%s (x) linux/amd64"\n' "\$DAGGER_VERSION" >"\$BIN_DIR/dagger"
chmod +x "\$BIN_DIR/dagger"
INSTALL
EOF
  chmod +x "$dir/curl"
}

repo="$WORK/r2"; dir="$WORK/s2"; workflow "$repo" 0.19.2; stubs "$dir"; installer "$dir"
out=$(run "$repo" "$dir")
if grep -qx "install 0.19.2 $dir/bin" "$dir/calls" && [[ $out == *"installed Dagger 0.19.2"* ]]; then
  ok 'a release pin is read and installed'
else
  bad 'a release pin is read and installed' "output: $out calls: $(cat "$dir/calls")"
fi

repo="$WORK/r3"; dir="$WORK/s3"; workflow "$repo" 1.0.0-beta.15; stubs "$dir"; installer "$dir"
out=$(run "$repo" "$dir")
if grep -qx "install 1.0.0-beta.15 $dir/bin" "$dir/calls" && [[ $out == *"installed Dagger 1.0.0-beta.15"* ]] \
  && grep -qxF "export PATH=\"$dir/bin:\$PATH\"" "$dir/env"; then
  ok 'a prerelease pin is installed into BIN_DIR and put on PATH through CLAUDE_ENV_FILE'
else
  bad 'a prerelease pin is installed into BIN_DIR and put on PATH through CLAUDE_ENV_FILE' "output: $out calls: $(cat "$dir/calls")"
fi

: >"$dir/calls"
out=$(run "$repo" "$dir")
if [ -z "$out" ] && ! installs "$dir" && [ "$(grep -c PATH "$dir/env")" = 1 ]; then
  ok 'a second run installs nothing, says nothing, and adds no second PATH line'
else
  bad 'a second run installs nothing, says nothing, and adds no second PATH line' "output: $out calls: $(cat "$dir/calls")"
fi

repo="$WORK/r4"; dir="$WORK/s4"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15
out=$(run "$repo" "$dir")
if [ -z "$out" ] && ! installs "$dir" && grep -q '^dagger version' "$dir/calls"; then
  ok 'when dagger version already reports the pin, nothing is installed'
else
  bad 'when dagger version already reports the pin, nothing is installed' "output: $out calls: $(cat "$dir/calls")"
fi

repo="$WORK/r5"; dir="$WORK/s5"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.14
run "$repo" "$dir" >/dev/null
if installs "$dir"; then
  ok 'a dagger at another version is replaced'
else
  bad 'a dagger at another version is replaced' "calls: $(cat "$dir/calls")"
fi

repo="$WORK/r6"; dir="$WORK/s6"; workflow "$repo" 1.0.0-beta.15; stubs "$dir"
sed -i '/default: "1.0.0-beta.15"/d' "$repo/.github/workflows/change-aware-ci.yml"
out=$(run "$repo" "$dir"; echo "exit=$?")
if ! installs "$dir" && [[ $out == *"cannot read the dagger-version default"*exit=0 ]] \
  && ! grep -q '1.25' "$dir/calls"; then
  ok 'a workflow with no dagger-version default installs nothing and says why'
else
  bad 'a workflow with no dagger-version default installs nothing and says why' "output: $out calls: $(cat "$dir/calls")"
fi

repo="$WORK/r11"; dir="$WORK/s11"; workflow "$repo" 1.0.0-beta.15; stubs "$dir"
out=$(run "$repo" "$dir"; echo "exit=$?")
if [[ $out == *"does not report v1.0.0-beta.15"*exit=0 ]]; then
  ok 'an install that leaves no CLI at the pin is reported, and the hook exits 0'
else
  bad 'an install that leaves no CLI at the pin is reported, and the hook exits 0' "output: $out"
fi

printf '\nwhen a host is blocked\n'

repo="$WORK/r7"; dir="$WORK/s7"; workflow "$repo" 1.0.0-beta.15; stubs "$dir"
# A proxy refusing CONNECT: curl exits 56, and -w prints 000.
printf '#!/bin/sh\necho "curl $*" >>"%s"\ncase "$*" in *http_code*) printf 000 ;; esac\nexit 56\n' "$dir/calls" >"$dir/curl"
out=$(run "$repo" "$dir"; echo "exit=$?")
if [[ $out == *"cannot download"*"dl.dagger.io is blocked"* ]] && [[ $out == *"cannot reach registry.dagger.io"* ]] \
  && [[ $out == *exit=0 ]] && [ "$(grep -c '^install-dagger:' <<<"$out")" = 2 ]; then
  ok 'each blocked host gets one line naming it, and the hook exits 0'
else
  bad 'each blocked host gets one line naming it, and the hook exits 0' "output: $out"
fi

printf '\nthe daemon\n'

repo="$WORK/r8"; dir="$WORK/s8"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15
# docker info fails until dockerd has run.
printf '#!/bin/sh\n[ -e "%s/up" ]\n' "$dir" >"$dir/docker"
printf '#!/bin/sh\necho "dockerd $*" >>"%s/calls"\ntouch "%s/up"\n' "$dir" "$dir" >"$dir/dockerd"
out=$(run "$repo" "$dir")
if grep -qx 'dockerd ' "$dir/calls" && [[ $out == *"started dockerd (log: $dir/dockerd.log)"* ]]; then
  ok 'no daemon answering: dockerd is started with no flags, and that is reported'
else
  bad 'no daemon answering: dockerd is started with no flags, and that is reported' "output: $out calls: $(cat "$dir/calls")"
fi

repo="$WORK/r9"; dir="$WORK/s9"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15
printf '#!/bin/sh\nexit 1\n' >"$dir/docker"
out=$(run "$repo" "$dir"; echo "exit=$?")
if [[ $out == *"dockerd did not answer within 1s; see $dir/dockerd.log"*exit=0 ]]; then
  ok 'a daemon that never answers is reported after the bounded wait, and the hook exits 0'
else
  bad 'a daemon that never answers is reported after the bounded wait, and the hook exits 0' "output: $out"
fi

# A resumed session: the previous container's containerd pid file survived, its
# process did not. A dockerd stub that exits, as the real one does, while it
# finds that file.
repo="$WORK/r12"; dir="$WORK/s12"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15
mkdir -p "$dir/run/containerd"; echo 236 >"$dir/run/containerd/containerd.pid"
printf '#!/bin/sh\n[ -e "%s/up" ]\n' "$dir" >"$dir/docker"
printf '#!/bin/sh\n[ -e "%s/run/containerd/containerd.pid" ] && exit 1\ntouch "%s/up"\n' "$dir" "$dir" >"$dir/dockerd"
# pgrep is the real one, so a dockerd running on this machine would hide the
# case; say so rather than fail it.
if pgrep -x dockerd >/dev/null 2>&1; then
  printf '  skip a stale containerd pid file is cleared (a dockerd is running here)\n'
else
  out=$(run "$repo" "$dir")
  if [ ! -e "$dir/run/containerd/containerd.pid" ] && [[ $out == *"started dockerd"* ]]; then
    ok 'a stale containerd pid file from a resumed session is cleared before dockerd starts'
  else
    bad 'a stale containerd pid file from a resumed session is cleared before dockerd starts' "output: $out"
  fi
fi

repo="$WORK/r10"; dir="$WORK/s10"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15
out=$(run "$repo" "$dir")
if ! grep -q '^dockerd' "$dir/calls" && [ -z "$out" ]; then
  ok 'a daemon already answering is left alone'
else
  bad 'a daemon already answering is left alone' "output: $out calls: $(cat "$dir/calls")"
fi

printf '\nthe engine\n'

# engine_stubs <stubs> — a docker that keeps one container's state in a file:
# `run` records it running with the -e values it was given, `inspect` prints
# them as real docker renders the hook's template (whether it runs on the first
# line, then one variable per line), `rm` drops it. The bridge gateway is
# 172.17.0.1. And a python3 that, run as the relay, records its pid in the
# pidfile it is handed and stays alive.
engine_stubs() {
  local dir=$1
  echo ca >"$dir/ca.crt"
  cat >"$dir/docker" <<EOF
#!/bin/sh
echo "docker \$*" >>"$dir/calls"
case "\$1" in
  info) exit 0 ;;
  network) echo 172.17.0.1 ;;
  inspect) [ -e "$dir/container" ] || exit 1; cat "$dir/container" ;;
  rm) rm -f "$dir/container" ;;
  run)
    echo true >"$dir/container"
    while [ \$# -gt 0 ]; do [ "\$1" = -e ] && echo "\$2" >>"$dir/container"; shift; done ;;
esac
EOF
  cat >"$dir/python3" <<EOF
#!/bin/sh
echo "python3 \$*" >>"$dir/calls"
echo \$\$ >"\$6"
exec sleep 60
EOF
  chmod +x "$dir/docker" "$dir/python3"
}

runner='export _EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://dagger-engine-v1.0.0-beta.15'

repo="$WORK/r13"; dir="$WORK/s13"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15; engine_stubs "$dir"
out=$(run "$repo" "$dir" http://127.0.0.1:44311)
if grep -q "^python3 .*proxy-forward.py 172.17.0.1 3129 127.0.0.1 44311 $dir/state/relay.pid" "$dir/calls" \
  && grep -q '^docker run -d --name dagger-engine-v1.0.0-beta.15 --privileged ' "$dir/calls" \
  && grep -q -- "-e HTTPS_PROXY=http://172.17.0.1:3129 .*-v $dir/ca.crt:/usr/local/share/ca-certificates/agent-proxy.crt:ro .*registry.dagger.io/engine:v1.0.0-beta.15" "$dir/calls" \
  && ! grep -q -- '--network' "$dir/calls" \
  && grep -qxF "$runner" "$dir/env" && [[ $out == *"started the Dagger engine"* ]]; then
  ok 'with a proxy: the relay and the engine start on the bridge, and the CLI is pointed at that engine'
else
  bad 'with a proxy: the relay and the engine start on the bridge, and the CLI is pointed at that engine' "output: $out calls: $(cat "$dir/calls")"
fi

: >"$dir/calls"
out=$(run "$repo" "$dir" http://127.0.0.1:44311)
if [ -z "$out" ] && ! grep -q '^python3\|^docker run\|^docker rm' "$dir/calls" && [ "$(grep -c RUNNER "$dir/env")" = 1 ]; then
  ok 'a second run finds both in place, says nothing, and adds no second runner line'
else
  bad 'a second run finds both in place, says nothing, and adds no second runner line' "output: $out calls: $(cat "$dir/calls")"
fi

: >"$dir/calls"
old=$(cat "$dir/state/relay.pid")
out=$(run "$repo" "$dir" http://127.0.0.1:50000)
if grep -q 'proxy-forward.py 172.17.0.1 3129 127.0.0.1 50000' "$dir/calls" && ! kill -0 "$old" 2>/dev/null \
  && ! grep -q '^docker run' "$dir/calls" && [ -z "$out" ]; then
  ok 'a resumed session with a new proxy port gets a new relay, and the engine is left alone'
else
  bad 'a resumed session with a new proxy port gets a new relay, and the engine is left alone' "output: $out calls: $(cat "$dir/calls")"
fi

: >"$dir/calls"
sed -i 's/^true$/false/' "$dir/container"
out=$(run "$repo" "$dir" http://127.0.0.1:50000)
if grep -q '^docker rm -f dagger-engine-v1.0.0-beta.15' "$dir/calls" && grep -q '^docker run' "$dir/calls" \
  && [[ $out == *"started the Dagger engine"* ]]; then
  ok 'a stopped engine, as a resumed session leaves it, is started again'
else
  bad 'a stopped engine, as a resumed session leaves it, is started again' "output: $out calls: $(cat "$dir/calls")"
fi

repo="$WORK/r14"; dir="$WORK/s14"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15; engine_stubs "$dir"
out=$(run "$repo" "$dir")
if [ -z "$out" ] && ! grep -q '^python3\|^docker run' "$dir/calls" && ! grep -q RUNNER "$dir/env" 2>/dev/null; then
  ok 'with no proxy, the engine is left to the CLI'
else
  bad 'with no proxy, the engine is left to the CLI' "output: $out calls: $(cat "$dir/calls")"
fi

repo="$WORK/r15"; dir="$WORK/s15"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15; engine_stubs "$dir"
rm "$dir/ca.crt"
out=$(run "$repo" "$dir" http://127.0.0.1:44311; echo "exit=$?")
if [[ $out == *"no proxy CA at $dir/ca.crt"*exit=0 ]] && ! grep -q '^python3\|^docker run' "$dir/calls"; then
  ok 'a proxy with no CA bundle starts nothing, says why, and the hook exits 0'
else
  bad 'a proxy with no CA bundle starts nothing, says why, and the hook exits 0' "output: $out calls: $(cat "$dir/calls")"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
