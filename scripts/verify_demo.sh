#!/usr/bin/env bash
# Run the local acceptance path and assert repeated-run hash determinism.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

go mod tidy
go build ./...
go test ./...

args=(demo --seed 17 --burst-loss 7 --bandwidth-kbps 900,280,1200)
run1="$(mktemp -t medialab-run1.XXXXXX)"
run2="$(mktemp -t medialab-run2.XXXXXX)"
hash1="$(mktemp -t medialab-hash1.XXXXXX)"
hash2="$(mktemp -t medialab-hash2.XXXXXX)"
bin="$(mktemp -t medialab-bin.XXXXXX)"
inspect_out="$(mktemp -t medialab-inspect.XXXXXX)"
trap 'rm -f "$run1" "$run2" "$hash1" "$hash2" "$bin" "$inspect_out"' EXIT

go build -o "$bin" ./cmd/medialab
"$bin" "${args[@]}" >"$run1"
"$bin" "${args[@]}" >"$run2"

diff -u "$run1" "$run2"
grep '^segment_sha256_[0-9]' "$run1" >"$hash1"
grep '^segment_sha256_[0-9]' "$run2" >"$hash2"
diff -u "$hash1" "$hash2"

for expected in \
  '^roundtrip_consistency=100\.00%$' \
  '^keyframe_index_hit=100\.00%$' \
  '^segment_sha256=12/12$' \
  '^duplicate_dropped=[0-9][0-9]*$' \
  '^reordered_packets=[0-9][0-9]*$' \
  '^burst_recovered=7/7$' \
  '^av_skew_ms=[0-9-][0-9]*$' \
  '^first_error_offset=[0-9-][0-9]*$'; do
  grep -Eq "$expected" "$run1"
done

# Invalid parameters must map to exit code 2 from the compiled adapter.
set +e
"$bin" demo --burst-loss 99 >/dev/null 2>&1
usage_code=$?
set -e
if [[ "$usage_code" -ne 2 ]]; then
  echo "expected exit 2 for invalid burst loss, got $usage_code" >&2
  exit 1
fi

# A generated truncated KLV fixture must be safely rejected with exit code 3.
./scripts/gen_fixtures.sh fixtures/generated >/dev/null
set +e
"$bin" inspect --file fixtures/generated/truncated_klv.mxf >"$inspect_out" 2>&1
reject_code=$?
set -e
if [[ "$reject_code" -ne 3 ]]; then
  echo "expected exit 3 for truncated KLV, got $reject_code" >&2
  cat "$inspect_out" >&2
  exit 1
fi
grep -q '^safe_reject' "$inspect_out"

set +e
"$bin" inspect --file fixtures/generated/crop_out_of_bounds.mp4 >"$inspect_out" 2>&1
crop_code=$?
set -e
if [[ "$crop_code" -ne 3 ]]; then
  echo "expected exit 3 for out-of-bounds crop, got $crop_code" >&2
  cat "$inspect_out" >&2
  exit 1
fi
grep -q 'code=crop_out_of_bounds' "$inspect_out"

echo "verify_demo: OK"
