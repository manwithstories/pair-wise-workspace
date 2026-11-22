#!/bin/sh
# Acceptance script for the archivecheck demo and verify paths.
# Run from the repository root:  sh scripts/acceptance.sh
set -eu

echo "== go mod tidy =="
go mod tidy

echo "== go build ./... =="
go build ./...

echo "== go test ./... =="
go test ./...

echo "== demo =="
go run ./cmd/archivecheck demo --seed 41 --loss-burst 3 --reorder 5 \
    --duplicate 2 --bandwidth-mbps 2,0.4,4 --interrupt-after index | tee /tmp/archivecheck-demo.txt

echo
echo "== verify =="
go run ./cmd/archivecheck verify --seed 41 | tee /tmp/archivecheck-verify.txt

echo
echo "== checking required keys =="
keys="target_bps actual_bps bitrate_delta_pct quality_psnr_db quality_samples
manifest_alignment_ms lost_packets duplicate_packets reordered_packets
timestamp_repairs index_entries rollback_count index_sha256"
missing=0
for k in $keys; do
    if ! grep -q "^${k}=" /tmp/archivecheck-demo.txt; then
        echo "MISSING: $k"
        missing=1
    fi
done
if [ "$missing" -ne 0 ]; then
    echo "acceptance: FAIL (missing keys)"
    exit 1
fi

if ! grep -q "index_digest_stable=true" /tmp/archivecheck-demo.txt; then
    echo "acceptance: FAIL (index digest changed across resume)"
    exit 1
fi
if ! grep -q "acceptance: PASS" /tmp/archivecheck-verify.txt; then
    echo "acceptance: FAIL (verify path)"
    exit 1
fi
echo "acceptance: PASS"
