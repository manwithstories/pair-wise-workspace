#!/usr/bin/env bash
# Build the CLI and verify stable process exit codes 2, 3, 75 and 0.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/mediad-cli.XXXXXX")"
bin="$work/mediad"
go build -o "$bin" "$root/cmd/mediad"

set +e
"$bin" not-a-command >/dev/null 2>&1
usage_code=$?
set -e
[[ "$usage_code" -eq 2 ]]

python3 - "$work/bad.mdr" <<'PY'
import sys
with open(sys.argv[1], "wb") as f:
    f.write(b"MDR1" + b"\xff\xff\xff\xff")
PY
set +e
"$bin" scan --input "$work/bad.mdr" >/dev/null 2>&1
bad_code=$?
set -e
[[ "$bad_code" -eq 3 ]]

"$bin" generate-fixture --seed 29 --seconds 1 --out "$work/case" >/dev/null
run_args=(
  run
  --input "$work/case/input.mdr"
  --index "$work/case/index.json"
  --out "$work/final.mp4"
  --work "$work/txn"
)
set +e
"$bin" "${run_args[@]}" --inject-stage mp4 >/dev/null 2>&1
interrupt_code=$?
set -e
[[ "$interrupt_code" -eq 75 ]]
[[ -f "$work/txn/mediad.wal" ]]

"$bin" "${run_args[@]}" >/dev/null
[[ -f "$work/final.mp4" ]]
echo "exit_codes_ok usage=2 unrecoverable=3 interrupted=75 resume=0"
