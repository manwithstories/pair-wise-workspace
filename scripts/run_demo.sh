#!/usr/bin/env bash
#
# run_demo.sh — popfix 端到端演示：
#   1. 生成确定性双声道夹具（含单点/声道分歧/连续削波三类异常）
#   2. analyze 检测并修复，落盘修复音频与 JSON 报告
#   3. verify 重读修复文件，核对帧数/声道/采样合法性与残差可复现
#   4. 检查终端摘要与 JSON 必备字段
#
# 纯本地、无外部服务、不读取真实录音。任一步失败立即退出并返回非零。
set -euo pipefail

# 定位构建产物：脚本位于 scripts/，构建目录默认在仓库根的 build/。
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
POPFIX="${POPFIX:-${ROOT_DIR}/build/popfix}"
OUT_DIR="${ROOT_DIR}/samples"

if [[ ! -x "${POPFIX}" ]]; then
  echo "未找到可执行文件 ${POPFIX}，请先运行:" >&2
  echo "  cmake -S . -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build -j2" >&2
  exit 2
fi

CLIPPED="${OUT_DIR}/clipped.wav"
REPAIRED="${OUT_DIR}/repaired.wav"
REPORT="${OUT_DIR}/report.json"

echo "==> [1/4] 生成夹具"
"${POPFIX}" generate-fixture --out "${CLIPPED}" --rate 48000 \
  --channels 2 --frames 96000 --seed 17

echo "==> [2/4] 分析并修复"
ANALYZE_OUT="$("${POPFIX}" analyze --in "${CLIPPED}" \
  --repair "${REPAIRED}" --report "${REPORT}" \
  --threshold 0.995 --context 32 --threads 4)"
echo "${ANALYZE_OUT}"

echo "==> [3/4] 校验修复结果"
"${POPFIX}" verify --original "${CLIPPED}" --repaired "${REPAIRED}" \
  --report "${REPORT}"

echo "==> [4/4] 检查关键字段"
fail=0
assert_contains() {  # $1=文本 $2=期望子串 $3=说明
  if [[ "$1" != *"$2"* ]]; then
    echo "  断言失败: 期望包含 '$2'（$3）" >&2
    fail=1
  else
    echo "  OK: $3"
  fi
}

# 终端摘要关键字段。
assert_contains "${ANALYZE_OUT}" "events=3" "事件数为 3"
assert_contains "${ANALYZE_OUT}" "repaired_frames=150" "修复帧数约 150"
assert_contains "${ANALYZE_OUT}" "channel_mask=" "含声道掩码"
assert_contains "${ANALYZE_OUT}" "peak_before=1.000000" "修复前满幅峰值"
assert_contains "${ANALYZE_OUT}" "frames=96000" "总帧数保持"

# JSON 报告必备字段（每个事件对象都应出现）。
for field in event_id start_frame end_frame channel_mask \
             repair_start repair_end residual_energy; do
  if grep -q "\"${field}\"" "${REPORT}"; then
    echo "  OK: JSON 字段 ${field}"
  else
    echo "  断言失败: JSON 缺少字段 ${field}" >&2
    fail=1
  fi
done

# peak_after 必须严格小于 1。
PEAK_AFTER="$(grep -o '"peak_after": [0-9.]*' "${REPORT}" | head -1 | cut -d' ' -f2)"
if awk "BEGIN{exit !(${PEAK_AFTER} < 1.0)}"; then
  echo "  OK: peak_after=${PEAK_AFTER} < 1.0"
else
  echo "  断言失败: peak_after=${PEAK_AFTER} 未降到 1.0 以下" >&2
  fail=1
fi

if [[ "${fail}" -ne 0 ]]; then
  echo "演示校验未通过" >&2
  exit 4
fi
echo "全部演示断言通过 ✔"
