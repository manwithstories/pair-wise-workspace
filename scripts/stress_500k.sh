#!/usr/bin/env bash
# Performance harness: 500k rows must finish within 30s under 512MB.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
cargo run --release -- stress \
  --config configs/default.toml \
  --out out/stress \
  --rows 500000
