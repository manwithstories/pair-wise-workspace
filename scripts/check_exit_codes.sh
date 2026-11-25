#!/usr/bin/env bash
# Verify the stable binary exit-code contract: 2 arguments, 3 bad input, 4 storage.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN="$TMP/mediad"
go build -o "$BIN" ./cmd/mediad

check_code() {
  local want="$1"
  shift
  set +e
  "$@" >"$TMP/exit-check.log" 2>&1
  local got=$?
  set -e
  if [[ "$got" -ne "$want" ]]; then
    cat "$TMP/exit-check.log" >&2
    echo "exit code mismatch: want=$want got=$got cmd=$*" >&2
    exit 1
  fi
  echo "ok exit_code=$want: $*"
}

check_code 2 "$BIN" demo --codec not-a-codec

check_code 3 "$BIN" demo \
  --out "$TMP/no-key" --codec h264 --seed 41 \
  --inject no-key

mkdir -p "$TMP/store"
"$BIN" demo --out "$TMP/store" --codec h264 --seed 41 >/dev/null
printf '{broken journal\n' > "$TMP/store/normal/h264/journal-h264.json"
check_code 4 "$BIN" demo \
  --out "$TMP/store" --codec h264 --seed 41 --resume

echo "exit-code contract verified"
