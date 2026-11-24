#!/usr/bin/env bash
# Reproduce the five terminal acceptance checks locally.
# All generated media and the compiled binary live under /tmp.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "==> go mod tidy / build / test"
go mod tidy
go build ./...
go test ./...

go build -o /tmp/bcast ./cmd/bcast

echo "==> clean"
rm -rf /tmp/bcast-clean
/tmp/bcast demo --case clean --out /tmp/bcast-clean | tee /tmp/bcast-clean.log
grep -q '^target_kbps=6000$' /tmp/bcast-clean.log
grep -q '^actual_kbps=6000.000$' /tmp/bcast-clean.log
grep -q '^sample_count=3600$' /tmp/bcast-clean.log
grep -q '^seek_ok=24/24$' /tmp/bcast-clean.log

echo "==> truncated"
rm -rf /tmp/bcast-bad
/tmp/bcast demo --case truncated --expect-error --out /tmp/bcast-bad | tee /tmp/bcast-bad.log
grep -q '^status=isolated$' /tmp/bcast-bad.log
grep -q '^first_error_byte=' /tmp/bcast-bad.log
grep -q '^rollback=1$' /tmp/bcast-bad.log

echo "==> drift/wrap/VFR with injected index interruption"
rm -rf /tmp/bcast-recover
set +e
/tmp/bcast demo --case drift-wrap-vfr --out /tmp/bcast-recover --fail-after index
rc=$?
set -e
if [[ $rc -ne 75 ]]; then
  echo "expected exit 75, got $rc" >&2
  exit 1
fi

echo "==> resume"
/tmp/bcast resume --out /tmp/bcast-recover | tee /tmp/bcast-resume1.log
grep -q '^recovered=1$' /tmp/bcast-resume1.log
grep -q '^committed=1$' /tmp/bcast-resume1.log
sha1="$(grep '^sha256=' /tmp/bcast-resume1.log)"

/tmp/bcast resume --out /tmp/bcast-recover | tee /tmp/bcast-resume2.log
sha2="$(grep '^sha256=' /tmp/bcast-resume2.log)"
test "$sha1" = "$sha2"
grep -q '^committed=1$' /tmp/bcast-resume2.log

echo "acceptance checks passed: $sha1"
