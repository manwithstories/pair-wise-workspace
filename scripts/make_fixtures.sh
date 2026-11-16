#!/usr/bin/env bash
# Build lutgrade, generate fixtures, and run the acceptance demo.
#
#   scripts/make_fixtures.sh [build-dir] [fixture-dir]
#
# Defaults: build/ and fixtures/. Everything is written inside the repository
# tree; nothing outside it is touched.
set -euo pipefail

BUILD_DIR="${1:-build}"
FIXTURE_DIR="${2:-fixtures}"

cd "$(dirname "$0")/.."

cmake -S . -B "$BUILD_DIR" -DCMAKE_BUILD_TYPE=Release >/dev/null
cmake --build "$BUILD_DIR" -j2 >/dev/null

BIN="$BUILD_DIR/lutgrade"

echo "== generate-fixture =="
"$BIN" generate-fixture --dir "$FIXTURE_DIR" --size 33

echo
echo "== run (tiled, 4 threads) =="
"$BIN" run \
  --input "$FIXTURE_DIR/input.ppm" \
  --lut "$FIXTURE_DIR/demo.cube" \
  --in-space srgb --out-space rec709 \
  --path tiled --threads 4 \
  --output "$FIXTURE_DIR/out.ppm"

echo
echo "== compare (scalar vs tiled) =="
"$BIN" compare \
  --input "$FIXTURE_DIR/input.ppm" \
  --lut "$FIXTURE_DIR/demo.cube" \
  --in-space srgb --out-space rec709 \
  --threads 4

echo
echo "== integration tests =="
"$BUILD_DIR/lutgrade_tests"
