#!/usr/bin/env bash
# Reproduces the mediad acceptance flow locally:
#   module checks -> build -> race tests -> clean/damaged/cancel+resume demos.
# Exit codes asserted: clean 0, damaged 2, cancel 3, resume 0.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mediad-accept.XXXXXX")"
BIN="$WORK/mediad"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "ACCEPTANCE_FAIL: $*" >&2; exit 1; }

echo "==> go module / build / tests"
go mod tidy || fail "go mod tidy"
go mod download || fail "go mod download"
go mod verify || fail "go mod verify"
go build -o "$BIN" ./cmd/mediad || fail "build"
go test -race ./... || fail "race tests"

contains() { # file token...
  local file="$1"; shift
  local tok
  for tok in "$@"; do
    grep -qF -- "$tok" "$file" || { echo "---- $file"; cat "$file"; fail "missing token: $tok"; }
  done
}

echo "==> clean"
"$BIN" demo --case clean --seed 23 --workers 4 --queue 8 --out "$WORK/clean" \
  | tee "$WORK/clean.out"
rc=${PIPESTATUS[0]}
[ "$rc" -eq 0 ] || fail "clean exit=$rc want 0"
contains "$WORK/clean.out" \
  "fragments=24" "roundtrip=24/24" "keyframe_hit=24/24" \
  "sha256=24/24" "representations=3" "aligned=8"

echo "==> damaged"
set +e
"$BIN" demo --case damaged --seed 23 --out "$WORK/damaged" > "$WORK/damaged.out" 2>&1
rc=$?
set -e
cat "$WORK/damaged.out"
[ "$rc" -eq 2 ] || fail "damaged exit=$rc want 2"
contains "$WORK/damaged.out" "quarantined=2" "roundtrip=22/22" "sha256=24/24"

echo "==> cancel + resume"
set +e
"$BIN" demo --case cancel --checkpoint "$WORK/recovery" > "$WORK/cancel.out" 2>&1
rc=$?
set -e
cat "$WORK/cancel.out"
[ "$rc" -eq 3 ] || fail "cancel exit=$rc want 3"

"$BIN" resume --checkpoint "$WORK/recovery" > "$WORK/resume.out"
cat "$WORK/resume.out"
contains "$WORK/resume.out" "resumed=1" "duplicate_emit=0" "result_hash_equal=true"

echo "ACCEPTANCE_OK ($WORK)"
