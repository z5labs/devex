#!/usr/bin/env bash
#
# install-dagger_test.sh — the cases for install-dagger.sh.
#
# Offline. Each case builds a fixture repository holding a change-aware-ci.yml,
# and a stub directory that shadows `curl`, `docker`, `dockerd` and, where the
# case needs one, `dagger`. Every stub records its arguments to a calls file, so
# a case asserts on what the script asked for as well as what it said. Nothing
# here touches the network or a real daemon.
#
# Run: .claude/hooks/install-dagger_test.sh
# Exit 0 when every case passes, 1 otherwise.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUT="$HERE/install-dagger.sh"
[ -x "$SUT" ] || { printf 'install-dagger.sh is not executable at %s\n' "$SUT" >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

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

# run <repo> <stubs> — the script as a cloud SessionStart hook runs it, with only
# the stubs and the base system on PATH.
run() {
  local repo=$1 dir=$2
  CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$repo" CLAUDE_ENV_FILE="$dir/env" \
    BIN_DIR="$dir/bin" DOCKERD_LOG="$dir/dockerd.log" DOCKERD_WAIT=1 \
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

repo="$WORK/r10"; dir="$WORK/s10"; workflow "$repo" 1.0.0-beta.15; stubs "$dir" 1.0.0-beta.15
out=$(run "$repo" "$dir")
if ! grep -q '^dockerd' "$dir/calls" && [ -z "$out" ]; then
  ok 'a daemon already answering is left alone'
else
  bad 'a daemon already answering is left alone' "output: $out calls: $(cat "$dir/calls")"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
