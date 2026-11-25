#!/usr/bin/env bash
# Full local acceptance flow: dependencies, build, race tests, verification.
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="/usr/local/go/bin:$PATH"
export GOTOOLCHAIN=local

go mod tidy
go build ./...
go test -race ./...

go run ./cmd/mediapack verify \
  --seed 73 \
  --faults pcr-wrap,cc-gap,psi-update,bad-crc,truncated \
  --buffer-packets 4096 \
  --consumer-delay 20ms \
  --crash-after 2 \
  --resume
