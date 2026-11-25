#!/usr/bin/env bash
# 省级媒资馆恢复管线验收脚本：构建、测试、正常/损坏/恢复路径和退出码。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CLEAN="${CLEAN_OUT:-/tmp/media-clean}"
DAMAGE="${DAMAGE_OUT:-/tmp/media-damage}"
RESUME="${RESUME_OUT:-/tmp/media-resume}"
OTHER="${OTHER_OUT:-/tmp/media-other}"
FATAL="${FATAL_OUT:-/tmp/media-fatal}"

echo "==> go mod download / tidy / build / test"
go mod download
go mod tidy
go build ./...
go test ./...

rm -rf "$CLEAN" "$DAMAGE" "$RESUME" "$OTHER" "$FATAL"

assert_grep() {
  local file="$1" pattern="$2"
  if ! grep -Eq "$pattern" "$file"; then
    echo "missing pattern '$pattern' in $file" >&2
    cat "$file" >&2
    exit 1
  fi
}

run_expect() {
  local expected="$1" log="$2"
  shift 2
  set +e
  go run ./cmd/archivecheck "$@" 2>&1 | tee "$log"
  local rc="${PIPESTATUS[0]}"
  set -e
  if [[ "$rc" != "$expected" ]]; then
    echo "expected exit $expected, got $rc" >&2
    exit 1
  fi
}

echo "==> clean path"
run_expect 0 /tmp/archivecheck-clean.log \
  --scenario clean --seed 41 --events 12000 --out "$CLEAN"
for kv in \
  '^tracks=3$' \
  '^events=12000$' \
  '^expected_packet=6001$' \
  '^located_packet=6001$' \
  '^offset_error=0$' \
  '^late_replays=[1-9][0-9]*$' \
  '^frames_sha256=[0-9a-f]{64}$' \
  '^manifest_sha256=[0-9a-f]{64}$'; do
  assert_grep /tmp/archivecheck-clean.log "$kv"
done

echo "==> damage localization"
run_expect 0 /tmp/archivecheck-damage.log \
  --scenario damage --seed 41 --events 12000 --damage-at 6001 --out "$DAMAGE"
for kv in \
  '^expected_packet=6001$' \
  '^located_packet=6001$' \
  '^offset_error=0$' \
  '^frames_sha256=[0-9a-f]{64}$' \
  '^manifest_sha256=[0-9a-f]{64}$'; do
  assert_grep /tmp/archivecheck-damage.log "$kv"
done

echo "==> interrupt and resume"
run_expect 75 /tmp/archivecheck-interrupt.log \
  --scenario recovery --seed 41 --events 12000 --interrupt-after 4300 --out "$RESUME"
assert_grep /tmp/archivecheck-interrupt.log '^checkpoint_events=4300$'

run_expect 0 /tmp/archivecheck-resume.log \
  --scenario recovery --seed 41 --events 12000 --resume --out "$RESUME" --compare "$CLEAN"
for kv in \
  '^tracks=3$' \
  '^events=12000$' \
  '^expected_packet=6001$' \
  '^located_packet=6001$' \
  '^offset_error=0$' \
  '^checkpoint_events=4300$' \
  '^resumed_events=7700$' \
  '^late_replays=[1-9][0-9]*$' \
  '^frames_sha256=[0-9a-f]{64}$' \
  '^manifest_sha256=[0-9a-f]{64}$' \
  '^compare=match$'; do
  assert_grep /tmp/archivecheck-resume.log "$kv"
done

recovery_ms="$(sed -n 's/^recovery_ms=//p' /tmp/archivecheck-resume.log | head -1)"
if [[ -z "$recovery_ms" || "$recovery_ms" -ge 3000 ]]; then
  echo "recovery_ms must be present and <3000, got '$recovery_ms'" >&2
  exit 1
fi

clean_frames="$(sed -n 's/^frames_sha256=//p' /tmp/archivecheck-clean.log | head -1)"
resume_frames="$(sed -n 's/^frames_sha256=//p' /tmp/archivecheck-resume.log | head -1)"
clean_manifest="$(sed -n 's/^manifest_sha256=//p' /tmp/archivecheck-clean.log | head -1)"
resume_manifest="$(sed -n 's/^manifest_sha256=//p' /tmp/archivecheck-resume.log | head -1)"
if [[ "$clean_frames" != "$resume_frames" || "$clean_manifest" != "$resume_manifest" ]]; then
  echo "clean and recovery hashes differ" >&2
  exit 1
fi

echo "==> exit codes 2/3/4"
run_expect 2 /tmp/archivecheck-usage.log --scenario clean
run_expect 3 /tmp/archivecheck-fatal.log \
  --scenario fatal --seed 41 --events 12000 --damage-at 6001 --out "$FATAL"
run_expect 4 /tmp/archivecheck-mismatch.log \
  --scenario clean --seed 42 --events 12000 --damage-at 6001 \
  --out "$OTHER" --compare "$CLEAN"

echo "ACCEPTANCE OK: recovery_ms=${recovery_ms}ms frames=${resume_frames}"
