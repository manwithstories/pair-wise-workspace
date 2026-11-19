#!/usr/bin/env bash
# Generate success and corruption sample sets for media-archive.
# Usage: scripts/make_samples.sh [output_dir]
set -euo pipefail

OUT="${1:-/tmp/archive-samples}"
BIN="$(cd "$(dirname "$0")/.." && pwd)/build/media-archive"

if [[ ! -x "$BIN" ]]; then
  echo "build media-archive first: cmake --build build" >&2
  exit 2
fi

mkdir -p "$OUT"
"$BIN" generate --seed 42 --out "$OUT" --codec h264
"$BIN" generate --seed 7  --out "$OUT/h265" --codec h265
"$BIN" generate --seed 99 --out "$OUT/av1"  --codec av1

# Corruption fixtures derived from the generated good samples.
mkdir -p "$OUT/corrupt"
cp "$OUT/stream.h264" "$OUT/corrupt/truncated.h264"
# Truncate to 60% of the original size.
full_size=$(stat -f%z "$OUT/corrupt/truncated.h264")
cut_size=$(( full_size * 6 / 10 ))
dd if="$OUT/corrupt/truncated.h264" of="$OUT/corrupt/truncated.tmp" \
   bs=1 count="$cut_size" 2>/dev/null
mv "$OUT/corrupt/truncated.tmp" "$OUT/corrupt/truncated.h264"

cp "$OUT/keyframes.idx" "$OUT/corrupt/keyframes-bad.idx"
# Flip bytes inside the last checkpoint block to force a rollback.
idx_size=$(stat -f%z "$OUT/corrupt/keyframes-bad.idx")
flip_off=$(( idx_size - 16 ))
printf '\xAA\xBB' | dd of="$OUT/corrupt/keyframes-bad.idx" bs=1 \
  seek="$flip_off" count=2 conv=notrunc 2>/dev/null

echo "samples written to $OUT"
