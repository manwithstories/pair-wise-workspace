#!/bin/sh
# Demonstrates interruption and resume: starts a slow process, sends SIGINT
# after a delay, then reruns with identical arguments and prints the result.
set -u
BIN="$1"; delay="${2:-0.6}"
rm -f tmp/big.mp4 tmp/big.mp4.part tmp/big.ckpt tmp/big.ckpt.prev
tmpdir="tmp/big.tables"
rm -rf "$tmpdir"

"$BIN" process --input ./tmp/fixtures/damaged.seq --output ./tmp/big.mp4 \
  --checkpoint ./tmp/big.ckpt --chunk-frames 256 --queue 8 --slow-ms 3 \
  > tmp/run1.log 2>&1 &
pid=$!
sleep "$delay"
kill -INT "$pid" 2>/dev/null || true
wait "$pid"
code=$?
echo "--- first run exit=$code"
cat tmp/run1.log
echo "--- second run"
"$BIN" process --input ./tmp/fixtures/damaged.seq --output ./tmp/big.mp4 \
  --checkpoint ./tmp/big.ckpt --chunk-frames 256 --queue 8
echo "--- second run exit=$?"
