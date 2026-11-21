#!/bin/sh
# Reproduces the acceptance demo against a throwaway directory.  Generates a
# boundary delivery, processes it with a bounded queue and checkpoint, verifies
# the output, and demonstrates interrupt + resume.  Pure local execution.
#
# Usage: scripts/demo.sh [path-to-mediaqc]
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${1:-${ROOT}/build/mediaqc}"
if [ ! -x "${BIN}" ]; then
  echo "mediaqc binary not found at ${BIN}; build it first (cmake --build build)" >&2
  exit 1
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mediaqc-demo.XXXXXX")"
trap 'echo "workdir: ${WORK}"' EXIT
echo "workdir: ${WORK}"

echo "==> generate boundary delivery (10,000 frames)"
"${BIN}" generate --scenario boundary --out "${WORK}/sample"

echo "==> process with queue bound 8 and a checkpoint"
set +e
"${BIN}" process --input "${WORK}/sample" --output "${WORK}/out" \
  --queue-frames 8 --checkpoint "${WORK}/state"
PROCESS_RC=$?
set -e
echo "process exit code: ${PROCESS_RC} (3 = damage found, as designed)"

echo "==> verify output"
"${BIN}" verify --input "${WORK}/out"
echo "verify exit code: $?"

echo "==> query frame 100 on the video track"
"${BIN}" query --input "${WORK}/out" --track video_main --frame 100

echo "==> interrupt after 600 frames, then resume"
"${BIN}" generate --scenario normal --out "${WORK}/resume-sample" --frames 2000
"${BIN}" process --input "${WORK}/resume-sample" --output "${WORK}/resume-out" \
  --checkpoint "${WORK}/resume-state" --stop-after 600
"${BIN}" process --input "${WORK}/resume-sample" --output "${WORK}/resume-out" \
  --checkpoint "${WORK}/resume-state"
"${BIN}" verify --input "${WORK}/resume-out"
