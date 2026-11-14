#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

go build -o bin/segmanifest ./cmd/segmanifest
./bin/segmanifest gen --out testdata/setA --seed 7 --representations 3 \
  --segments 120 --offset-ms -37 --jitter-ms 1.6
./bin/segmanifest plan --in testdata/setA --out out/setA.smf \
  --target 4s --tolerance 2ms
./bin/segmanifest emit --manifest out/setA.smf --rep r1080 --out out/paths.txt
./bin/segmanifest verify --manifest out/setA.smf --json
./bin/segmanifest verify --manifest out/setA.smf --strict
./bin/segmanifest stats --manifest out/setA.smf
