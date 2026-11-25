#!/usr/bin/env bash
# Reproduce the terminal acceptance checks locally.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "== toolchain =="
go version

echo "== dependencies/build/tests =="
go mod download
go build ./...
go test ./...

PASS_OUT="${TMPDIR:-/tmp}/av-pass-$$"
RECOVER_OUT="${TMPDIR:-/tmp}/av-recover-$$"
INVALID_OUT="${TMPDIR:-/tmp}/av-invalid-$$"
trap 'rm -rf "$PASS_OUT" "$RECOVER_OUT" "$INVALID_OUT"' EXIT

echo "== normal demo =="
NORMAL="$(go run ./cmd/mediaproc demo \
  --seed 41 \
  --out "$PASS_OUT" \
  --max-memory 8388608 \
  --queue 4 \
  --slow-consumer 20ms)"
echo "$NORMAL"
for expected in \
  "roundtrip=100.00%" \
  "keyframe_hit=100.00%" \
  "segments=6" \
  "hash_ok=6/6" \
  "silence=2" \
  "dedup=1" \
  "peak_queue=4" \
  "peak_memory<=8388608"; do
  grep -Fq "$expected" <<<"$NORMAL"
done

echo "== truncated-block recovery =="
RECOVERED="$(go run ./cmd/mediaproc demo \
  --seed 41 \
  --out "$RECOVER_OUT" \
  --fault truncated-block \
  --recover \
  --max-memory 8388608 \
  --queue 4)"
echo "$RECOVERED"
grep -Eq "recoveries=1 first_error_offset=[0-9]+ segments=5 hash_ok=5/5" <<<"$RECOVERED"

echo "== invalid VINT recovery =="
go run ./cmd/mediaproc demo \
  --seed 41 \
  --out "$INVALID_OUT" \
  --fault invalid-vint \
  --recover \
  --queue 4 >/dev/null

echo "== full queue cancellation =="
QUEUE_CANCEL="$(go run ./cmd/mediaproc demo --fault queue-cancel --queue 4)"
echo "$QUEUE_CANCEL"
grep -Eq "queue_cancel=1 first_error_offset=[0-9]+ cleaned=5 peak_queue=4" <<<"$QUEUE_CANCEL"

echo "acceptance: ok"
