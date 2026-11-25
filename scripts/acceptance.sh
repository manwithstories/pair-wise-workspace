#!/usr/bin/env bash
# Local acceptance flow for archivectl.
# Mirrors the commands in the task statement. No network asset is fetched
# beyond Go module resolution; all media fixtures are generated in-code.
set -euo pipefail

cd "$(dirname "$0")/.."

go version
go mod tidy
go mod download
go build -o "${TMPDIR:-/tmp}/archivectl" ./cmd/archiver
go test ./...

WORK="$(mktemp -d "${TMPDIR:-/tmp}/archive.XXXXXX")"
echo "WORK=$WORK"

"${TMPDIR:-/tmp}/archivectl" demo \
  --root "$WORK" --seed 41 --segments 48 \
  --fault-profile boundary-v1 --resume-check

"${TMPDIR:-/tmp}/archivectl" verify --root "$WORK" --repeat 2
