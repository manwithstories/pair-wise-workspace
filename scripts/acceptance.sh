#!/usr/bin/env bash
# Local acceptance entrypoint: build, test, demo, rerun hashes, exit codes.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go mod tidy
go build ./...
go test ./...

for c in clean damaged recover; do
  go run ./cmd/medcheck demo --case "$c" --seed 17
done

work="$(mktemp -d "${TMPDIR:-/tmp}/medcheck-acceptance.XXXXXX")"
trap 'rm -rf "$work"' EXIT

go run ./cmd/medcheck demo --case recover --seed 17 --workdir "$work" > "$work/run1.txt"
go run ./cmd/medcheck demo --case recover --seed 17 --workdir "$work" > "$work/run2.txt"
grep '^segment_hash ' "$work/run1.txt" > "$work/hashes1.txt"
grep '^segment_hash ' "$work/run2.txt" > "$work/hashes2.txt"
diff -u "$work/hashes1.txt" "$work/hashes2.txt"
grep -E '^segment_sha256=[0-9a-f]{64}$' "$work/run1.txt" >/dev/null

expect_code() {
  local want="$1"
  shift
  set +e
  "$@" >/dev/null 2>&1
  local got="$?"
  set -e
  if [[ "$got" != "$want" ]]; then
    echo "expected exit $want, got $got: $*" >&2
    exit 1
  fi
}

expect_code 2 go run ./cmd/medcheck demo --case invalid
expect_code 3 go run ./cmd/medcheck demo --case damaged --seed 17 --strict --workdir "$work/strict"
expect_code 4 go run ./cmd/medcheck demo --case clean --workdir /dev/null/medcheck-io-fail

echo "acceptance checks passed"
