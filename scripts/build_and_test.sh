#!/usr/bin/env bash
# 一键配置、构建并运行测试（macOS，纯本地）。
# 依赖：brew install cmake ffmpeg catch2
set -euo pipefail

cd "$(dirname "$0")/.."

cmake -S . -B build -DCMAKE_PREFIX_PATH="$(brew --prefix)"
cmake --build build
ctest --test-dir build --output-on-failure

echo "build_and_test: OK"
