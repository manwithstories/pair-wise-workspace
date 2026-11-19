#!/bin/sh
# Demo: interrupt a run with SIGINT, then resume with the same arguments.
#
#   sh scripts/demo.sh [fixture-dir] [kill-after-seconds]
#
# Requires a built binary at ./tmp/mediq (go build -o tmp/mediq ./cmd/mediq).
set -u
dir="${1:-./tmp/fixtures}"
delay="${2:-0.4}"
bin="./tmp/mediq"
input="$dir/damaged.seq"
out="./tmp/demo.mp4"
ckpt="./tmp/demo.ckpt"

rm -f "$out" "$out.part" "$ckpt" "$ckpt.part" "$ckpt.prev" "$ckpt.chunks"
rm -rf "$out.tables"

echo "== interrupted run"
"$bin" process --input "$input" --output "$out" --checkpoint "$ckpt" \
  --chunk-frames 256 --queue 8 --slow-ms 2 > ./tmp/demo.run1.log 2>&1 &
pid=$!
sleep "$delay"
kill -INT "$pid" 2>/dev/null || true
wait "$pid"
echo "exit=$?"
cat ./tmp/demo.run1.log

echo
echo "== resumed run (identical arguments)"
"$bin" process --input "$input" --output "$out" --checkpoint "$ckpt" \
  --chunk-frames 256 --queue 8
echo "exit=$?"

echo
echo "== verification"
"$bin" verify --input "$out" --source "$input"
