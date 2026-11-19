#!/bin/sh
# make_fixtures.sh -- generate the boundary fixture set for mediaqc.
#
# All samples are produced locally by the mediaqc binary (which drives ffmpeg's
# lavfi sources and then patches bytes).  No external media is downloaded or
# referenced.
#
# Usage:
#   scripts/make_fixtures.sh [WORK_DIR] [MEDIAQC_BINARY]
#
# Defaults: WORK_DIR=./build/fixtures, MEDIAQC_BINARY=./build/mediaqc
#
# After this script runs:
#   WORK_DIR/input/       12 boundary inputs (clean, truncated, corrupt, ...)
#   WORK_DIR/output/      created empty for the batch command
#   WORK_DIR/checkpoints/ seeded with one deliberately stale checkpoint
set -eu

WORK_DIR=${1:-./build/fixtures}
BIN=${2:-./build/mediaqc}

if [ ! -x "$BIN" ]; then
  echo "make_fixtures: $BIN is not executable; build first:" >&2
  echo "  cmake -S . -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build --parallel" >&2
  exit 2
fi

if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "make_fixtures: ffmpeg not found in PATH; install it with 'brew install ffmpeg'" >&2
  exit 2
fi

echo "make_fixtures: generating into $WORK_DIR"
"$BIN" selftest --generate-fixtures --generate-only --work "$WORK_DIR"

echo
echo "make_fixtures: inputs"
ls -1 "$WORK_DIR/input"
echo
echo "make_fixtures: next steps"
echo "  $BIN batch  --input $WORK_DIR/input --output $WORK_DIR/output --resume --json"
echo "  $BIN verify --input $WORK_DIR/output --json"
