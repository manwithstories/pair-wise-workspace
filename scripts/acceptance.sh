#!/usr/bin/env bash
# Reproduce the mediad acceptance demo and independently check the manifest.
set -euo pipefail

cd "$(dirname "$0")/.."

go version
go mod tidy
go build ./...
go test ./...

DIR="${1:-${TMPDIR:-/tmp}/mediad-demo}"
go run ./cmd/mediad demo \
  --seed 73021 \
  --dir "$DIR" \
  --faults all \
  --interrupt-after 128 \
  --resume

echo "independent manifest hash:"
shasum -a 256 "$DIR/master.m3u8"
