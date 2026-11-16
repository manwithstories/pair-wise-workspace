#!/usr/bin/env bash
set -euo pipefail

: "${H264GOP:?H264GOP must point to h264gop}"
: "${GEN_FIXTURES:?GEN_FIXTURES must point to gen_fixtures}"

TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/h264gop-tests.XXXXXX")"
trap 'rm -rf "$TMP_ROOT"' EXIT
FIXTURES="$TMP_ROOT/fixtures"
OUTPUT=""
STATUS=0

fail() {
    echo "FAIL: $*" >&2
    if [[ -n "$OUTPUT" ]]; then
        echo "--- command output ---" >&2
        echo "$OUTPUT" >&2
    fi
    exit 1
}

assert_contains() {
    [[ "$OUTPUT" == *"$1"* ]] || fail "missing output: $1"
}

run_expect() {
    local expected="$1"
    shift
    set +e
    OUTPUT="$("$@" 2>&1)"
    STATUS=$?
    set -e
    [[ "$STATUS" -eq "$expected" ]] || fail "expected exit $expected, got $STATUS: $*"
}

[[ -x "$H264GOP" ]] || fail "missing h264gop binary"
[[ -x "$GEN_FIXTURES" ]] || fail "missing gen_fixtures binary"

run_expect 0 "$GEN_FIXTURES" --out "$FIXTURES" --cases 6
for name in baseline_3byte baseline_4byte multislice mbaff_multi_slice \
            interlaced_mix sps_change; do
    [[ -s "$FIXTURES/$name.h264" ]] || fail "fixture not generated: $name"
done

run_expect 0 "$H264GOP" probe "$FIXTURES/baseline_3byte.h264"
assert_contains "sequences=1 gops=1 frames=8 i=1 p=3 b=4 slices=8 anomalous=0"

run_expect 0 "$H264GOP" probe "$FIXTURES/baseline_4byte.h264"
assert_contains "frames=8"

run_expect 0 "$H264GOP" probe "$FIXTURES/multislice.h264"
assert_contains "sequences=1 gops=1 frames=15"
assert_contains "slices=18 anomalous=0"

run_expect 0 "$H264GOP" probe "$FIXTURES/mbaff_multi_slice.h264"
assert_contains "sequences=1 gops=4 frames=120 i=4 p=52 b=64 slices=137 anomalous=0"
assert_contains "sps chroma_format_idc=1"
assert_contains "frame_mbs_only_flag=0 mbaff=1"
assert_contains "crop=1"

run_expect 0 "$H264GOP" probe "$FIXTURES/sps_change.h264"
assert_contains "sequences=2 gops=2 frames=18"

run_expect 0 "$H264GOP" verify "$FIXTURES/baseline_3byte.h264" --expect-progressive
assert_contains "verify: OK progressive"

run_expect 2 "$H264GOP" verify "$FIXTURES/interlaced_mix.h264" --expect-progressive
assert_contains "verify: FAIL offset=0"

run_expect 3 "$H264GOP"
run_expect 3 "$H264GOP" probe
run_expect 1 "$H264GOP" probe "$TMP_ROOT/does-not-exist.h264"
assert_contains "cannot open input file"

printf '\x11\x22\x33' > "$TMP_ROOT/no_start.h264"
run_expect 1 "$H264GOP" probe "$TMP_ROOT/no_start.h264"
assert_contains "no Annex B start code"

printf '\x00\x00\x00\x01\x60\x80' > "$TMP_ROOT/unknown_nal.h264"
run_expect 1 "$H264GOP" probe "$TMP_ROOT/unknown_nal.h264"
assert_contains "unsupported NAL unit type 0"
assert_contains "byte offset 0"

head -c 8 "$FIXTURES/baseline_3byte.h264" > "$TMP_ROOT/truncated.h264"
run_expect 1 "$H264GOP" probe "$TMP_ROOT/truncated.h264"
assert_contains "SPS parse failed"
assert_contains "byte offset 0"

cp "$FIXTURES/baseline_3byte.h264" "$TMP_ROOT/residual.h264"
printf '\x00\x00\x01' >> "$TMP_ROOT/residual.h264"
run_expect 0 "$H264GOP" probe "$TMP_ROOT/residual.h264"
assert_contains "anomalous=1"
assert_contains "residual NAL without header"

JSON_PATH="$TMP_ROOT/newdir/index.json"
run_expect 0 "$H264GOP" index "$FIXTURES" --json "$JSON_PATH"
assert_contains "indexed files=6"
[[ -s "$JSON_PATH" ]] || fail "JSON output missing"
grep -q '"sequences":\[{"file"' "$JSON_PATH" || fail "JSON has no sequences array"
grep -q '"sps":{' "$JSON_PATH" || fail "JSON has no SPS object"
grep -q '"profile_idc":100' "$JSON_PATH" || fail "JSON SPS profile missing"
grep -q '"chroma_format_idc":1' "$JSON_PATH" || fail "JSON SPS chroma missing"
grep -q '"frame_mbs_only_flag":false' "$JSON_PATH" || fail "JSON SPS field flag missing"
grep -q '"log2_max_frame_num":4' "$JSON_PATH" || fail "JSON SPS frame_num field missing"
grep -q '"gops":\[{"sequence_id"' "$JSON_PATH" || fail "JSON GOP data missing"
grep -Eq '"start_offset":[0-9]+' "$JSON_PATH" || fail "JSON offset is not decimal"
grep -q '"frame_count":193' "$JSON_PATH" || fail "JSON frame total mismatch"

GOP_COUNT="$(grep -o '"sequence_id":' "$JSON_PATH" | wc -l | tr -d ' ')"
[[ "$GOP_COUNT" -eq 10 ]] || fail "expected 10 GOP records, got $GOP_COUNT"

echo "end-to-end tests: OK"
