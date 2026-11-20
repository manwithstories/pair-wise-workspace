#!/usr/bin/env bash
# 生成可复现的 PCM/WAV 验收样例（固定种子 12785，纯本地、仅标准库）。
# 用法: bash scripts/make_fixtures.sh [输出目录，默认 build/fixtures]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUTDIR="${1:-build/fixtures}"

# -I: 隔离模式，忽略环境变量与当前目录中的同名模块，防止 planted json.py。
python3 -I "$SCRIPT_DIR/_make_fixtures.py" "$OUTDIR"
