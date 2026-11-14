#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PACKETS="${PACKETS:-3000}"
DURATION="${DURATION:-6s}"
SEED="${SEED:-42}"
IMPAIR="${IMPAIR:-loss=0.01,dup=0.01,reorder=0.02}"
REPORT="${REPORT:-out/report.json}"
MIN_FORWARD_RATIO="${MIN_FORWARD_RATIO:-0.98}"
MAX_JITTER_P95_MS="${MAX_JITTER_P95_MS:-30}"

mkdir -p "$(dirname "$REPORT")"

go run ./cmd/relayctl demo \
  --packets "$PACKETS" \
  --duration "$DURATION" \
  --seed "$SEED" \
  --impair "$IMPAIR" \
  --sink 127.0.0.1:0 \
  --json "$REPORT"

go run ./cmd/relayctl verify \
  --report "$REPORT" \
  --min-forward-ratio "$MIN_FORWARD_RATIO" \
  --max-jitter-p95-ms "$MAX_JITTER_P95_MS"
