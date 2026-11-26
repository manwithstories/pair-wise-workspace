#!/usr/bin/env bash
# Generate all binary fixtures from the deterministic Go sample generator.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

out="${1:-fixtures/generated}"
go run ./cmd/medialab gen-fixtures --out "$out"
