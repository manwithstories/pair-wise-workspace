#!/usr/bin/env bash
# 自生成全部测试/演示夹具（纯本地，无外部素材引用）。
# 用法: scripts/make_fixtures.sh [输出目录] [可执行文件路径]
set -euo pipefail

DIR="${1:-./tmp}"
BIN="${2:-./build/media_pipeline}"

if [[ ! -x "$BIN" ]]; then
  echo "错误: 找不到可执行文件 $BIN，请先执行 CMake 构建" >&2
  exit 1
fi

mkdir -p "$DIR"
"$BIN" selftest --generate-fixtures --work "$DIR"
echo "夹具已生成到: $DIR"
ls -la "$DIR"/*.wav
