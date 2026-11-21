#!/bin/sh
# Acceptance gate for mediad. Runs the documented commands and fails on any
# threshold violation or unexpected exit code.
#
# Usage: ./scripts/acceptance.sh [output-root]
# The output root defaults to ./tmp/acceptance and is rebuilt each run.
set -eu

ROOT=${1:-./tmp/acceptance}
BIN="$ROOT/bin/mediad"

rm -rf "$ROOT"
mkdir -p "$ROOT/bin"

echo "== tidy =="
go mod tidy

echo "== build =="
go build ./...
go build -o "$BIN" ./cmd/mediad

echo "== test =="
go test ./...

echo "== successful demo =="
set +e
"$BIN" demo --seed 42 --bandwidth-kbps 1800 --loss-burst 4 --retry 2 \
  --out "$ROOT/demo" >"$ROOT/demo.out" 2>"$ROOT/demo.err"
STATUS=$?
set -e
cat "$ROOT/demo.out"
if [ "$STATUS" -ne 0 ]; then
  echo "FAIL: success path exited $STATUS" >&2
  cat "$ROOT/demo.err" >&2
  exit 1
fi

field() {
  sed -n "s/^$1=//p" "$ROOT/demo.out" | head -n1
}

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

# Compare a decimal field to a bound using awk (POSIX, no bc dependency).
le() { awk -v v="$1" -v b="$2" 'BEGIN { exit !(v <= b) }'; }
ge() { awk -v v="$1" -v b="$2" 'BEGIN { exit !(v >= b) }'; }

FIELD_ERR=$(field bitrate_error_pct)
FIELD_PSNR=$(field psnr_mean_db)
FIELD_SSIM=$(field ssim_mean)
FIELD_DRIFT=$(field manifest_drift_ms)
FIELD_FRAMES=$(field frame_count)
FIELD_BLOCKED=$(field blocked_node)
FIELD_LOST=$(field lost_packets)

[ -n "$FIELD_ERR" ] || fail "missing bitrate_error_pct"
le "$FIELD_ERR" 8 || fail "bitrate_error_pct $FIELD_ERR > 8"
ge "$FIELD_PSNR" 30 || fail "psnr_mean_db $FIELD_PSNR < 30"
ge "$FIELD_SSIM" 0.90 || fail "ssim_mean $FIELD_SSIM < 0.90"
le "$FIELD_DRIFT" 1 || fail "manifest_drift_ms $FIELD_DRIFT > 1"
[ "$FIELD_FRAMES" -gt 0 ] || fail "frame_count is zero"
[ "$FIELD_BLOCKED" = "none" ] || fail "blocked_node $FIELD_BLOCKED != none"
[ "$FIELD_LOST" = "0" ] || fail "lost_packets $FIELD_LOST != 0"

echo "== corrupted metadata =="
set +e
"$BIN" demo --seed 42 --corrupt-metadata --out "$ROOT/bad" \
  >"$ROOT/bad.out" 2>"$ROOT/bad.err"
STATUS=$?
set -e
cat "$ROOT/bad.err" >&2
if [ "$STATUS" -ne 2 ]; then
  fail "corrupt path exited $STATUS, want 2"
fi
grep -q 'category=metadata' "$ROOT/bad.err" || fail "corrupt error lacks category"
grep -q 'track=' "$ROOT/bad.err" || fail "corrupt error lacks track"
grep -q 'blocked_node=' "$ROOT/bad.err" || fail "corrupt error lacks blocking dependency"

echo "== usage error =="
set +e
"$BIN" demo --out "$ROOT/usage" --bandwidth-kbps 0 >/dev/null 2>&1
STATUS=$?
set -e
[ "$STATUS" -eq 1 ] || fail "usage path exited $STATUS, want 1"

echo "== burst-loss linkage =="
"$BIN" demo --seed 42 --scenario burst-loss --loss-burst 4 --out "$ROOT/burst" \
  >"$ROOT/burst.out"
grep -q 'lost_packets=4' "$ROOT/burst.out" || fail "burst loss not reported"
grep -q 'blocked_node=transport:gap' "$ROOT/burst.out" || fail "burst did not block frames"
grep -q '^gap_blocked=' "$ROOT/burst.out" || fail "gap/frame linkage missing"

echo "== resume =="
"$BIN" demo --seed 42 --out "$ROOT/first" >/dev/null
"$BIN" demo --seed 42 --out "$ROOT/second" --resume-from "$ROOT/first/manifest.json" \
  >"$ROOT/resume.out"
grep -q 'blocked_node=none' "$ROOT/resume.out" || fail "resume blocked"
if command -v cmp >/dev/null 2>&1; then
  # Frame artifacts are the strongest evidence of idempotent resume.
  cmp "$ROOT/first/frames/frame_0000.ppm" "$ROOT/second/frames/frame_0000.ppm" \
    || fail "resumed frame differs from original"
fi

echo "PASS: all acceptance checks satisfied"
