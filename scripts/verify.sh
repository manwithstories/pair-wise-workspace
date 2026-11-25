#!/usr/bin/env bash
# Full local verification: download deps, vet, race-enabled tests and build.
# Exit code mirrors the first failing step (0 when everything passes).
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> go mod download"
go mod download

echo "==> go vet ./..."
go vet ./...

echo "==> gofmt check"
unformatted=$(gofmt -l . || true)
if [[ -n "$unformatted" ]]; then
	echo "unformatted files:" >&2
	echo "$unformatted" >&2
	exit 1
fi

echo "==> go test -race ./..."
go test -race ./...

echo "==> go build"
go build -o /tmp/avpipe ./cmd/avpipe

echo "VERIFY OK"
