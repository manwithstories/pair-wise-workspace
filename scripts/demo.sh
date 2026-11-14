#!/bin/sh
# End-to-end demo: synthesize -> slice -> manifest -> verify.
set -e
cd "$(dirname "$0")/.."

go build -o bin/segctl ./cmd/segctl

./bin/segctl fixture --out testdata --minutes 6 --seed 7 --inject loss=3,dup=2,reorder=5,rebase=1
./bin/segctl slice --out testdata/slices --target 2s
./bin/segctl manifest --root testdata/slices --master testdata/index.m3u8 --allow-gap

set +e
./bin/segctl verify --master testdata/index.m3u8 --root testdata/slices --json out/verify.json
code=$?
set -e
echo "verify exit code: $code"
exit 0
