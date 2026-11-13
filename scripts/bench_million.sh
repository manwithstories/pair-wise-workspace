#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
BENCH_DIR="$(mktemp -d "${TMPDIR:-/tmp}/soc-correlator-bench.XXXXXX")"
echo "生成、入库、关联与报告：1,000,000 原始行"
echo "输出目录：$BENCH_DIR"
/usr/bin/time -p cargo run --release -- demo --seed 42 --raw-lines 1000000 --out "$BENCH_DIR"
