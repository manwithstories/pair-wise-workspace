#!/usr/bin/env bash
# 端到端验收脚本：依次跑成功组与损坏组，校验关键 token、退出码与可复现性。
# 纯本地运行，退出码非 0 即验收失败。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
RUN="${SCRIPT_DIR}/mediacheck.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
GOOD="${WORK}/good"
BAD="${WORK}/bad"
REBASE1="${WORK}/rebase1"
REBASE2="${WORK}/rebase2"

echo "== 1. go build / go test =="
(
	cd "${REPO_ROOT}"
	go build ./...
	go test ./...
)

echo "== 2. happy path =="
set +e
GOOD_OUT="$("${RUN}" demo --seed 7 --rate 48000 --layout stereo \
	--target-lufs -23 --target-kbps 6000 --out "${GOOD}")"
GOOD_CODE=$?
set -e
echo "${GOOD_OUT}"
[ "${GOOD_CODE}" -eq 0 ] || { echo "happy path exit=${GOOD_CODE}, want 0"; exit 1; }

for token in \
	"actual_kbps=5940-6060" "bitrate_delta_pct<=1.0" \
	"samples_in/samples_out" "peak_dbfs" "lufs" "color_conflicts" \
	"manifest_alignment_ms<=1.0" "fragment_gaps=1" "summary_sha256" \
	"status=ok"; do
	echo "${GOOD_OUT}" | grep -q "${token}" || { echo "missing token: ${token}"; exit 1; }
done
[ -f "${GOOD}/manifest.json" ] || { echo "success manifest missing"; exit 1; }

echo "== 3. corrupted tfdt regression rejected =="
set +e
BAD_OUT="$("${RUN}" demo --seed 7 --inject tfdt-regression --out "${BAD}")"
BAD_CODE=$?
set -e
echo "${BAD_OUT}"
[ "${BAD_CODE}" -eq 2 ] || { echo "reject exit=${BAD_CODE}, want 2"; exit 1; }
echo "${BAD_OUT}" | grep -q "status=rejected error=tfdt_regression" \
	|| { echo "missing reject token"; exit 1; }
[ ! -f "${BAD}/manifest.json" ] || { echo "no success manifest allowed on rejection"; exit 1; }

echo "== 4. rebase policy rerun reproducible =="
R1="$("${RUN}" demo --seed 7 --inject tfdt-regression --policy rebase --out "${REBASE1}")"
R2="$("${RUN}" demo --seed 7 --inject tfdt-regression --policy rebase --out "${REBASE2}")"
SHA1="$(echo "${R1}" | grep summary_sha256 | cut -d= -f2)"
SHA2="$(echo "${R2}" | grep summary_sha256 | cut -d= -f2)"
[ -n "${SHA1}" ] && [ "${SHA1}" = "${SHA2}" ] \
	|| { echo "rebase rerun sha mismatch: ${SHA1} vs ${SHA2}"; exit 1; }

echo "ALL ACCEPTANCE CHECKS PASSED"
