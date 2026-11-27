#!/bin/sh
# Acceptance: build, test, and run the selfcheck demo.
# Usage: scripts/selfcheck.sh [seed] [bytes] [outdir]
set -eu

seed="${1:-41}"
bytes="${2:-2097152}"
out="${3:-/tmp/tscheck-run}"

cd "$(dirname "$0")/.."
go mod tidy
go build ./...
go test ./...
go run ./cmd/tscheck selfcheck --seed "$seed" --bytes "$bytes" --out "$out"
