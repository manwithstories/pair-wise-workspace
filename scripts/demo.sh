#!/bin/sh
# Runs the documented acceptance flow end to end from a clean checkout.
# Usage: scripts/demo.sh [workdir]
set -eu

WORK="${1:-/tmp/av-demo}"
BIN="${TMPDIR:-/tmp}/avrepair"
CASES="$WORK/cases"

cd "$(dirname "$0")/.."

go mod tidy
go build ./...
go test ./...
go build -o "$BIN" ./cmd/avrepair

rm -rf "$WORK"
mkdir -p "$WORK"

echo "== fixture"
"$BIN" fixture --dir "$CASES"

echo "== success path (lossy)"
set +e
"$BIN" repair --input "$CASES/lossy" --state "$WORK/state" --output "$WORK/out"
echo "exit=$?"
set -e

echo "== corrupt path"
set +e
"$BIN" repair --input "$CASES/corrupt" --state "$WORK/bad-state" --output "$WORK/bad-out"
echo "exit=$?"
set -e

echo "== resume path"
set +e
"$BIN" repair --input "$CASES/normal" --state "$WORK/resume-state" --output "$WORK/resume-out" --interrupt-after index
echo "exit=$?"
set -e
"$BIN" repair --input "$CASES/normal" --state "$WORK/resume-state" --output "$WORK/resume-out" --resume
echo "exit=$?"

echo "== verify index"
"$BIN" verify --index "$WORK/out/index.json" --output "$WORK/out"
