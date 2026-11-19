#!/bin/bash
# Acceptance driver for media_repair.
#
# Generates the reference clip plus a set of self-made damaged fixtures, runs
# repair/verify over each, and prints a one-line PASS/FAIL summary per case
# together with the acceptance metrics. Exit code 0 means every case behaved as
# specified; 1 means an acceptance metric was missed; 2 means the environment
# or arguments are wrong.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${ROOT}/build/media_repair"
WORK="${ROOT}/build/acceptance"

SEED="${SEED:-42}"
FRAMES="${FRAMES:-1200}"
SEGMENT_FRAMES="${SEGMENT_FRAMES:-120}"
LOSS_BURST="${LOSS_BURST:-7}"
DUPLICATE_RATE="${DUPLICATE_RATE:-0.08}"
REORDER_WINDOW="${REORDER_WINDOW:-16}"

# shellcheck source=lib/acceptance_common.sh
source "${ROOT}/scripts/lib/acceptance_common.sh"

# ---------------------------------------------------------------------------
# Reference clip
# ---------------------------------------------------------------------------
"${BIN}" generate --seed "${SEED}" --frames "${FRAMES}" \
  --loss-burst "${LOSS_BURST}" --duplicate-rate "${DUPLICATE_RATE}" \
  --reorder-window "${REORDER_WINDOW}" --out "${WORK}/sample.mfs" \
  > "${WORK}/generate.json" 2> "${WORK}/generate.err"
if [[ $? -ne 0 ]]; then
  echo "error: sample generation failed:" >&2
  cat "${WORK}/generate.err" >&2
  exit 2
fi

"${BIN}" repair --input "${WORK}/sample.mfs" \
  --segment-frames "${SEGMENT_FRAMES}" --json "${WORK}/result.json" \
  > "${WORK}/repair.json" 2> "${WORK}/repair.err"
repair_status=$?

"${BIN}" verify --input "${WORK}/sample.mfs" --report "${WORK}/result.json" \
  > "${WORK}/verify.json" 2> "${WORK}/verify.err"
verify_status=$?

roundtrip=$(read_metric roundtrip_match_rate "${WORK}/verify.json")
index_hit=$(read_metric keyframe_index_hit_rate "${WORK}/verify.json")
segment_hash=$(read_metric segment_hash_match_rate "${WORK}/verify.json")
status=$(read_string status "${WORK}/verify.json")
repaired=$(read_metric repaired_frames "${WORK}/verify.json")
duplicates=$(read_metric duplicate_packets "${WORK}/verify.json")
lost=$(read_metric lost_ranges "${WORK}/verify.json")
decode_errors=$(read_metric decode_errors "${WORK}/verify.json")
first_error=$(read_metric first_error_offset "${WORK}/verify.json")

echo "reference clip: seed=${SEED} frames=${FRAMES} segment_frames=${SEGMENT_FRAMES}"
echo "metrics:"
echo "  roundtrip_match_rate      ${roundtrip}"
echo "  keyframe_index_hit_rate   ${index_hit}"
echo "  segment_hash_match_rate   ${segment_hash}"
echo "  repaired_frames           ${repaired}"
echo "  duplicate_packets         ${duplicates}"
echo "  lost_ranges               ${lost}"
echo "  decode_errors             ${decode_errors}"
echo "  first_error_offset        ${first_error}"
echo

if [[ "${repair_status}" -ne 0 ]]; then
  print_case "repair exits 0" "FAIL" "exit=${repair_status}"
else
  print_case "repair exits 0" "PASS" ""
fi

if [[ "${verify_status}" -eq 0 && "${status}" == "pass" ]]; then
  print_case "verify reports pass" "PASS" ""
else
  print_case "verify reports pass" "FAIL" "exit=${verify_status} status=${status}"
fi

expect_metric "roundtrip_match_rate" "${roundtrip}" "0.99"
expect_metric "keyframe_index_hit_rate" "${index_hit}" "0.98"
expect_metric "segment_hash_match_rate" "${segment_hash}" "1.00"

if [[ "${decode_errors}" == "0" ]]; then
  print_case "clean clip has no decode errors" "PASS" ""
else
  print_case "clean clip has no decode errors" "FAIL" "${decode_errors}"
fi

# ---------------------------------------------------------------------------
# Damaged fixtures, generated from the reference clip itself
# ---------------------------------------------------------------------------
make_fixture corrupt_checksum checksum
make_fixture length_overflow length
make_fixture truncated_tail tail
make_fixture bad_magic magic
make_fixture unknown_required_field required
make_fixture truncated_header header
make_fixture empty_sequence empty

echo
echo "damaged fixtures:"

run_case_expect corrupt_checksum checksum_mismatch
run_case_expect length_overflow length_overflow
run_case_expect truncated_tail record_truncated
run_case_expect bad_magic record_magic_mismatch
run_case_expect unknown_required_field unknown_required_field

# A truncated header must be rejected as input-format error (exit 2).
"${BIN}" repair --input "${WORK}/truncated_header.mfs" \
  --segment-frames "${SEGMENT_FRAMES}" --json "${WORK}/truncated_header.json" \
  > "${WORK}/truncated_header.out" 2> "${WORK}/truncated_header.err"
code=$?
if [[ ${code} -eq 2 ]]; then
  print_case "truncated_header" "PASS" "rejected with exit 2 at offset 0"
else
  print_case "truncated_header" "FAIL" "exit=${code}"
fi

# A header-only file is an empty sequence: repair succeeds but reports it.
"${BIN}" repair --input "${WORK}/empty_sequence.mfs" \
  --segment-frames "${SEGMENT_FRAMES}" --json "${WORK}/empty_sequence.json" \
  > "${WORK}/empty_sequence.out" 2> "${WORK}/empty_sequence.err"
code=$?
if [[ ${code} -eq 0 ]] && grep -q '"empty_sequence": true' "${WORK}/empty_sequence.json"; then
  print_case "empty_sequence" "PASS" "reported as empty sequence"
else
  print_case "empty_sequence" "FAIL" "exit=${code}"
fi

# ---------------------------------------------------------------------------
# Reproducibility: the same seed must produce identical bytes.
# ---------------------------------------------------------------------------
"${BIN}" generate --seed "${SEED}" --frames "${FRAMES}" \
  --loss-burst "${LOSS_BURST}" --duplicate-rate "${DUPLICATE_RATE}" \
  --reorder-window "${REORDER_WINDOW}" --out "${WORK}/sample_repeat.mfs" \
  > /dev/null 2>&1
if cmp -s "${WORK}/sample.mfs" "${WORK}/sample_repeat.mfs"; then
  print_case "seed reproducibility" "PASS" "identical bytes"
else
  print_case "seed reproducibility" "FAIL" "output differs"
fi

# ---------------------------------------------------------------------------
# Argument handling: bad input must be exit code 2.
# ---------------------------------------------------------------------------
"${BIN}" repair --input "${WORK}/does_not_exist.mfs" --segment-frames 10 \
  --json "${WORK}/missing.json" > /dev/null 2>&1
code=$?
if [[ ${code} -eq 2 ]]; then
  print_case "missing input file" "PASS" "exit 2"
else
  print_case "missing input file" "FAIL" "exit=${code}"
fi

"${BIN}" repair --input "${WORK}/sample.mfs" > /dev/null 2>&1
code=$?
if [[ ${code} -eq 2 ]]; then
  print_case "missing arguments" "PASS" "exit 2"
else
  print_case "missing arguments" "FAIL" "exit=${code}"
fi

finish
