#!/usr/bin/env bash
# End-to-end acceptance check for mediarig. All outputs stay in a private
# temporary directory; no media assets are fetched from external services.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

go mod download
go mod tidy
bin_dir="$(mktemp -d)"
trap 'rm -rf "$bin_dir"' EXIT
bin="$bin_dir/mediarig"
go build -o "$bin" ./cmd/mediarig
go test ./...

root="$(mktemp -d)"
echo "ACCEPTANCE_ROOT=$root"

"$bin" demo --root "$root/clean" --case clean \
  --rate 48000 --layout stereo --lufs -23 --target-kbps 1536

"$bin" demo --root "$root/damaged" --case damaged \
  --drop 5 --reorder 7 --wrap-seq --wrap-ts --color-conflict

set +e
boundary="$("$bin" demo --root "$root/boundary" --case truncated --truncate-last 2>&1)"
boundary_code=$?
set -e
echo "$boundary"
test "$boundary_code" -eq 2
grep -q 'error_stage=framing' <<<"$boundary"
grep -q 'packet_sequence=' <<<"$boundary"
grep -q 'committed=false' <<<"$boundary"
test ! -e "$root/boundary/output/master.mp4"
test ! -e "$root/boundary/report/manifest.json"

set +e
"$bin" run --root "$root/recover" --fail-after audio
run_code=$?
set -e
test "$run_code" -eq 3

"$bin" resume --root "$root/recover"
audit1="$("$bin" audit --root "$root/recover")"
audit2="$("$bin" audit --root "$root/recover")"
echo "$audit1"
test "$audit1" = "$audit2"
grep -q 'commit=true' <<<"$audit1"
grep -q 'manifest_skew_us=0.000' <<<"$audit1"

echo "acceptance: PASS"
