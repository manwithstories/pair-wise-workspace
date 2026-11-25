#!/usr/bin/env bash
# FieldPost local acceptance entry point.
# No network service is contacted beyond Go module dependency resolution.
set -euo pipefail

cd "$(dirname "$0")/.."

go version
go mod tidy
go build ./...
go test ./...
go run ./cmd/fieldpost demo --seed 42 --profile all --memory-cap 8388608
