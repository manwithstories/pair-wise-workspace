#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

go version
go mod tidy
go build ./...
go test ./...

go run ./cmd/mediad demo \
  --work ./tmp/proof \
  --seed 17 \
  --profile variable \
  --interrupt-after 2

# Independent corrupt-input command must use exit code 3.
set +e
go run ./cmd/mediad scan --input ./tmp/proof/truncated-offset.mp4 >/dev/null 2>&1
scan_code=$?
set -e
if [[ "$scan_code" -ne 3 ]]; then
  echo "corrupt scan exit code=$scan_code, want 3" >&2
  exit 1
fi

# Parameter errors must use exit code 2.
set +e
go run ./cmd/mediad demo >/dev/null 2>&1
arg_code=$?
set -e
if [[ "$arg_code" -ne 2 ]]; then
  echo "argument error exit code=$arg_code, want 2" >&2
  exit 1
fi
