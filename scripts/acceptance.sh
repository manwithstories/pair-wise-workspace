#!/usr/bin/env bash
# Local acceptance suite for mediq. No external services are used.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GOVER="${GOVER:-$(go env GOPATH)/bin/go1.25.6}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/mediq-acceptance.XXXXXX")"
echo "work=$WORK"

"$GOVER" mod tidy
"$GOVER" build ./...
"$GOVER" test ./...

"$GOVER" run ./cmd/mediq demo --case clean --out "$WORK/clean"
"$GOVER" run ./cmd/mediq demo --case damaged --out "$WORK/damaged"

# The go command documents that `go run` does not preserve the compiled
# program's exit status, so verify the contractual 75 with a real binary.
"$GOVER" build -o "$WORK/mediq" ./cmd/mediq
set +e
"$WORK/mediq" demo --case recovery --crash-after checkpoint --out "$WORK/recovery"
crash_rc=$?
set -e
echo "crash_exit_code=$crash_rc"
test "$crash_rc" -eq 75

"$GOVER" run ./cmd/mediq demo --case recovery --resume --out "$WORK/recovery"
hash1="$(shasum -a 256 "$WORK/recovery/mediq-delivery.mp4" | awk '{print $1}')"

"$GOVER" run ./cmd/mediq demo --case recovery --resume --out "$WORK/recovery" >/dev/null
hash2="$(shasum -a 256 "$WORK/recovery/mediq-delivery.mp4" | awk '{print $1}')"
test "$hash1" = "$hash2"

"$WORK/mediq" demo --case recovery --out "$WORK/recovery-normal" >/dev/null
hash3="$(shasum -a 256 "$WORK/recovery-normal/mediq-delivery.mp4" | awk '{print $1}')"
test "$hash1" = "$hash3"

echo "product_sha256=$hash1"
echo "acceptance=PASS"
