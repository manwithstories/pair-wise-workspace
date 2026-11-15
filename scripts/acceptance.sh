#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
cmake -S . -B build
cmake --build build -j
python3 scripts/gen_fixtures.py
./build/smx genfont --out fixtures/synth.ttf --seed 7
mkdir -p out
set +e
./build/smx measure --font fixtures/synth.ttf --size 32 --max-width 640 \
  --max-lines 2 --input fixtures/cues.txt --out out/report.json
measure_code=$?
./build/smx batch --font fixtures/synth.ttf --size 32 --max-width 640 \
  --dir fixtures/cues --out-dir out --jobs 4
batch_code=$?
set -e
./build/smx selftest
python3 scripts/check_report.py out/report.json
[ "$measure_code" -eq 2 ]
[ "$batch_code" -eq 2 ]
echo "acceptance: OK (measure=2, batch=2, selftest=0)"
