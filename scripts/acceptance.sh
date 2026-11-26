#!/usr/bin/env bash
# Repeatable local acceptance run. No external services or media are used.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}/.."

echo "==> Go version"
go version

echo "==> Tidy modules"
go mod tidy

echo "==> Tests"
go test ./...

echo "==> Build /tmp/relay"
go build -o /tmp/relay ./cmd/relay

ROOT="${RELAY_DEMO_ROOT:-${TMPDIR:-/tmp}/relay-demo}"
echo "==> Demo root: ${ROOT}"
/tmp/relay demo \
  --root "${ROOT}" \
  --segments 1200 \
  --chunk-records 64 \
  --max-inflight-bytes 4194304 \
  --slow-every 7 \
  --interrupt-after 5
