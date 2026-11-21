#!/bin/bash
# Acceptance: generate samples, simulate duplicate/reorder/burst-loss and
# varying bandwidth, run interrupt+resume, assert on the output fields.
# Exit 0 = all assertions pass, 1 = assertion failed, 2 = invalid args/input.
set -u

SEED="" PACKET_WINDOW="" MEMORY_MIB="" INTERRUPT_AFTER="" RESUME=0
while [ $# -gt 0 ]; do
  case "$1" in
    --seed) SEED="$2"; shift 2;;
    --packet-window) PACKET_WINDOW="$2"; shift 2;;
    --memory-mib) MEMORY_MIB="$2"; shift 2;;
    --interrupt-after) INTERRUPT_AFTER="$2"; shift 2;;
    --resume) RESUME=1; shift;;
    *) echo "unknown argument: $1" >&2; exit 2;;
  esac
done
for v in "$SEED" "$PACKET_WINDOW" "$MEMORY_MIB" "$INTERRUPT_AFTER"; do
  case "$v" in ''|*[!0-9]*) echo "invalid numeric argument" >&2; exit 2;; esac
done
[ "$RESUME" = 1 ] || { echo "--resume is required for acceptance" >&2; exit 2; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/build/relay_inspect"
WORK="$ROOT/build/acceptance"
[ -x "$BIN" ] || { echo "relay_inspect not built" >&2; exit 2; }
rm -rf "$WORK"; mkdir -p "$WORK"; cd "$WORK" || exit 2

fail() { echo "ASSERT FAIL: $1" >&2; exit 1; }
field() { grep "^$2=" "$1" | head -1 | cut -d= -f2; }

# --- generate samples -----------------------------------------------------
ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i testsrc2=size=320x240:rate=25 -t 6 \
  -c:v libx264 -g 25 -bf 0 -pix_fmt yuv420p sample.mp4 || exit 2

"$BIN" damage-media --in sample.mp4 --out damaged.mp4 --at-ms 3000 \
  --expected-out expected_media.txt || exit 2

"$BIN" gen-packets --seed "$SEED" --count 1500 --clean --out clean.csv || exit 2
"$BIN" gen-packets --seed "$SEED" --count 1500 \
  --burst-loss-at-ms 30000 --burst-loss-count 10 \
  --expected-out expected_transport.txt --out damaged.csv || exit 2
"$BIN" gen-captions --format srt --out clean.srt || exit 2
"$BIN" gen-captions --format srt --with-anomalies --out bad.srt || exit 2
cat expected_media.txt expected_transport.txt > expected_all.txt

# --- 1. normal sample: zero damaged ranges --------------------------------
"$BIN" analyze --packets clean.csv --media sample.mp4 --captions clean.srt \
  --packet-window "$PACKET_WINDOW" --memory-mib "$MEMORY_MIB" \
  --checkpoint ckpt_normal > normal.out || fail "normal analyze run"
[ "$(field normal.out damaged_ranges)" = "0" ] || fail "normal sample must have 0 damaged ranges"

# --- 2. damaged sample: interrupt, then resume ----------------------------
"$BIN" analyze --packets damaged.csv --media damaged.mp4 --captions bad.srt \
  --expected-damage expected_all.txt \
  --packet-window "$PACKET_WINDOW" --memory-mib "$MEMORY_MIB" \
  --interrupt-after "$INTERRUPT_AFTER" --checkpoint ckpt1 > run1.out
rc=$?
[ $rc = 3 ] || fail "first run should be interrupted (exit 3), got $rc"

"$BIN" analyze --packets damaged.csv --media damaged.mp4 --captions bad.srt \
  --expected-damage expected_all.txt \
  --packet-window "$PACKET_WINDOW" --memory-mib "$MEMORY_MIB" \
  --resume --checkpoint ckpt1 > run2.out || fail "resume run"

# --- 3. repeat the whole cycle: identical checksum ------------------------
"$BIN" analyze --packets damaged.csv --media damaged.mp4 --captions bad.srt \
  --expected-damage expected_all.txt \
  --packet-window "$PACKET_WINDOW" --memory-mib "$MEMORY_MIB" \
  --interrupt-after "$INTERRUPT_AFTER" --checkpoint ckpt2 > run3.out
[ $? = 3 ] || fail "repeat run should be interrupted"
"$BIN" analyze --packets damaged.csv --media damaged.mp4 --captions bad.srt \
  --expected-damage expected_all.txt \
  --packet-window "$PACKET_WINDOW" --memory-mib "$MEMORY_MIB" \
  --resume --checkpoint ckpt2 > run4.out || fail "repeat resume run"

SHA_A="$(field run2.out output_sha256)"
SHA_B="$(field run4.out output_sha256)"
[ -n "$SHA_A" ] && [ "$SHA_A" = "$SHA_B" ] || fail "resume checksums differ: $SHA_A vs $SHA_B"

# --- 4. assertions on the required fields ---------------------------------
BERR="$(field run2.out boundary_error_ms)"
[ -n "$BERR" ] && [ "$BERR" -le 40 ] || fail "boundary_error_ms=$BERR exceeds 40 ms"
PEAK="$(field run2.out peak_memory_mib)"
awk -v p="$PEAK" -v m="$MEMORY_MIB" 'BEGIN{exit !(p+0 <= m+0)}' \
  || fail "peak_memory_mib=$PEAK exceeds limit $MEMORY_MIB"
[ "$(field run2.out burst_losses)" -ge 1 ] || fail "burst loss not detected"
[ "$(field run2.out duplicate_packets)" -ge 1 ] || fail "duplicates not detected"
[ "$(field run2.out reordered_packets)" -ge 1 ] || fail "reorder not detected"

# --- report ---------------------------------------------------------------
for f in damaged_ranges boundary_error_ms resume_ms output_sha256 \
         peak_memory_mib duplicate_packets reordered_packets burst_losses; do
  echo "$f=$(field run2.out "$f")"
done
echo "acceptance: PASS"
exit 0
