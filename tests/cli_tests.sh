#!/bin/sh
# End-to-end command-line checks for mediaqc.  Runs the real binary against
# self-generated fixtures and asserts the accepted output format and exit
# codes.  Pure local execution, no network, no fixed absolute paths.

set -u

BIN="$1"
if [ -z "${BIN}" ] || [ ! -x "${BIN}" ]; then
  echo "usage: cli_tests.sh <path-to-mediaqc>"
  exit 99
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mediaqc-cli.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# 1. Generate + process the boundary scenario: damage found => exit 3.
"${BIN}" generate --scenario boundary --out "${WORK}/sample" --frames 1200 >/dev/null \
  || fail "generate boundary failed"

OUT="$("${BIN}" process --input "${WORK}/sample" --output "${WORK}/out" \
        --queue-frames 8 --checkpoint "${WORK}/state")"
RC=$?
[ "${RC}" -eq 3 ] || fail "boundary process exit ${RC}, expected 3"

for field in frames_total frames_repaired first_corrupt_offset \
             peak_queue_frames resume_ms output_sha256 index_entries; do
  echo "${OUT}" | grep -q "^${field}=" || fail "missing output field ${field}"
done

# Queue peak must respect the configured cap.
PEAK="$(echo "${OUT}" | sed -n 's/^peak_queue_frames=//p')"
[ -n "${PEAK}" ] && [ "${PEAK}" -le 8 ] || fail "peak_queue_frames ${PEAK} > 8"

# Corrupt offset must be a non-negative byte offset.
OFFSET="$(echo "${OUT}" | sed -n 's/^first_corrupt_offset=//p')"
[ "${OFFSET}" -ge 0 ] || fail "first_corrupt_offset not located"

# Checksum is exactly 64 hex digits.
SHA="$(echo "${OUT}" | sed -n 's/^output_sha256=//p')"
echo "${SHA}" | grep -Eq '^[0-9a-f]{64}$' || fail "output_sha256 not 64 hex digits"

# resume_ms is an integer.
RESUME="$(echo "${OUT}" | sed -n 's/^resume_ms=//p')"
echo "${RESUME}" | grep -Eq '^[0-9]+$' || fail "resume_ms not an integer"

# 2. Verify the output directory: exits 0 when intact.
"${BIN}" verify --input "${WORK}/out" >/dev/null || fail "verify failed on clean output"

# 3. A clean scenario exits 0.
"${BIN}" generate --scenario normal --out "${WORK}/clean" --frames 600 >/dev/null \
  || fail "generate normal failed"
"${BIN}" process --input "${WORK}/clean" --output "${WORK}/clean-out" >/dev/null \
  || fail "clean process did not exit 0"

# 4. Invalid arguments exit 2.
"${BIN}" process --input "${WORK}/clean" >/dev/null 2>&1
[ $? -eq 2 ] || fail "missing --output should exit 2"

"${BIN}" generate --scenario does-not-exist --out "${WORK}/x" >/dev/null 2>&1
[ $? -eq 2 ] || fail "unknown scenario should exit 2"

"${BIN}" frobnicate >/dev/null 2>&1
[ $? -eq 2 ] || fail "unknown command should exit 2"

# 5. Verify detects a tampered segment => exit 3.
SEG="$(ls "${WORK}/out"/media/*.mp4 | head -n1)"
printf 'X' | dd of="${SEG}" bs=1 seek=200 conv=notrunc >/dev/null 2>&1
"${BIN}" verify --input "${WORK}/out" >/dev/null 2>&1
[ $? -eq 3 ] || fail "verify should exit 3 on corrupted segment"

# 6. Resume: interrupt then complete; output verifies.
"${BIN}" generate --scenario normal --out "${WORK}/r" --frames 2000 >/dev/null \
  || fail "generate resume fixture failed"
"${BIN}" process --input "${WORK}/r" --output "${WORK}/r-out" \
  --checkpoint "${WORK}/r-state" --stop-after 700 >/dev/null \
  || fail "interrupted run failed"
[ -f "${WORK}/r-state" ] || fail "checkpoint not written"
"${BIN}" process --input "${WORK}/r" --output "${WORK}/r-out" \
  --checkpoint "${WORK}/r-state" >/dev/null || fail "resume run failed"
[ ! -f "${WORK}/r-state" ] || fail "checkpoint not consumed after success"
"${BIN}" verify --input "${WORK}/r-out" >/dev/null || fail "resumed output failed verify"

# 7. Query returns a located entry.
"${BIN}" query --input "${WORK}/r-out" --track video_main --frame 100 >/dev/null \
  || fail "query by frame failed"

echo "cli checks passed"
exit 0
