#!/bin/sh
# Runs both acceptance commands and checks the required report lines.
# Usage: scripts/demo.sh [out-dir]
set -eu
cd "$(dirname "$0")/.."
BIN="${1:-./bin/mediacore}"
mkdir -p "$(dirname "$BIN")"
go build -o "$BIN" ./cmd/mediacore

fail() { echo "demo: $*" >&2; exit 1; }

echo "== normal run =="
"$BIN" verify --self-generate --segments 12 --slow-consumer 40ms --memory-limit 64MiB | tee /tmp/mediacore-normal.txt
grep -q 'roundtrip_rate=100.00%' /tmp/mediacore-normal.txt || fail "roundtrip_rate"
grep -q 'keyframe_hit_rate=100.00%' /tmp/mediacore-normal.txt || fail "keyframe_hit_rate"
grep -q 'segment_sha256_match=12/12' /tmp/mediacore-normal.txt || fail "segment match"
awk -F'[ =]' '/queue_peak_bytes=/{if ($2+0 > 67108864) exit 1}' /tmp/mediacore-normal.txt || fail "queue peak"

echo "== fault + recovery run =="
set +e
"$BIN" verify --self-generate --fault truncate --simulate-crash-after 5 --resume > /tmp/mediacore-fault.txt
rc=$?
set -e
cat /tmp/mediacore-fault.txt
[ "$rc" -eq 2 ] || fail "expected exit 2 for isolated corruption, got $rc"
grep -q 'index_invalidated=1' /tmp/mediacore-fault.txt || fail "index_invalidated"
grep -q 'recovered_segments=5' /tmp/mediacore-fault.txt || fail "recovered_segments"
grep -q 'quarantined kind=' /tmp/mediacore-fault.txt || fail "quarantine location"
echo "demo: ok"
