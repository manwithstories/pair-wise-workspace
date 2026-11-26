#!/usr/bin/env bash
# Reproduce the mediad build, test, selftest, and verification acceptance path.
set -euo pipefail

cd "$(dirname "$0")/.."

brew install go@1.25
export PATH="$(brew --prefix go@1.25)/bin:$PATH"

# The benchmark pins the exact patch release. Some Homebrew indexes provide a
# newer go@1.25 patch; use the local go1.25.6 shim/SDK when this workspace has it.
if command -v go1.25.6 >/dev/null 2>&1; then
  export PATH="$(dirname "$(command -v go1.25.6")"):$PATH"
fi

go version
go mod download
go mod tidy
go build ./...
go test ./...

OUT="${TMPDIR:-/tmp}/mediad-run"
go run ./cmd/mediad selftest \
  --seed 42 \
  --duration 60s \
  --loss 0.03 \
  --burst 7 \
  --out "$OUT"

go run ./cmd/mediad verify --input "$OUT/repaired.mp4"
