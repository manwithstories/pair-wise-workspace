#!/usr/bin/env bash
# Reproduce the documented local acceptance run and check repeatability.
set -euo pipefail

cd "$(dirname "$0")/.."

go mod download
go build ./...
go test ./...

case_dir="${1:-${TMPDIR:-/tmp}/mediad-case}"
go run ./cmd/mediad demo --seed 29 --seconds 12 --out "$case_dir"
shasum -a 256 "$case_dir/final.mp4"

first="$(shasum -a 256 "$case_dir/final.mp4" | awk '{print $1}')"
go run ./cmd/mediad demo --seed 29 --seconds 12 --out "$case_dir" >/dev/null
second="$(shasum -a 256 "$case_dir/final.mp4" | awk '{print $1}')"

if [[ "$first" != "$second" ]]; then
  echo "checksum changed across runs: $first != $second" >&2
  exit 1
fi
echo "repeat_sha256=$second"
