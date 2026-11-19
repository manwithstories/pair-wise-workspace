#!/usr/bin/env bash
# 端到端验收脚本：构建、单元测试、正常演示（字段阈值断言）、截断演示（退出码 2）。
# 纯本地运行，无外部服务依赖。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
BUILD="$ROOT/build"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/media-deliver-demo.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

echo "==> 1/5 依赖检查"
for tool in cmake ninja ffmpeg; do
  command -v "$tool" >/dev/null || { echo "缺少依赖: $tool（请先 brew install ffmpeg catch2 cmake ninja）"; exit 1; }
done

echo "==> 2/5 配置与构建"
cmake -S . -B "$BUILD" -G Ninja -DCMAKE_BUILD_TYPE=Release
cmake --build "$BUILD"

echo "==> 3/5 单元测试"
ctest --test-dir "$BUILD" --output-on-failure

echo "==> 4/5 正常演示（12 秒，漂移 180ppm，回绕+VFR+坏包）"
NORMAL_OUT="$WORK/normal"
set +e
"$BUILD/media-deliver" demo \
  --seconds 12 --drift-ppm 180 --wrap-pts --vfr --corrupt-every 97 \
  --queue-packets 64 --target-bitrate 1800000 \
  --out "$NORMAL_OUT" --json "$WORK/report.json" | tee "$WORK/normal.txt"
RC=${PIPESTATUS[0]}
set -e
[ "$RC" -eq 0 ] || { echo "正常演示应返回 0，实际 $RC"; exit 1; }

# 必备字段存在性
for key in packets_in packets_repaired wraps drift_ppm queue_peak \
           target_bitrate_bps actual_bitrate_bps bitrate_error_pct \
           psnr_db manifest_alignment_ms seek_error_ms; do
  grep -q "^${key}=" "$WORK/normal.txt" || { echo "缺少输出字段: $key"; exit 1; }
done

# 阈值断言：码率偏差 <=8%、PSNR 25..45dB、对齐/落点误差 <=20ms、队列峰值 <=64。
get() { grep "^$1=" "$WORK/normal.txt" | cut -d= -f2; }
python3 - "$(get bitrate_error_pct)" "$(get psnr_db)" \
         "$(get manifest_alignment_ms)" "$(get seek_error_ms)" \
         "$(get queue_peak)" "$(get packets_in)" <<'PY'
import sys
berr, psnr, align, seek, peak, pin = map(float, sys.argv[1:])
checks = [
    (berr <= 8.0, f"bitrate_error_pct {berr} > 8"),
    (25.0 <= psnr <= 45.0, f"psnr_db {psnr} 不在 25..45"),
    (align <= 20.0, f"manifest_alignment_ms {align} > 20"),
    (seek <= 20.0, f"seek_error_ms {seek} > 20"),
    (peak <= 64.0, f"queue_peak {peak} > 64"),
    (pin >= 1000.0, f"packets_in {pin} 非千级"),
]
failed = [m for ok, m in checks if not ok]
if failed:
    print("\n".join(failed)); sys.exit(1)
print("阈值断言全部通过")
PY

echo "==> 5/5 截断演示（应返回 2 并报告首个坏包位置）"
TRUNC_OUT="$WORK/trunc"
set +e
"$BUILD/media-deliver" demo --truncate-at 43 --queue-packets 8 \
  --out "$TRUNC_OUT" > "$WORK/trunc.txt" 2>&1
RC=$?
set -e
[ "$RC" -eq 2 ] || { echo "截断演示应返回 2，实际 $RC"; cat "$WORK/trunc.txt"; exit 1; }
grep -q "first_bad_packet=43" "$WORK/trunc.txt" || { echo "未报告首个坏包位置"; cat "$WORK/trunc.txt"; exit 1; }
cat "$WORK/trunc.txt"

echo
echo "全部验收通过 ✅  报告: $WORK/report.json"
