#!/bin/sh
# End-to-end demo of the kmsctl pipeline: fixture generation, encrypted
# packing with key rotation, license issuance, verification, and the three
# classic incidents (tamper, nonce replay, expired license).
set -eu
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
BIN="$WORK/kmsctl"
go build -o "$BIN" ./cmd/kmsctl

echo "== 1. gen-fixture =="
"$BIN" gen-fixture --shards 24 --shard-size 65536 --seed 7 --out "$WORK/fx"

echo "== 2. encrypt (rotate every 8 shards) =="
"$BIN" encrypt --fixture "$WORK/fx" --rotate-every 8 --out "$WORK/pack"

echo "== 3. issue =="
"$BIN" issue --pack "$WORK/pack" --ttl 90s --shards 0-23 --out "$WORK/lic.json"

echo "== 4. verify (expect failed=0, exit 0) =="
"$BIN" verify --pack "$WORK/pack" --license "$WORK/lic.json" --json

echo "== 5. tamper two bytes of shard 5 =="
printf '\xde\xad' | dd of="$WORK/pack/shards/shard-000005.bin" bs=1 seek=1024 conv=notrunc 2>/dev/null

echo "== 6. re-issue with reused nonce and expired window =="
NONCE=$(sed -n 's/.*"nonce": "\([0-9a-f]*\)".*/\1/p' "$WORK/lic.json")
"$BIN" issue --pack "$WORK/pack" --ttl=-120s --shards 0-23 --nonce "$NONCE" --out "$WORK/lic2.json"

echo "== 7. verify (expect failed=3: tampered, replay, expired; exit 1) =="
set +e
OUT=$("$BIN" verify --pack "$WORK/pack" --license "$WORK/lic2.json" --json)
CODE=$?
set -e
echo "$OUT"
echo "exit=$CODE"
[ "$CODE" -eq 1 ] || { echo "FAIL: expected exit 1"; exit 1; }
echo "$OUT" | grep -q '"failed":3' || { echo "FAIL: expected failed=3"; exit 1; }
echo "$OUT" | grep -q '"tampered":1' || { echo "FAIL: expected tampered=1"; exit 1; }
echo "$OUT" | grep -q '"replay":1' || { echo "FAIL: expected replay=1"; exit 1; }
echo "$OUT" | grep -q '"expired":1' || { echo "FAIL: expected expired=1"; exit 1; }

echo "== 8. report =="
"$BIN" report --pack "$WORK/pack"
echo "DEMO OK (workspace: $WORK)"
