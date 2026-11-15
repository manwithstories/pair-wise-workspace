#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j

./build/fixture_gen --out fixtures --frames 240 --slices 3 --interlaced-pair 2

./build/h264_audit scan fixtures/mixed_1080p.h264 > /tmp/h264_audit_mixed.txt
grep -q 'profile_idc=100 level_idc=41 chroma_format_idc=1' /tmp/h264_audit_mixed.txt
grep -q 'bit_depth_luma=8' /tmp/h264_audit_mixed.txt
grep -q '1920x1080' /tmp/h264_audit_mixed.txt
grep -q 'frames=240 I=8 P=176 B=56 slices_per_au_max=3' /tmp/h264_audit_mixed.txt
grep -q 'GOPs: count=8' /tmp/h264_audit_mixed.txt

./build/h264_audit scan fixtures/mixed_1080p.h264 --json out/report.json > /tmp/h264_audit_json.txt
grep -q '"sps"' out/report.json
grep -q '"pps"' out/report.json
grep -q '"frames"' out/report.json
grep -q '"gops"' out/report.json
grep -q '"warnings"' out/report.json
test "$(grep -o '"index":' out/report.json | wc -l | tr -d ' ')" = "8"

./build/h264_audit gops fixtures/interlaced_paff.h264 --recovery-point --min-gop 8 > /tmp/h264_audit_gops.txt
grep -q 'frames=96' /tmp/h264_audit_gops.txt
grep -q 'GOPs: count=12' /tmp/h264_audit_gops.txt

./build/h264_audit scan fixtures/interlaced_mbaff.h264 > /tmp/h264_audit_mbaff.txt
grep -q 'mb_adaptive_frame_field_flag=1' /tmp/h264_audit_mbaff.txt
grep -q 'frames=64' /tmp/h264_audit_mbaff.txt

set +e
./build/h264_audit scan fixtures/truncated.h264 > /tmp/h264_audit_truncated.txt 2>&1
truncated_status=$?
set -e
test "$truncated_status" = "2"
grep -q 'truncated slice' /tmp/h264_audit_truncated.txt

./build/h264_audit selftest
echo "all tests passed"
