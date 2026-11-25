#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

go mod download
go build -o /tmp/mediad ./cmd/mediad
go test ./...

d="$(mktemp -d)"
echo "acceptance root: $d"

/tmp/mediad demo --out "$d/demo" --seed 41 --rate 48000 --layout stereo --lufs -18
/tmp/mediad verify --root "$d/demo" | tee "$d/verify.out"

for expected in \
  'roundtrip=1.000 (12/12)' \
  'keyframe_hit=1.000 (48/48)' \
  'segment_sha256=9' \
  'replay_match=1' \
  'output_sr=48000' \
  'channels=2' \
  'integrated_lufs=-18.0'
do
  grep -F "$expected" "$d/verify.out" >/dev/null
done

set +e
/tmp/mediad demo --out "$d/recover" --interrupt-after loudness >"$d/interrupt.out" 2>&1
interrupt_code=$?
set -e
if [[ "$interrupt_code" -ne 75 ]]; then
  cat "$d/interrupt.out"
  echo "expected exit 75, got $interrupt_code" >&2
  exit 1
fi

/tmp/mediad resume --root "$d/recover" | tee "$d/resume.out"
grep -F 'resume_match=1' "$d/resume.out" >/dev/null

set +e
/tmp/mediad demo --out "$d/bad" --damage truncated-mdat >"$d/bad.out" 2>&1
damage_code=$?
set -e
if [[ "$damage_code" -ne 1 ]]; then
  cat "$d/bad.out"
  echo "expected exit 1, got $damage_code" >&2
  exit 1
fi
grep -F 'error_code=E_BOX_TRUNCATED' "$d/bad.out" >/dev/null
grep -F 'first_box_offset=0' "$d/bad.out" >/dev/null

echo "ACCEPTANCE_OK $d"
