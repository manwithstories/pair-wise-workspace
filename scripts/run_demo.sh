#!/usr/bin/env bash
# Reproduce the deterministic end-to-end demo from a clean output directory.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

OUT="${1:-out/demo}"
echo "Running secpipe full pipeline (seed=42, self-check) -> $OUT"
cargo run --release -- all \
  --config configs/default.toml \
  --seed 42 \
  --out "$OUT" \
  --self-check
