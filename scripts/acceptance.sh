#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j

./build/h264scan synth --out samples/case_a.h264 \
    --gops 4 --slices 3 --interlaced pair --seed 7
./build/h264scan analyze samples/case_a.h264 --json out/case_a.json --verbose
./build/h264scan analyze samples --recursive --csv out/summary.csv
./build/h264scan analyze samples/case_a.h264 --strict

grep -q '"profile_idc": 100' out/case_a.json
grep -q '"level_idc": 41' out/case_a.json
grep -q '"width": 1280' out/case_a.json
test "$(grep -c '"start_offset":' out/case_a.json)" -eq 4
grep -q '"frames": 96' out/case_a.json
grep -q '"gop_count": 4' out/case_a.json
grep -q '"slice_count": 288' out/case_a.json
grep -q '"warnings": 0' out/case_a.json
grep -q '^file,nal_count,gop_count,frames,slice_count' out/summary.csv

printf 'acceptance: PASS\n'
