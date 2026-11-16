#!/usr/bin/env bash
# End-to-end acceptance for mp4loc. Runs the demo flow from the task
# brief, checks expected keys/ranges/hash format, and verifies exit
# codes on the fault-injection paths. Build artifacts stay in a
# mktemp dir — nothing lands in the repo.
set -euo pipefail
cd "$(dirname "$0")/.."

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN="$TMP/mp4loc"
go build -o "$BIN" ./cmd/mp4loc

fail() { echo "CHECK FAILED: $*" >&2; exit 1; }

# --- 1. fixture -------------------------------------------------------------
"$BIN" fixture --out "$TMP/sample.mp4" --video-frames 240 --audio-frames 480 \
  --edit-list --timestamp-wrap --seed 42 >/dev/null || fail "fixture exit"
[ -s "$TMP/sample.mp4" ] || fail "fixture file missing"

# --- 2. inspect -------------------------------------------------------------
INSPECT="$("$BIN" inspect --input "$TMP/sample.mp4" --format text)" || fail "inspect exit"
for k in BOX_COUNT TRACK_COUNT VIDEO_TRACK_ID TIMESCALE SAMPLE_COUNT \
         KEYFRAME_COUNT EDIT_SEGMENT_COUNT WRAP_EVENTS; do
  grep -q "^$k: " <<<"$INSPECT" || fail "inspect missing $k"
done
val() { grep "^$1: " <<<"$INSPECT" | head -1 | awk '{print $2}'; }
[ "$(val BOX_COUNT)" -ge 20 ] && [ "$(val BOX_COUNT)" -le 80 ] || fail "BOX_COUNT out of [20,80]"
[ "$(val TRACK_COUNT)" = "2" ] || fail "TRACK_COUNT != 2"
[ "$(val SAMPLE_COUNT)" = "240" ] || fail "SAMPLE_COUNT != 240"
[ "$(val KEYFRAME_COUNT)" -ge 20 ] || fail "KEYFRAME_COUNT < 20"
[ "$(val WRAP_EVENTS)" -ge 1 ] || fail "WRAP_EVENTS < 1"
"$BIN" inspect --input "$TMP/sample.mp4" --format json >/dev/null || fail "inspect json exit"

# --- 3. seek ----------------------------------------------------------------
SEEK="$("$BIN" seek --input "$TMP/sample.mp4" --track 1 --time 12.345 \
  --before 2 --after 3 --hash sha256)" || fail "seek exit"
for k in LANDING_SAMPLE LANDING_PTS FRAME_TYPE FILE_OFFSET SAMPLE_SIZE; do
  grep -q "^$k: " <<<"$SEEK" || fail "seek missing $k"
done
grep -q "^FRAME_TYPE: keyframe$" <<<"$SEEK" || fail "landing not keyframe"
grep -Eq "^SHA256: [0-9a-f]{64}$" <(grep -o '^SAMPLE\[[0-9]\].SHA256: .*' <<<"$SEEK" | sed 's/^SAMPLE\[[0-9]\].//') \
  || grep -Eq "SHA256: [0-9a-f]{64}$" <<<"$SEEK" || fail "no 64-hex SHA256"
[ "$(grep -c 'SHA256: ' <<<"$SEEK")" = "6" ] || fail "window hash count != 6"

# --- 4. export ----------------------------------------------------------------
"$BIN" export --input "$TMP/sample.mp4" --out "$TMP/report.jsonl" >/dev/null || fail "export exit"
[ -s "$TMP/report.jsonl" ] || fail "report.jsonl missing"
head -1 "$TMP/report.jsonl" | grep -q '"type":"summary"' || fail "jsonl summary line"
# every line is valid JSON
while IFS= read -r line; do
  python3 -c 'import json,sys; json.loads(sys.argv[1])' "$line" || fail "jsonl invalid line"
done < "$TMP/report.jsonl"

# --- 5. exit codes on fault paths ---------------------------------------------
expect_code() { # expect_code <want> <cmd...>
  local want="$1"; shift
  set +e; "$@" >/dev/null 2>&1; local got=$?; set -e
  [ "$got" = "$want" ] || fail "exit code $got != $want: $*"
}
"$BIN" fixture --out "$TMP/trunc.mp4"   --inject truncate   --seed 42 >/dev/null
"$BIN" fixture --out "$TMP/badoff.mp4"  --inject bad-offset --seed 42 >/dev/null
"$BIN" fixture --out "$TMP/nostss.mp4"  --inject no-stss    --seed 42 >/dev/null
"$BIN" fixture --out "$TMP/badelst.mp4" --inject bad-elst --edit-list --seed 42 >/dev/null
expect_code 3 "$BIN" inspect --input "$TMP/trunc.mp4"
expect_code 3 "$BIN" inspect --input "$TMP/badoff.mp4"
expect_code 3 "$BIN" inspect --input "$TMP/badelst.mp4"
expect_code 4 "$BIN" seek --input "$TMP/nostss.mp4" --track 1 --time 5 --hash sha256
expect_code 4 "$BIN" seek --input "$TMP/sample.mp4" --track 1 --time 0.2
expect_code 4 "$BIN" seek --input "$TMP/sample.mp4" --track 1 --time 99
expect_code 2 "$BIN" inspect --input "$TMP/does-not-exist.mp4"
expect_code 2 "$BIN" inspect

# --- 6. unit + golden tests ----------------------------------------------------
go test ./... >/dev/null || fail "go test ./..."

echo "ALL CHECKS PASSED"
