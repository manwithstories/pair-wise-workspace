#!/usr/bin/env bash
# 可复现端到端演示。产物写入 out/ 与 fixtures/（均被 .gitignore 忽略）。
set -u
cd "$(dirname "$0")/.."
MK=00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
go build -o /tmp/kmsctl-demo ./cmd/kmsctl || exit 1
K=/tmp/kmsctl-demo

echo "== 1. 生成确定性夹具 =="
$K gen-fixture --out fixtures/lesson.bin --size 2097152 --device dev-A

echo "== 2. 分段加密（128KiB/段，每 4 段轮换）=="
$K encrypt --in fixtures/lesson.bin --out out/lesson.enc \
  --manifest out/lesson.json --segment-size 131072 --rotate-every 4 --master-key $MK

echo "== 3. 签发许可证 =="
$K license issue --device dev-A --segments 0-15 \
  --not-before 2026-01-01T00:00:00Z --not-after 2026-01-01T00:10:00Z --out out/lic.json

echo "== 4. 正常校验（期望退出码 0）=="
$K verify --manifest out/lesson.json --license out/lic.json --device dev-A \
  --now 2026-01-01T00:05:00Z --report out/report.json --ledger out/demo-ledger.json
echo "exit=$?"

echo "== 5. 篡改第 7 段密文后校验（期望 TAG_INVALID，退出码 3）=="
python3 - <<'PY'
import json
m = json.load(open('out/lesson.json'))
off = m['segments'][7]['offset']
data = bytearray(open('out/lesson.enc', 'rb').read())
data[off] ^= 0xFF
open('out/lesson.tampered.enc', 'wb').write(data)
PY
$K verify --manifest out/lesson.json --license out/lic.json --device dev-A \
  --now 2026-01-01T00:05:00Z --report out/report-tamper.json \
  --ledger out/demo-ledger-tamper.json --ciphertext out/lesson.tampered.enc
echo "exit=$?"

echo "== 6. 过期校验（--now=00:20，期望 EXPIRED，退出码 4）=="
$K verify --manifest out/lesson.json --license out/lic.json --device dev-A \
  --now 2026-01-01T00:20:00Z --report out/report-expired.json \
  --ledger out/demo-ledger-expired.json
echo "exit=$?"

echo "== 7. 重放：重复使用同一凭据（期望 REPLAY，退出码 5）=="
$K verify --manifest out/lesson.json --license out/lic.json --device dev-A \
  --now 2026-01-01T00:05:00Z --report out/report-replay.json \
  --ledger out/demo-ledger.json
echo "exit=$?"

echo "== 8. 参数错误（期望退出码 2）=="
$K verify --manifest out/lesson.json 2>/dev/null
echo "exit=$?"

echo "== 9. 独立审计 =="
$K audit --report out/report.json
