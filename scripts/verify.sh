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

mkdir -p ./tmp/bin
go build -o ./tmp/bin/mediad ./cmd/mediad

expect_code() {
  local want="$1"
  shift
  set +e
  "$@" >/tmp/mediad-command.out 2>/tmp/mediad-command.err
  local got=$?
  set -e
  if [[ "$got" -ne "$want" ]]; then
    cat /tmp/mediad-command.err >&2
    echo "command [$*] exit code=$got, want $want" >&2
    exit 1
  fi
}

# Independent corrupt-input discovery uses exit code 3.
expect_code 3 ./tmp/bin/mediad scan --input ./tmp/proof/truncated-offset.mp4

# Parameter errors use exit code 2.
expect_code 2 ./tmp/bin/mediad demo

# Corrupt checkpoint/integrity data uses exit code 4.
printf '%s\n' '{truncated-json' > ./tmp/proof/bad.checkpoint.json
expect_code 4 ./tmp/bin/mediad verify --checkpoint ./tmp/proof/bad.checkpoint.json

echo "all acceptance checks passed"
