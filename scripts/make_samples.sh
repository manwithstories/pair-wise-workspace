#!/usr/bin/env bash
# Generate success and corruption sample sets for media-archive.
# Usage: scripts/make_samples.sh [output_dir]
set -euo pipefail

OUT="${1:-/tmp/archive-samples}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Use the first runnable local build directory. Stale builds left behind by a
# Homebrew FFmpeg upgrade may fail to load, so probe each candidate.
BIN=""
for candidate in "$ROOT/build-fresh/media-archive" \
                 "$ROOT/build-rel/media-archive" \
                 "$ROOT/build/media-archive"; do
  if [[ -x "$candidate" ]] && "$candidate" --help >/dev/null 2>&1; then
    BIN="$candidate"
    break
  fi
done

if [[ ! -x "$BIN" ]]; then
  echo "build media-archive first: cmake --build build" >&2
  exit 2
fi

mkdir -p "$OUT"
"$BIN" generate --seed 42 --out "$OUT" --codec h264
"$BIN" generate --seed 7  --out "$OUT/h265" --codec h265
"$BIN" generate --seed 99 --out "$OUT/av1"  --codec av1

# Produce the recovered keyframe index so the corruption fixture has a source.
"$BIN" recover --codec h264 \
  --input "$OUT/stream.h264" \
  --packets "$OUT/packets.csv" \
  --audio "$OUT/audio.pcm" \
  --index "$OUT/keyframes.idx" \
  --checkpoint "$OUT/checkpoint" \
  --report json >/dev/null

# Corruption fixtures derived from the generated good h264 sample.
mkdir -p "$OUT/corrupt"
cp "$OUT/stream.h264" "$OUT/corrupt/truncated.h264"
full_size=$(stat -f%z "$OUT/corrupt/truncated.h264")
cut_size=$(( full_size * 6 / 10 ))
dd if="$OUT/corrupt/truncated.h264" of="$OUT/corrupt/truncated.tmp" \
   bs=1 count="$cut_size" 2>/dev/null
mv "$OUT/corrupt/truncated.tmp" "$OUT/corrupt/truncated.h264"

if [[ -f "$OUT/keyframes.idx" ]]; then
  cp "$OUT/keyframes.idx" "$OUT/corrupt/keyframes-bad.idx"
  idx_size=$(stat -f%z "$OUT/corrupt/keyframes-bad.idx")
  flip_off=$(( idx_size - 16 ))
  printf '\xAA\xBB' | dd of="$OUT/corrupt/keyframes-bad.idx" bs=1 \
    seek="$flip_off" count=2 conv=notrunc 2>/dev/null
fi

echo "samples written to $OUT"
