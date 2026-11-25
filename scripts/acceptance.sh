#!/usr/bin/env bash
# Deterministic local acceptance run for FieldCut.
# It checks the stable binary exit codes directly because `go run` collapses
# any non-zero child status to exit code 1 while still printing the child code.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go mod tidy
go build -o "${TMPDIR:-/tmp}/fieldcut-acceptance" ./cmd/fieldcut
go test ./...

BIN="${TMPDIR:-/tmp}/fieldcut-acceptance"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

expect_code() {
  local want="$1"
  shift
  "$@"
  local got=$?
  if [[ "$got" -ne "$want" ]]; then
    echo "expected exit $want, got $got: $*" >&2
    exit 1
  fi
}

"$BIN" demo --case clean --seed 17 --out "$OUT/clean"
"$BIN" verify --out "$OUT/clean"

expect_code 2 "$BIN" demo --case damaged --seed 17 --out "$OUT/damaged"

expect_code 75 "$BIN" demo --case interrupted --seed 17 --out "$OUT/recover"
"$BIN" resume --out "$OUT/recover"
"$BIN" verify --out "$OUT/recover"

# Go 1.25.6's tidy treats the identical toolchain directive as redundant and
# removes it; restore the explicit pin required by the project specification.
go mod edit -toolchain=go1.25.6

echo "acceptance artifacts: $OUT"
