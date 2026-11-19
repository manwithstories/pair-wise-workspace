#!/usr/bin/env bash
# 运行两条验收演示命令（样例数据由程序生成）。
# 注意：损坏批次按设计以退出码 2 结束（含可恢复错误但报告已生成），
# 因此这里不使用 set -e，而是显式捕获并打印每条命令的退出码。
set -uo pipefail
cd "$(dirname "$0")/.."

echo "== 1) 合格批次（期望退出码 0）=="
./build/media_batch demo --work demo-run --target-bitrate 4000000 \
  --chunk-frames 120 --queue-capacity 8 --slow-consumer-ms 5 \
  --emit-frames 12 --json
rc1=$?
echo "exit=$rc1"

echo "== 2) 损坏批次（续跑，期望退出码 2）=="
./build/media_batch demo --work damaged-run --fixtures damaged --resume --json
rc2=$?
echo "exit=$rc2"

[ "$rc1" -eq 0 ] && [ "$rc2" -eq 2 ]
