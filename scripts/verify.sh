#!/usr/bin/env bash
# scripts/verify.sh — 一键验收：构建、生成夹具、selftest、ctest
#
# 退出码（与 media_tool 约定一致）：
#   0 构建/处理/统计全部通过
#   1 验收不符
#   2 参数或依赖错误
#   3 输入损坏且无法恢复
set -euo pipefail

# 始终定位到仓库根目录（脚本位于 scripts/ 下）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

BUILD_DIR="${BUILD_DIR:-build}"
OUT_DIR="${OUT_DIR:-${BUILD_DIR}/demo}"
QUEUE_BLOCKS="${QUEUE_BLOCKS:-8}"
TARGET_KBPS="${TARGET_KBPS:-256}"

echo "==> [1/4] 检查依赖 (cmake / ffmpeg / Catch2)"
for tool in cmake ffmpeg; do
    if ! command -v "${tool}" >/dev/null 2>&1; then
        echo "缺少依赖: ${tool}，请先执行 brew install ffmpeg catch2 cmake" >&2
        exit 2
    fi
done

echo "==> [2/4] 配置与构建 (Release)"
cmake -S . -B "${BUILD_DIR}" -DCMAKE_BUILD_TYPE=Release
cmake --build "${BUILD_DIR}" -j

echo "==> [3/4] selftest：生成夹具并运行验收流水线"
# 不在 set -e 下直接退出，以便区分退出码 1/2/3 并给出提示
set +e
"${BUILD_DIR}/media_tool" selftest \
    --generate-fixtures \
    --queue-blocks "${QUEUE_BLOCKS}" \
    --target-kbps "${TARGET_KBPS}" \
    --out "${OUT_DIR}"
SELF_RC=$?
set -e

case "${SELF_RC}" in
    0) echo "selftest ACCEPT (rc=0)" ;;
    1) echo "selftest 验收不符 (rc=1)" >&2; exit 1 ;;
    2) echo "selftest 参数/依赖错误 (rc=2)" >&2; exit 2 ;;
    3) echo "selftest 输入损坏无法恢复 (rc=3)" >&2; exit 3 ;;
    *) echo "selftest 未知退出码 ${SELF_RC}" >&2; exit "${SELF_RC}" ;;
esac

echo "==> [4/4] ctest：单元与端到端测试"
ctest --test-dir "${BUILD_DIR}" --output-on-failure

echo
echo "全部验收通过 ✅"
echo "  构建目录 : ${BUILD_DIR}"
echo "  演示产物 : ${OUT_DIR}"
exit 0
