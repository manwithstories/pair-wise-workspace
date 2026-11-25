#!/usr/bin/env bash
# Reproduce the MediaQC acceptance sequence locally with no external services.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

OUT="${1:-${TMPDIR:-/tmp}/mediqc-run}"
mkdir -p "$OUT"

go mod tidy
go mod download
go build ./...
go test ./...

go run ./cmd/mediqc run --seed 412 --suite all --out "$OUT" | tee "$OUT/run-summary.txt"

run_hash="$(sed -n 's/^manifest_sha256=//p' "$OUT/run-summary.txt" | tail -1)"
verify_output="$(go run ./cmd/mediqc verify --dir "$OUT")"
printf '%s\n' "$verify_output"
verify_hash="$(printf '%s\n' "$verify_output" | sed -n 's/^manifest_sha256=//p')"

test -n "$run_hash"
test "$run_hash" = "$verify_hash"

for token in \
  'cases=6' 'clean=1' 'repaired=4' 'rejected=1' \
  'cc_gaps=1' 'psi_switches=1' 'pcr_wraps=1' 'late_replays=1' \
  'sample_hash_match=100%' 'replay_sha256_equal=true'; do
  grep -F "$token" "$OUT/run-summary.txt" >/dev/null
done

ppm="$(sed -n 's/^bitrate_target_bps=[0-9]* bitrate_actual_bps=[0-9]* bitrate_delta_ppm=\(-\?[0-9]*\)$/\1/p' "$OUT/run-summary.txt")"
if [ "${ppm#-}" -gt 5000 ]; then
  echo "bitrate delta_ppm $ppm exceeds 5000" >&2
  exit 1
fi

echo "acceptance OK: manifest_sha256=$verify_hash"
