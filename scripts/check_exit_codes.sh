#!/usr/bin/env bash
# Verify the stable CLI exit-code contract: 2 arguments, 3 bad input, 4 storage.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

check_code() {
  local want="$1"
  shift
  set +e
  "$@" >/tmp/mediad-exit-check.log 2>&1
  local got=$?
  set -e
  if [[ "$got" -ne "$want" ]]; then
    cat /tmp/mediad-exit-check.log >&2
    echo "exit code mismatch: want=$want got=$got cmd=$*" >&2
    exit 1
  fi
  echo "ok exit_code=$want: $*"
}

check_code 2 go run ./cmd/mediad demo --codec not-a-codec

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

check_code 3 go run ./cmd/mediad demo \
  --out "$TMP/no-key" --codec h264 --seed 41 \
  --inject no-key

mkdir -p "$TMP/store"
go run ./cmd/mediad demo --out "$TMP/store" --codec h264 --seed 41 >/dev/null
printf '{broken journal\n' > "$TMP/store/journal-h264.json"
check_code 4 go run ./cmd/mediad demo \
  --out "$TMP/store" --codec h264 --seed 41 --resume

echo "exit-code contract verified"
