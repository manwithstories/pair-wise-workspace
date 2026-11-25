#!/usr/bin/env bash
# Reproduce the mediad acceptance contract locally without external services.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
GO="${GO:-go1.25.6}"

echo "==> go mod tidy"
"$GO" mod tidy
# Go 1.25 removes a toolchain directive identical to the go directive; restore
# the repository's pinned contract after tidying.
"$GO" mod edit -toolchain=go1.25.6

echo "==> go build ./..."
"$GO" build ./...

echo "==> go test ./..."
"$GO" test ./...

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mediad-acceptance.XXXXXX")"
CLEAN_OUT="$WORK/pf-clean"
BLOCKED_OUT="$WORK/pf-blocked"
DAMAGED_OUT="$WORK/pf-damaged"
mkdir -p "$CLEAN_OUT"

echo "==> clean demo (first run)"
"$GO" run ./cmd/mediad demo --scenario clean --seed 73 --out "$CLEAN_OUT" | tee "$WORK/clean-1.txt"

echo "==> clean demo (repeat against same directory)"
"$GO" run ./cmd/mediad demo --scenario clean --seed 73 --out "$CLEAN_OUT" | tee "$WORK/clean-2.txt"

for expected in \
  'roundtrip=100.00%' \
  'keyframe_hit=100.00%' \
  'segments=12' \
  'hash_match=12/12' \
  'duplicate=24' \
  'burst_loss=4' \
  'bandwidth_steps=5'; do
  grep -Fx "$expected" "$WORK/clean-1.txt" >/dev/null
done

grep '^segment_hash ' "$WORK/clean-1.txt" > "$WORK/hashes-1.txt"
grep '^segment_hash ' "$WORK/clean-2.txt" > "$WORK/hashes-2.txt"
diff -u "$WORK/hashes-1.txt" "$WORK/hashes-2.txt"
[[ "$(wc -l < "$WORK/hashes-1.txt" | tr -d ' ')" == "12" ]]

# `go run` wraps a non-zero binary status as its own status 1 plus "exit status".
# Build a direct binary to verify the specification's exact process exit codes.
BIN="$WORK/mediad"
"$GO" build -o "$BIN" ./cmd/mediad

set +e
"$BIN" demo --scenario damaged --seed 73 --out "$DAMAGED_OUT" >"$WORK/damaged.txt" 2>&1
DAMAGED_CODE=$?
"$BIN" demo --scenario missing-init --seed 73 --out "$BLOCKED_OUT" >"$WORK/blocked.txt" 2>&1
BLOCKED_CODE=$?
"$BIN" demo --scenario bogus --seed 73 --out "$WORK/bad-args" >"$WORK/bad-args.txt" 2>&1
ARGS_CODE=$?
set -e

[[ "$DAMAGED_CODE" -eq 2 ]]
[[ "$BLOCKED_CODE" -eq 3 ]]
[[ "$ARGS_CODE" -eq 4 ]]
grep -Fx 'first_blocked=repair:track-02:init-segment' "$WORK/blocked.txt" >/dev/null
for expected in \
  'roundtrip=100.00%' \
  'hash_match=12/12' \
  'duplicate=24' \
  'burst_loss=4' \
  'bandwidth_steps=5'; do
  grep -Fx "$expected" "$WORK/damaged.txt" >/dev/null
done

# Also exercise the exact go-run invocations from the task.
"$GO" run ./cmd/mediad demo --scenario damaged --seed 73 --out "$DAMAGED_OUT" >/dev/null 2>&1 || true
"$GO" run ./cmd/mediad demo --scenario missing-init --seed 73 --out "$BLOCKED_OUT" >/dev/null 2>&1 || true

echo "acceptance=PASS workdir=$WORK"
