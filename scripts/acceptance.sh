#!/usr/bin/env bash
# Local acceptance + demo driver for relaycheck.
#
# It runs the exact commands from the specification (including `go run`) and
# additionally builds a binary to assert the semantic exit codes 0/2/75/3,
# because the stock `go run` tool masks a non-zero child exit as shell code 1
# while still printing "exit status N".
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SEED_ARGS=(--seed 25 --target-rate 48000 --layout stereo --integrated-lufs -18
  --duplicate-every 17 --reorder-window 9 --burst-loss 6
  --bandwidth-kbps 12000,4000,9000)

pass=0
fail=0
ok()   { echo "  PASS: $1"; pass=$((pass+1)); }
bad()  { echo "  FAIL: $1"; fail=$((fail+1)); }

echo "== toolchain =="
go version
go mod download && ok "go mod download"
go mod tidy     && ok "go mod tidy"
go build ./...  && ok "go build ./..."
go test ./...   && ok "go test ./..."

mkdir -p bin
go build -o bin/relaycheck ./cmd/relaycheck || { echo "build binary failed"; exit 1; }

echo "== normal demo (go run) =="
go run ./cmd/relaycheck demo "${SEED_ARGS[@]}" --out ./.relay-ok > /tmp/relay_ok.txt 2>/tmp/relay_ok.err
[ $? -eq 0 ] && ok "normal exit 0" || bad "normal exit"
grep -q 'roundtrip_consistency=100.00%'        /tmp/relay_ok.txt && ok "roundtrip_consistency=100.00%" || bad "roundtrip"
grep -q 'keyframe_hit_rate=100.00%（120/120）'  /tmp/relay_ok.txt && ok "keyframe 120/120" || bad "keyframe"
grep -q 'segment_hash_match=100.00%（120 片段）' /tmp/relay_ok.txt && ok "segment_hash 120" || bad "segment hash"
n=$(grep -oE '[0-9a-f]{64}' /tmp/relay_ok.txt | wc -l | tr -d ' ')
[ "$n" = "120" ] && ok "120 SHA-256 tokens" || bad "hash token count=$n"

echo "== corrupt path =="
go run ./cmd/relaycheck demo --case truncated-mp4 --out ./.relay-bad > /tmp/relay_bad.txt 2>&1
grep -q 'parse_error' /tmp/relay_bad.txt && ok "prints parse_error" || bad "parse_error token"
grep -q 'track_id'    /tmp/relay_bad.txt && ok "prints track_id"    || bad "track_id token"
grep -q 'time_range'  /tmp/relay_bad.txt && ok "prints time_range"  || bad "time_range token"
[ ! -e ./.relay-bad/manifest.json ] && ok "no committed manifest" || bad "manifest exists"
./bin/relaycheck demo --case truncated-mp4 --out /tmp/relay-bad-bin >/dev/null 2>&1
[ $? -eq 2 ] && ok "binary exit code 2" || bad "binary exit code != 2"

echo "== crash / resume =="
go run ./cmd/relaycheck demo "${SEED_ARGS[@]}" --crash-after-segment 3 --out ./.relay-recover \
  > /tmp/relay_crash.txt 2>&1
grep -q 'exit_code=75' /tmp/relay_crash.txt && ok "crash reports exit_code=75" || bad "crash marker"
./bin/relaycheck demo "${SEED_ARGS[@]}" --crash-after-segment 3 --out /tmp/relay-crash-bin >/dev/null 2>&1
[ $? -eq 75 ] && ok "binary crash exit 75" || bad "binary crash exit != 75"

go run ./cmd/relaycheck demo "${SEED_ARGS[@]}" --crash-after-segment 3 --out ./.relay-recover --resume \
  > /tmp/relay_resume.txt 2>/dev/null
[ $? -eq 0 ] && ok "resume exit 0" || bad "resume exit"
grep -q 'wal_replay=1'                /tmp/relay_resume.txt && ok "wal_replay=1" || bad "wal_replay"
grep -q 'resume_hash_match=100.00%'   /tmp/relay_resume.txt && ok "resume_hash_match=100%" || bad "resume hash"
grep -q 'segment_hash_match=100.00%'  /tmp/relay_resume.txt && ok "segment_hash_match=100%" || bad "segment hash resume"

grep -oE '[0-9a-f]{64}' /tmp/relay_ok.txt     > /tmp/h_ok
grep -oE '[0-9a-f]{64}' /tmp/relay_resume.txt > /tmp/h_res
if diff -q /tmp/h_ok /tmp/h_res >/dev/null; then
  ok "resume hashes identical to uninterrupted run"
else
  bad "resume hash sequence differs"
fi

echo
echo "RESULT pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
