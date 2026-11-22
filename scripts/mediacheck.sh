#!/usr/bin/env bash
# mediacheck 演示/验收入口包装。
#
# 背景：不同 Go 工具链版本下 `go run` 对非零退出码的透传行为不一致
# （部分版本把程序的退出码 2 归一化为 1）。本脚本先构建到临时二进制再执行，
# 严格透传程序退出码：0 完成且指标通过，2 输入损坏被拒绝，1 运行/校验失败。
#
# 用法与 `go run ./cmd/mediacheck` 完全一致，例如：
#   scripts/mediacheck.sh demo --seed 7 --rate 48000 --layout stereo \
#       --target-lufs -23 --target-kbps 6000 --out /tmp/broadcast-demo
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

BIN_DIR="$(mktemp -d)"
trap 'rm -rf "${BIN_DIR}"' EXIT
BIN="${BIN_DIR}/mediacheck"

(
	cd "${REPO_ROOT}"
	go build -o "${BIN}" ./cmd/mediacheck
)

# 程序可能以 2 退出，临时关闭 errexit 以捕获并透传真实退出码。
set +e
"${BIN}" "$@"
code=$?
set -e
exit "${code}"
