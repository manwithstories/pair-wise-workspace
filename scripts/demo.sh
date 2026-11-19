#!/bin/sh
# Run the acceptance demo and print the terminal summary.
# Usage: scripts/demo.sh [extra demo flags]
set -eu
cd "$(dirname "$0")/.."
exec go run ./cmd/mediasync demo \
  --seed 42 \
  --packet-loss-burst 4 \
  --bandwidth-kbps 800,2400,600 \
  --out "${TMPDIR:-/tmp}/media-report.json" \
  "$@"
