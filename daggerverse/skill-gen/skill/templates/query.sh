#!/usr/bin/env bash
set -euo pipefail

# Resolution order for the .env file, first match wins:
#   1. --env-file PATH CLI flag
#   2. PG_ENV_FILE environment variable
#   3. ./.env in the current working directory
# If --env-file or PG_ENV_FILE is set, the path must exist or the script
# exits with an error. If neither is set and ./.env doesn't exist, the
# baked-in defaults below are used.
# See .env.example for the keys this skill reads.

ENV_FILE=""
SQL_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --env-file)
      if [ $# -lt 2 ] || [ -z "${2:-}" ] || [[ "${2:-}" == -* ]]; then
        echo "error: --env-file requires a non-empty PATH argument" >&2
        exit 1
      fi
      ENV_FILE="$2"
      shift 2
      ;;
    --env-file=*)
      ENV_FILE="${1#--env-file=}"
      if [ -z "$ENV_FILE" ]; then
        echo "error: --env-file requires a non-empty PATH argument" >&2
        exit 1
      fi
      shift
      ;;
    --)
      shift
      while [ $# -gt 0 ]; do SQL_ARGS+=("$1"); shift; done
      ;;
    *)
      SQL_ARGS+=("$1")
      shift
      ;;
  esac
done

if [ -z "$ENV_FILE" ]; then
  ENV_FILE="${PG_ENV_FILE:-}"
fi
if [ -z "$ENV_FILE" ] && [ -f ./.env ]; then
  ENV_FILE="./.env"
fi

# Parse KEY=VALUE lines from $ENV_FILE without sourcing it. Sourcing would
# execute arbitrary shell, which is unsafe given that ./.env is loaded
# automatically when present.
load_env_file() {
  local file="$1" line key value
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    if [[ "$line" =~ ^[[:space:]]*(#.*)?$ ]]; then continue; fi
    if [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
      key="${BASH_REMATCH[2]}"
      value="${BASH_REMATCH[3]}"
      if [[ "$value" =~ ^\"(.*)\"$ ]] || [[ "$value" =~ ^\'(.*)\'$ ]]; then
        value="${BASH_REMATCH[1]}"
      fi
      export "$key=$value"
    else
      echo "invalid env assignment in $file: $line" >&2
      exit 1
    fi
  done < "$file"
}

if [ -n "$ENV_FILE" ]; then
  if [ ! -f "$ENV_FILE" ]; then
    echo "env file not found: $ENV_FILE" >&2
    exit 1
  fi
  load_env_file "$ENV_FILE"
fi

# Fall back to generation-time values for any key the .env didn't supply.
# Host/user/db are baked in as single-quoted literals (escaped at generation
# time), and assigned outside a ${:=} default word, so an unusual introspected
# value can't trigger command/parameter substitution when this script runs.
[ -n "${PGHOST:-}" ] || PGHOST=<host>
[ -n "${PGPORT:-}" ] || PGPORT=<port>
[ -n "${PGUSER:-}" ] || PGUSER=<user>
: "${PGPASSWORD:=}"
# PGDATABASE is hardcoded — this skill is schema-specific by design.
PGDATABASE=<dbname>
export PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE

# Forward every libpq env var (PGHOST/PGPORT/PGUSER/PGDATABASE/PGPASSWORD plus
# PGSSLMODE, PGSSLROOTCERT, PGSERVICE, PGOPTIONS, PGTARGETSESSIONATTRS,
# PGAPPNAME, PGCONNECT_TIMEOUT, …) into the container. Filter `^PG[A-Z]` so
# libpq-style names (PGFOO) pass while our internal config (PG_DOCKER_ARGS,
# PG_CONTAINER_RUNTIME, PG_ENV_FILE) does not — those start with PG_ and would
# confuse psql if forwarded.
LIBPQ_ENV_ARGS=()
while IFS= read -r var; do
  [ -z "$var" ] && continue
  LIBPQ_ENV_ARGS+=(-e "$var")
done < <(compgen -e | grep -E '^PG[A-Z]' || true)

if [ -n "${PG_CONTAINER_RUNTIME:-}" ]; then
  RUNTIME="$PG_CONTAINER_RUNTIME"
elif command -v docker >/dev/null 2>&1; then
  RUNTIME=docker
elif command -v podman >/dev/null 2>&1; then
  RUNTIME=podman
else
  echo "neither docker nor podman found on PATH" >&2
  exit 1
fi
PSQL_IMAGE="${PSQL_IMAGE:-docker.io/alpine/psql:17.7}"

read -r -a EXTRA_ARGS <<< "${PG_DOCKER_ARGS:-}"

if [ ${#SQL_ARGS[@]} -eq 0 ]; then
  exec "$RUNTIME" run --rm -i \
    "${LIBPQ_ENV_ARGS[@]}" \
    "${EXTRA_ARGS[@]}" "$PSQL_IMAGE"
else
  # Join all positional args with spaces so unquoted SQL like
  # `query.sh SELECT 1` is preserved instead of silently truncated to "SELECT".
  exec "$RUNTIME" run --rm -i \
    "${LIBPQ_ENV_ARGS[@]}" \
    "${EXTRA_ARGS[@]}" "$PSQL_IMAGE" -c "${SQL_ARGS[*]}"
fi
