#!/usr/bin/env bash
# Reproduce the local acceptance sequence from the task specification.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go mod tidy
go build ./cmd/qc
go test ./...

go run ./cmd/qc demo --profile normal --codec opus --seed 41
go run ./cmd/qc demo --profile burst --codec aac --seed 41 --fail-once audio.repair

# The go tool wraps a child exit code 2 as its own status 1 while printing
# "exit status 2". Build and execute ./qc directly to verify the CLI contract.
set +e
go run ./cmd/qc demo --profile corrupt --seed 41
GO_RUN_STATUS=$?
go build -o qc ./cmd/qc
./qc demo --profile corrupt --seed 41
BIN_STATUS=$?
set -e

if [[ "$BIN_STATUS" -ne 2 ]]; then
  echo "expected corrupt binary exit code 2, got $BIN_STATUS" >&2
  exit 1
fi

echo "acceptance_ok go_run_wrapper_status=$GO_RUN_STATUS binary_status=$BIN_STATUS"
