#!/bin/sh
# End-to-end acceptance for the motion CLI.
# Usage: e2e.sh /path/to/motion
set -u
BIN="$1"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "E2E FAIL: $1" >&2; exit 1; }

# --- golden fixture: generate + analyze -------------------------------
"$BIN" generate --out "$TMP/fix" >/dev/null || fail "generate"
[ -f "$TMP/fix/frames.tsv" ] || fail "manifest missing"

"$BIN" analyze --manifest "$TMP/fix/frames.tsv" --enter 0.8 --exit 0.4 \
  --min-frames 2 --gap 0 --budget 26000 --out "$TMP/res" >"$TMP/out.txt" \
  || fail "analyze exit"
grep -qF 'frame_rows=12' "$TMP/out.txt" || fail "frame_rows"
grep -qF 'valid_pairs=11' "$TMP/out.txt" || fail "valid_pairs"
grep -qF 'intensity_0_count=6' "$TMP/out.txt" || fail "intensity_0"
grep -qF 'intensity_1_count=5' "$TMP/out.txt" || fail "intensity_1"
grep -qF 'histogram=[6,0,0,0,0,0,0,0,0,5]' "$TMP/out.txt" || fail "histogram"
grep -qF 'segments=[3,6),[9,11)' "$TMP/out.txt" || fail "segments"
grep -qF 'skips=0' "$TMP/out.txt" || fail "skips"
grep -qE '^summary_pixels_sum=[0-9]+$' "$TMP/out.txt" || fail "pixel sum"
grep -qE '^summary_fnv1a64=[0-9a-f]{16}$' "$TMP/out.txt" || fail "fnv"
[ -f "$TMP/res/frames.csv" ] || fail "frames.csv"
[ -f "$TMP/res/segments.csv" ] || fail "segments.csv"
[ -f "$TMP/res/histogram.ppm" ] || fail "histogram.ppm"
awk -F, 'NR>1 { if ($5==4000) h++; else if ($5==1000) l++ }
         END { exit !(h==5 && l==6) }' "$TMP/res/frames.csv" || fail "bits"
awk -F, 'NR>1 && $5!="" { s+=$5 } END { exit !(s==26000) }' \
  "$TMP/res/frames.csv" || fail "budget conservation"
grep -q '^3,6$' "$TMP/res/segments.csv" || fail "segment csv 1"
grep -q '^9,11$' "$TMP/res/segments.csv" || fail "segment csv 2"

# --- corrupt frame without --skip: exit 3, no result files ------------
cp -r "$TMP/fix" "$TMP/bad"
printf 'not a ppm' >"$TMP/bad/frame_004.ppm"
"$BIN" analyze --manifest "$TMP/bad/frames.tsv" --enter 0.8 --exit 0.4 \
  --min-frames 2 --gap 0 --budget 26000 --out "$TMP/res2" >/dev/null 2>&1
rc=$?
[ "$rc" -eq 3 ] || fail "corrupt exit code ($rc)"
[ ! -e "$TMP/res2/frames.csv" ] || fail "leftover frames.csv"
[ ! -e "$TMP/res2/histogram.ppm" ] || fail "leftover histogram.ppm"

# --- corrupt frame with --skip: exit 0, chain cut, reason recorded ----
"$BIN" analyze --manifest "$TMP/bad/frames.tsv" --enter 0.8 --exit 0.4 \
  --min-frames 2 --gap 0 --budget 26000 --skip --out "$TMP/res3" \
  >"$TMP/out3.txt" || fail "skip run"
grep -qF 'skips=1' "$TMP/out3.txt" || fail "skips=1"
grep -qF 'valid_pairs=9' "$TMP/out3.txt" || fail "valid_pairs after skip"
grep -qF 'skip index=4 reason=' "$TMP/out3.txt" || fail "skip reason"
grep -q '^4,.*,skipped,' "$TMP/res3/frames.csv" || fail "skipped csv row"

# --- frame size change: exit 3 ----------------------------------------
cp -r "$TMP/fix" "$TMP/badsize"
{ printf 'P6\n8 8\n255\n'; head -c 192 /dev/zero; } >"$TMP/badsize/frame_007.ppm"
"$BIN" analyze --manifest "$TMP/badsize/frames.tsv" --enter 0.8 --exit 0.4 \
  --min-frames 2 --gap 0 --budget 26000 --out "$TMP/res4" >/dev/null 2>&1
rc=$?
[ "$rc" -eq 3 ] || fail "size-change exit code ($rc)"

# --- parameter errors: exit 2 -----------------------------------------
"$BIN" analyze --manifest "$TMP/fix/frames.tsv" --enter 0.4 --exit 0.8 \
  --min-frames 2 --gap 0 --budget 26000 --out "$TMP/res5" >/dev/null 2>&1
rc=$?
[ "$rc" -eq 2 ] || fail "enter<exit exit code ($rc)"
"$BIN" analyze --manifest "$TMP/fix/frames.tsv" --enter 0.8 >/dev/null 2>&1
rc=$?
[ "$rc" -eq 2 ] || fail "missing-args exit code ($rc)"

# --- manifest validation: exit 3 --------------------------------------
printf '100\t../escape.ppm\n' >"$TMP/evil.tsv"
"$BIN" analyze --manifest "$TMP/evil.tsv" --enter 0.8 --exit 0.4 \
  --min-frames 2 --gap 0 --budget 26000 --out "$TMP/res6" >/dev/null 2>&1
rc=$?
[ "$rc" -eq 3 ] || fail "traversal exit code ($rc)"

echo "e2e ok"
