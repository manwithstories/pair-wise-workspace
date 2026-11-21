#!/usr/bin/env bash
# Runs the documented acceptance flow and asserts the required output keys.
# Usage: scripts/acceptance.sh [work-dir]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${1:-$(mktemp -d)/pairflow}"
FIXTURE="$WORK/fixture"
OUT="$WORK/out"
mkdir -p "$WORK"

cd "$ROOT"
echo "== go mod tidy + go test =="
go mod tidy
go test ./...

echo "== fixture -> $FIXTURE =="
go run ./cmd/fixture -out "$FIXTURE" -frames 1200 -corrupt-every 137

echo "== mediad -> $OUT =="
go run ./cmd/mediad -input "$FIXTURE" -queue 8 -slow-consumer 5ms \
  -bitrates 360p,720p,1080p -out "$OUT" | tee "$WORK/acceptance.txt"

echo "== assertions =="
grep -qE '^roundtrip_rate=0\.99[0-9]+$' "$WORK/acceptance.txt"
grep -q '^keyframe_hit_rate=1\.000$' "$WORK/acceptance.txt"
grep -qE '^segment_sha256_checked=[0-9]+$' "$WORK/acceptance.txt"
grep -q '^alignment_error_ticks=0$' "$WORK/acceptance.txt"
grep -qE '^peak_inflight_frames=([0-9]|1[0-9]|2[0-4])$' "$WORK/acceptance.txt"
grep -qE '^corrupt_records=[0-9]+$' "$WORK/acceptance.txt"
grep -qE '^first_error_offset=[0-9]+$' "$WORK/acceptance.txt"

echo "== boundary self-check =="
go run ./cmd/mediad -self-check-boundaries -input "$FIXTURE" | tee "$WORK/boundaries.txt"
grep -q '^truncated=1 checksum_mismatch=1 timestamp_wrap=1 duplicate=1$' "$WORK/boundaries.txt"

echo "acceptance: OK ($WORK)"
