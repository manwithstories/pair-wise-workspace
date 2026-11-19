#!/usr/bin/env bash
#
# demo.sh - generate synthetic samples in-place and print replay metrics.
# Pure local execution, no network and no external data files. Requires the
# project to already be configured and built (see README / CMakeLists.txt).
#
# Usage:
#   scripts/demo.sh [path-to-media_replay]
#
set -euo pipefail

BIN="${1:-./build/media_replay}"
if [[ ! -x "$BIN" ]]; then
  echo "error: media_replay binary not found at $BIN" >&2
  echo "build it first: cmake -S . -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build -j" >&2
  exit 64
fi

WORK_DIR="./tmp/demo"
rm -rf "$WORK_DIR" 2>/dev/null || true
mkdir -p "$WORK_DIR"

echo "=================================================================="
echo " media_replay deterministic demo"
echo "=================================================================="

for CODEC in h264 h265 av1; do
  echo
  echo "--- codec=$CODEC : weak-network trace (dup/reorder/burst/bandwidth) ---"
  "$BIN" demo \
    --codec "$CODEC" \
    --seed 41 \
    --loss-burst 7 \
    --bandwidth 1800,700,2400 \
    --checkpoint-dir "$WORK_DIR/index_$CODEC"
done

echo
echo "--- damaged fixtures: locatable error categories and block/frame ids ---"
"$BIN" fixtures --codec h264 --seed 41

echo
echo "--- interrupt then resume (tmp checkpoints must be ignored) ---"
RESUME_DIR="$WORK_DIR/resume"
"$BIN" demo --codec h264 --seed 41 --loss-burst 7 \
  --bandwidth 1800,700,2400 --checkpoint-dir "$RESUME_DIR" \
  --inject-interrupt 3
echo ">>> resuming with identical parameters"
"$BIN" resume --codec h264 --seed 41 --loss-burst 7 \
  --bandwidth 1800,700,2400 --checkpoint-dir "$RESUME_DIR"

echo
echo "--- exit code contract ---"
set +e
"$BIN" demo --codec bogus >/dev/null 2>&1; echo "bad codec exit=$? (expect 64)"
"$BIN" demo --truncated --checkpoint-dir "$WORK_DIR/trunc" >/dev/null 2>&1
echo "truncated input exit=$? (expect 2)"
echo "demo finished; artifacts under $WORK_DIR"
