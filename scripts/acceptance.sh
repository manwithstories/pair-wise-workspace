#!/usr/bin/env bash
# Acceptance script for the mediaroute demo.
#
# Runs the documented commands from the task statement and checks the
# published thresholds. Exits non-zero if any check fails.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== go mod tidy =="
go mod tidy
echo "== go build =="
go build ./...
echo "== go test =="
go test ./...

echo "== demo: seed 42, loss burst 12, tail cut 4096 =="
json=$(go run ./cmd/mediaroute demo --seed 42 --loss-burst 12 --truncate-tail 4096 --json)
printf '%s\n' "$json"

echo "== demo: repeat 2 (stability) =="
repeat_json=$(go run ./cmd/mediaroute demo --seed 42 --repeat 2 --json)
printf '%s\n' "$repeat_json"

python3 - "$json" "$repeat_json" <<'PY'
import json
import sys

main = json.loads(sys.argv[1])
repeat = json.loads(sys.argv[2])
failures = []

def check(name, ok, detail):
    print(("PASS " if ok else "FAIL ") + name + ": " + detail)
    if not ok:
        failures.append(name)

check("sync_error_ms_p95", main["sync_error_ms_p95"] <= 20, f'{main["sync_error_ms_p95"]} <= 20')
check("packet_recovery_rate", main["packet_recovery_rate"] >= 0.95, f'{main["packet_recovery_rate"]} >= 0.95')
check("latency_ms_p95", main["latency_ms_p95"] <= 250, f'{main["latency_ms_p95"]} <= 250')
check("indexed_samples", main["indexed_samples"] > 0, str(main["indexed_samples"]))
check("corrupt_ranges", "corrupt_ranges" in main, str(main["corrupt_ranges"]))
check("index_rebuilt key", "index_rebuilt" in main, str(main["index_rebuilt"]))
check("repeat stable", repeat["artifact_stable"] is True, str(repeat["artifact_stable"]))
check("repeat digests equal", len(set(repeat["artifact_digests"])) == 1, str(repeat["artifact_digests"]))
check("repeat index_rebuilt false", repeat["index_rebuilt"] is False, str(repeat["index_rebuilt"]))
check("exit code", main["exit_code"] == 0, str(main["exit_code"]))

if failures:
    print("FAILED: " + ", ".join(failures))
    sys.exit(1)
print("ALL CHECKS PASSED")
PY

echo "== exit code paths =="
# "go run" collapses every non-zero program status to 1, so build a binary to
# observe the documented 0/1/2/3 exit codes faithfully.
workdir=$(mktemp -d)
bin="$workdir/mediaroute"
trap 'rm -rf "$workdir"' EXIT
go build -o "$bin" ./cmd/mediaroute
"$bin" demo --seed 7 --dir "$workdir" --quiet >/dev/null 2>&1 || true
"$bin" scan --manifest "$workdir/normal-manifest.json" --dir "$workdir" >/dev/null 2>&1
echo "normal scan exit: $?"
set +e
"$bin" scan --manifest "$workdir/truncated-manifest.json" --dir "$workdir" >/dev/null 2>&1
trunc=$?
"$bin" demo --seed -1 >/dev/null 2>&1
badcfg=$?
set -e
echo "truncated scan exit: $trunc (expect 2)"
echo "bad config exit: $badcfg (expect 1)"
if [ "$trunc" -ne 2 ] || [ "$badcfg" -ne 1 ]; then
  echo "FAIL: exit codes"
  exit 1
fi
echo "ALL CHECKS PASSED"
