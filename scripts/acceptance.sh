#!/usr/bin/env bash
# Reproduce the archivectl acceptance checks locally.
# Archivectl itself exits 2 for a recovered archive; note that `go run`
# wraps a child's nonzero status as go tool exit status 1.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go version
go mod tidy
go build -o /tmp/archivectl ./cmd/archivectl
go test ./...

DEMO_ARGS=(demo --seed 41 --frames 196608 --channels 6
  --inject wrap,drift,vfr,bad-crc,truncate,missing-cue)

echo '--- go run acceptance command ---'
go run ./cmd/archivectl "${DEMO_ARGS[@]}" || true

echo '--- built binary metric/exit-code verification ---'
set +e
output="$(/tmp/archivectl "${DEMO_ARGS[@]}" --workdir demo-work-acceptance)"
binary_code=$?
set -e
printf '%s\n' "$output"

printf '%s\n' "$output" | grep -q 'roundtrip_rate=1.000'
printf '%s\n' "$output" | grep -Eq 'keyframe_hit_rate=(0\.9[89][0-9]*|1\.000)'
printf '%s\n' "$output" | grep -q 'sha256_segments=48'
printf '%s\n' "$output" | grep -q 'sha256_match=48/48'
[ "$binary_code" -eq 2 ]
echo "binary_exit_code=${binary_code}"
exit "$binary_code"
