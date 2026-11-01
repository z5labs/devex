#!/usr/bin/env bash
# count.sh — count the files in a directory. Does not call gh pr list.
set -euo pipefail
find "${1:-.}" -type f | wc -l
