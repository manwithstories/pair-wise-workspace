#!/usr/bin/env bash
# Field QC end-to-end acceptance gate (macOS / Linux, no extra deps).
#
# Runs the exact sequence from the task, then checks:
#   * one JSON line per case (6)
#   * sync_p95_ms <= 20, p95_window_ms <= 250
#   * rtp-burst loss_recovery_pct >= 70
#   * every damaged case locates >= 1 span, normal locates 0
#   * repeated runs yield identical output apart from live window timing
#   * exit codes 0/1/2/3 match the contract
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "== go version (want go1.25.6) =="
go version

echo "== go mod download =="
go mod download

echo "== go build ./... =="
go build ./...

echo "== go test ./... =="
go test ./...

BIN_DIR="$(mktemp -d)"
trap 'rm -rf "$BIN_DIR"' EXIT
BIN="$BIN_DIR/fieldqc"
go build -o "$BIN" ./cmd/fieldqc

ARGS=(demo --case all --seed 41 --hours 2 --tracks 4 --loss 0.03 --window 10s)

echo "== acceptance command =="
"$BIN" "${ARGS[@]}" | tee "$BIN_DIR/run1.jsonl"

# Numeric / localization gates in portable awk (BSD awk compatible).
awk '
function num(line, key, re, s) {
  re = "\"" key "\":-?[0-9.]+";
  if (match(line, re)) { s = substr(line, RSTART, RLENGTH); sub(".*:", "", s); return s + 0 }
  return -1
}
{
  n++
  if (num($0, "sync_p95_ms") > 20)      { print "SYNC FAIL: " $0; bad=1 }
  if (num($0, "p95_window_ms") > 250)    { print "WINDOW FAIL: " $0; bad=1 }
  dmg = num($0, "damaged_spans")
  if (index($0, "\"case\":\"normal\"") > 0) {
    if (dmg != 0) { print "NORMAL SPAN FAIL: " $0; bad=1 }
  } else if (dmg < 1) { print "DAMAGE LOCATE FAIL: " $0; bad=1 }
  if (index($0, "\"case\":\"rtp-burst\"") > 0 && num($0, "loss_recovery_pct") < 70) {
    print "RECOVERY FAIL: " $0; bad=1
  }
}
END { if (n != 6) { print "expected 6 case lines, got " n; bad=1 }; exit bad }
' "$BIN_DIR/run1.jsonl"
echo "metric gates: OK"

echo "== digest / summary repeatability =="
strip_timing() { sed -E 's/,"p95_window_ms":[0-9.]+//g' "$1"; }
"$BIN" "${ARGS[@]}" > "$BIN_DIR/run2.jsonl"
"$BIN" "${ARGS[@]}" > "$BIN_DIR/run3.jsonl"
strip_timing "$BIN_DIR/run1.jsonl" > "$BIN_DIR/a"
strip_timing "$BIN_DIR/run2.jsonl" > "$BIN_DIR/b"
strip_timing "$BIN_DIR/run3.jsonl" > "$BIN_DIR/c"
if ! cmp -s "$BIN_DIR/a" "$BIN_DIR/b" || ! cmp -s "$BIN_DIR/a" "$BIN_DIR/c"; then
  echo "DETERMINISM FAIL: repeated summaries differ" >&2
  diff -u "$BIN_DIR/a" "$BIN_DIR/b" >&2 || true
  exit 1
fi
echo "repeatable summary: OK"

echo "== exit code contract =="
check_code() { # expected, description, command...
  want="$1"; desc="$2"; shift 2
  set +e; "$@" >/dev/null 2>&1; got=$?; set -e
  if [ "$got" -ne "$want" ]; then
    echo "EXIT CODE FAIL [$desc]: got $got want $want" >&2
    exit 1
  fi
  echo "  $desc -> $got"
}
set -e
check_code 0 "all scenarios as expected" "$BIN" "${ARGS[@]}"
check_code 0 "normal clean" "$BIN" demo --case normal --hours 1
check_code 2 "single damaged case located" "$BIN" demo --case truncated-box --hours 1
check_code 1 "unknown parameter/case" "$BIN" demo --case no-such-case
check_code 3 "cancellation" "$BIN" cancel

echo
echo "ACCEPTANCE PASSED"
