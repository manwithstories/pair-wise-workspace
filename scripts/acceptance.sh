#!/usr/bin/env bash
# Deterministic local acceptance run for avpipe.
# All generated artifacts and logs stay under /tmp.
set -euo pipefail

BIN=/tmp/avpipe
CLEAN=/tmp/avpipe-clean
DAMAGED=/tmp/avpipe-damaged
TRUNCATED=/tmp/avpipe-truncated
RESUME=/tmp/avpipe-resume
LOGDIR=/tmp/avpipe-acceptance-logs
mkdir -p "$LOGDIR"

pass=0
fail=0

assert_grep() {
  local description="$1"
  local expected="$2"
  local file="$3"
  if grep -Fq "$expected" "$file"; then
    printf 'PASS %s (%s)\n' "$description" "$expected"
    pass=$((pass + 1))
  else
    printf 'FAIL %s: missing %q in %s\n' "$description" "$expected" "$file" >&2
    fail=$((fail + 1))
  fi
}

echo '== go mod tidy =='
go mod tidy
echo '== go build ./... =='
go build ./...
echo '== go test ./... =='
go test ./...
echo '== build acceptance binary =='
go build -o "$BIN" ./cmd/avpipe

echo '== clean =='
"$BIN" demo --seed 23 --case clean --bitrates 600000,1200000 --segments 12 --out "$CLEAN" \
  | tee "$LOGDIR/clean-demo.log"
"$BIN" verify --out "$CLEAN" | tee "$LOGDIR/clean-verify.log"
for token in \
  'roundtrip=100.00%' \
  'packets=1920' \
  'keyframe_hit=24/24' \
  'aligned_slots=12/12' \
  'segment_sha256=24/24'; do
  assert_grep clean "$token" "$LOGDIR/clean-verify.log"
done

echo '== damaged =='
"$BIN" demo --seed 23 --case damaged --bitrates 600000,1200000 --segments 12 --out "$DAMAGED" \
  | tee "$LOGDIR/damaged.log"
for token in \
  'cc_gap=1' \
  'psi_crc_reject=1' \
  'keyframe_hit=23/24' \
  'gap_slots=1'; do
  assert_grep damaged "$token" "$LOGDIR/damaged.log"
done
assert_grep damaged-manifest 'psi_crc_reject=1' "$DAMAGED/r0_600000.m3u8"
assert_grep damaged-manifest 'missing_idr=1' "$DAMAGED/r0_600000.m3u8"
assert_grep damaged-recovery 'psi_crc_mismatch' "$DAMAGED/recovery.json"
assert_grep damaged-recovery '"seq": 11' "$DAMAGED/recovery.json"

echo '== truncated =='
"$BIN" demo --seed 23 --case truncated --bitrates 600000,1200000 --segments 12 --out "$TRUNCATED" \
  | tee "$LOGDIR/truncated.log"
assert_grep truncated 'incomplete_tail=1' "$LOGDIR/truncated.log"
if find "$TRUNCATED/segments" -type f ! -size 0 | while read -r f; do
  size=$(stat -f%z "$f")
  test $((size % 188)) -eq 0 || { echo "partial packet in $f"; exit 1; }
done; then
  echo 'PASS truncated segments contain whole 188-byte packets only'
  pass=$((pass + 1))
else
  echo 'FAIL truncated segment included a partial TS packet' >&2
  fail=$((fail + 1))
fi

echo '== interrupt and resume =='
set +e
"$BIN" demo --seed 23 --case clean --bitrates 600000,1200000 --segments 12 \
  --interrupt-after 8 --out "$RESUME" >"$LOGDIR/interrupt.log" 2>&1
rc=$?
set -e
if test "$rc" -eq 75; then
  echo 'PASS interrupt exit code 75'
  pass=$((pass + 1))
else
  echo "FAIL interrupt rc=$rc want 75" >&2
  fail=$((fail + 1))
fi
"$BIN" resume --out "$RESUME" | tee "$LOGDIR/resume.log"
for token in \
  'checkpoint_reused=8' \
  'rebuilt=4' \
  'idempotent=1'; do
  assert_grep resume "$token" "$LOGDIR/resume.log"
done
"$BIN" verify --out "$RESUME" | tee "$LOGDIR/resume-verify.log"
assert_grep resume-verify 'segment_sha256=24/24' "$LOGDIR/resume-verify.log"

echo
echo "acceptance checks: pass=$pass fail=$fail"
if test "$fail" -ne 0; then
  exit 1
fi
