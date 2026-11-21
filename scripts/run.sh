#!/usr/bin/env bash
# Reproducible local pipeline for the mediad media-processing DAG.
# Purely local: no network access, no external media services.
set -euo pipefail

cd "$(dirname "$0")/.."

BIN="$(pwd)/tmp/mediad-bin"
DEMO_DIR="$(pwd)/tmp/demo"
CORRUPT_DIR="$(pwd)/tmp/corrupt"

echo "== go mod tidy =="
go mod tidy

echo "== go build ./... =="
go build ./...
go build -o "$BIN" ./cmd/mediad

echo "== go test ./... =="
go test ./...

echo "== healthy demo =="
"$BIN" demo --out "$DEMO_DIR"

echo "== corrupt demo (exit 2 expected) =="
set +e
"$BIN" demo --out "$CORRUPT_DIR" --corrupt
code=$?
set -e
if [ "$code" -ne 2 ]; then
  echo "expected corrupt demo exit code 2, got $code" >&2
  exit 1
fi

echo "== interrupt then resume =="
set +e
"$BIN" demo --out "$DEMO_DIR" --interrupt-after 3
ic=$?
set -e
if [ "$ic" -ne 2 ]; then
  echo "expected interrupted demo exit code 2, got $ic" >&2
  exit 1
fi
"$BIN" resume --out "$DEMO_DIR"

echo "ALL STEPS OK"
