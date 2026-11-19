#!/bin/sh
# Demo: interrupt a run with SIGINT, then resume with the same arguments.
#
#   sh scripts/demo.sh [fixture-dir] [extra-delay-seconds]
#
# Requires a built binary at ./tmp/mediq (go build -o tmp/mediq ./cmd/mediq).
# The interrupt is delivered as soon as the first chunk has been committed, so
# the resumed run always has work to recover.
set -u
dir="${1:-./tmp/fixtures}"
extra="${2:-0.2}"
bin="./tmp/mediq"
input="$dir/damaged.seq"
out="./tmp/demo.mp4"
ckpt="./tmp/demo.ckpt"

rm -f "$out" "$out.part" "$ckpt" "$ckpt.part" "$ckpt.prev" "$ckpt.chunks"
rm -rf "$out.tables"

echo "== interrupted run"
"$bin" process --input "$input" --output "$out" --checkpoint "$ckpt" \
  --chunk-frames 64 --queue 8 --slow-ms 1 > ./tmp/demo.run1.log 2>&1 &
pid=$!
# Wait for the first committed checkpoint (bounded to 10 s).
i=0
while [ ! -f "$ckpt" ] && [ "$i" -lt 1000 ]; do
  sleep 0.01
  i=$((i + 1))
done
sleep "$extra"
kill -INT "$pid" 2>/dev/null || true
wait "$pid"
echo "exit=$?"
cat ./tmp/demo.run1.log

echo
echo "== resumed run (same job, without the artificial slow-down)"
"$bin" process --input "$input" --output "$out" --checkpoint "$ckpt" \
  --chunk-frames 64 --queue 8
echo "exit=$?"

echo
echo "== verification"
"$bin" verify --input "$out" --source "$input"
