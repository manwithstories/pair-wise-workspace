#!/usr/bin/env bash
# Run the same local acceptance sequence documented for Broadcast MediaPipe.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export PATH="$(go env GOPATH)/bin:$PATH"
GO_BIN="${GO_BIN:-go1.25.6}"

"$GO_BIN" mod tidy
"$GO_BIN" build ./...
"$GO_BIN" test ./...
"$GO_BIN" run ./cmd/mediapipe demo \
  --work .tmp/demo \
  --seed 17 \
  --seconds 30 \
  --target-kbps 1200 \
  --fault-suite
