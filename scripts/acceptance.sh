#!/bin/sh
# Run the two acceptance scenarios from the project directory.
#
# Usage: scripts/acceptance.sh [seed]
# Exits non-zero if either scenario misses its acceptance targets.
set -eu

cd "$(dirname "$0")/.."
SEED="${1:-42}"

echo "== scenario 1: mixed damage, interrupted resume =="
go run ./cmd/archive demo \
	--seed "$SEED" --duration 120s --late-window 2s --bitrates 64k,128k \
	--interrupt-after 40s --resume

echo
echo "== scenario 2: burst loss with boundary-late traffic =="
go run ./cmd/archive demo \
	--seed "$SEED" --damage burst-loss --late-at-boundary \
	--interrupt-after 20s --resume
