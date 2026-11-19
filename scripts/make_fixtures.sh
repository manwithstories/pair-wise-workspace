#!/usr/bin/env bash
# 生成确定性自测素材（回绕/漂移/VFR/截断/损坏包）。
# 纯本地运行：调用已构建的 media-audit fixtures，不访问网络、不引用外部素材。
#
# 用法:
#   scripts/make_fixtures.sh [seed] [输出目录]
# 示例:
#   scripts/make_fixtures.sh 42 ./generated
set -euo pipefail

SEED="${1:-42}"
OUT_DIR="${2:-./generated}"

# 定位可执行文件：优先 build/，其次 PATH。
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN="${REPO_ROOT}/build/media-audit"
if [[ ! -x "${BIN}" ]]; then
  if command -v media-audit >/dev/null 2>&1; then
    BIN="media-audit"
  else
    echo "错误: 未找到 media-audit，请先执行 cmake --build build" >&2
    exit 4
  fi
fi

echo "生成夹具: seed=${SEED} 输出目录=${OUT_DIR}"
"${BIN}" fixtures --seed "${SEED}" --out-dir "${OUT_DIR}"

echo "生成文件:"
for f in scene_normal scene_vfr scene_drift scene_wrap scene_corrupt scene_truncated; do
  p="${OUT_DIR}/${f}.mp4"
  if [[ -f "${p}" ]]; then
    # macOS stat 字段与 GNU 不同，统一用 -f。
    size="$(stat -f '%z' "${p}")"
    echo "  ${p} (${size} bytes)"
  else
    echo "  缺失: ${p}" >&2
    exit 4
  fi
done
