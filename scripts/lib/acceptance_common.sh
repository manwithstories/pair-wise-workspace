#!/bin/bash
# Shared helpers for the acceptance driver. Sourced, never executed directly.
#
# Callers set ROOT/BIN/WORK and the clip parameters before sourcing, then use
# print_case/expect_metric/read_metric to report one line per check.

set -uo pipefail

if [[ ! -x "${BIN}" ]]; then
  echo "error: ${BIN} not built. Run: cmake --build build" >&2
  exit 2
fi

mkdir -p "${WORK}"
failures=0

print_case() {
  local name="$1" status="$2" detail="$3"
  printf '%-32s %-4s %s\n' "${name}" "${status}" "${detail}"
  if [[ "${status}" != "PASS" ]] && [[ "${status}" != "INFO" ]]; then
    failures=$((failures + 1))
  fi
}

# Extract a numeric metric from a JSON line such as "key": 0.99,
read_metric() {
  sed -n "s/.*\"$1\": \([0-9.-]*\).*/\1/p" "$2" | head -n 1
}

read_string() {
  sed -n "s/.*\"$1\": \"\([^\"]*\)\".*/\1/p" "$2" | head -n 1
}

expect_metric() {
  local name="$1" value="$2" floor="$3"
  if awk -v v="${value}" -v f="${floor}" 'BEGIN { exit !(v + 0 >= f + 0) }'; then
    print_case "${name} >= ${floor}" "PASS" "${value}"
  else
    print_case "${name} >= ${floor}" "FAIL" "${value}"
  fi
}

# ---------------------------------------------------------------------------
# Damaged fixtures
# ---------------------------------------------------------------------------
# Each fixture is produced by the tool itself, so its declared offset is the
# ground truth the repair report is checked against.
: > "${WORK}/fixture_offsets.txt"

make_fixture() {
  local name="$1" kind="$2"
  local out="${WORK}/${name}.mfs"
  "${BIN}" damage --input "${WORK}/sample.mfs" --kind "${kind}" --out "${out}" \
    | sed -n 's/.*"offset": \([0-9]*\).*/\1/p' > "${WORK}/${name}.offset"
  if [[ ! -s "${WORK}/${name}.offset" ]]; then
    echo "error: failed to build fixture ${name}" >&2
    exit 2
  fi
  echo "${name} $(cat "${WORK}/${name}.offset")" >> "${WORK}/fixture_offsets.txt"
}

# Repairs a fixture and checks that the reported first error offset equals the
# offset the builder recorded, and that the expected error type is present.
run_case_expect() {
  local name="$1" expected_type="$2"
  local out="${WORK}/${name}.json"
  "${BIN}" repair --input "${WORK}/${name}.mfs" \
    --segment-frames "${SEGMENT_FRAMES}" --json "${out}" \
    > "${out}.stdout" 2> "${out}.stderr"
  local code=$?
  if [[ ${code} -ne 0 ]]; then
    print_case "${name}" "FAIL" "repair exit=${code}"
    return
  fi
  local offset frames expected
  offset=$(read_metric first_error_offset "${out}")
  frames=$(read_metric repaired_frames "${out}")
  expected=$(sed -n "s/^${name} //p" "${WORK}/fixture_offsets.txt" | head -n 1)
  if [[ -z "${expected}" ]]; then
    print_case "${name}" "FAIL" "missing expected offset"
  elif [[ "${offset}" != "${expected}" ]]; then
    print_case "${name}" "FAIL" "offset=${offset} expected=${expected}"
  elif ! grep -q "\"type\": \"${expected_type}\"" "${out}"; then
    print_case "${name}" "FAIL" "missing ${expected_type}"
  elif [[ "${frames}" -le 0 ]]; then
    print_case "${name}" "FAIL" "no frames recovered"
  else
    print_case "${name}" "PASS" "offset=${offset} frames=${frames}"
  fi
}

finish() {
  echo
  if [[ ${failures} -eq 0 ]]; then
    echo "acceptance: PASS"
    exit 0
  fi
  echo "acceptance: FAIL (${failures} case(s))"
  exit 1
}
