#!/usr/bin/env bash
# Install pinned dependencies, build/test every module, and run the deterministic demo.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go mod download
go mod tidy
go build ./...
go test ./...

OUT="${OUT:-$(mktemp -d)}"
echo "OUT=$OUT"
go run ./cmd/mediad demo \
  --out "$OUT" \
  --codec all \
  --seed 41 \
  --target-bps 900000 \
  --drift-ppm 120 \
  --inject wrap,duplicate,truncate \
  --resume
