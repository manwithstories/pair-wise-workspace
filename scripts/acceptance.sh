#!/usr/bin/env bash
# Local acceptance driver. All sample media is generated in TMPDIR; this never
# contacts a network service after Go module dependencies are present.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO_BIN="${GO_BIN:-go1.25.6}"
if ! command -v "$GO_BIN" >/dev/null 2>&1; then
  GO_BIN="$(go env GOPATH)/bin/go1.25.6"
fi

"$GO_BIN" mod tidy
"$GO_BIN" mod download
"$GO_BIN" build ./...
"$GO_BIN" test ./...

proof="${TMPDIR:-/tmp}/pairflow-proof-$$"
"$GO_BIN" run ./cmd/mediasync prove \
  --work "$proof" \
  --seed 417 \
  --tracks 4 \
  --frames-per-track 1200 \
  --payload-bytes 4096 \
  --drift-ppm 180 \
  --rollover 4294967000 \
  --vfr-seed 53 \
  --drop-seqs 811,812 \
  --damage-track cam-03 \
  --damage-frame 311 \
  --corrupt-byte 29 \
  --crash-after-records 720

cmp "$proof/cold/output.jsonl" "$proof/resumed/output.jsonl"
printf 'cold_resume_cmp=identical\n'
shasum -a 256 "$proof/cold/output.jsonl" "$proof/resumed/output.jsonl"
