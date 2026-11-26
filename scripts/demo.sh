#!/bin/sh
# demo.sh — 端到端验收演示：生成样例、跑干净流、故障流中断续跑、独立核验，
# 并核对验收阈值与 run/verify 结果摘要一致。
#
# 退出码: 0 完成且核验通过; 脚本内部也会校验“中断”一步确实返回 3;
#         relaycheck 的 3=可续跑中断, 4=不可恢复输入或核验失败。
#
# 可覆盖变量: GO(默认 go1.25.6) DEMO(默认 /tmp/relay-demo)
#             BIN(默认 /tmp/relaycheck) CHUNK(默认 32)
set -eu

GO=${GO:-go1.25.6}
DEMO=${DEMO:-/tmp/relay-demo}
BIN=${BIN:-/tmp/relaycheck}
CHUNK=${CHUNK:-32}

case "$DEMO" in
  "" | "/" | "." ) echo "refusing to clean unsafe DEMO=$DEMO" >&2; exit 2 ;;
esac

cd "$(dirname "$0")/.."

echo "== modules / tests / build =="
$GO mod download
$GO test ./...
$GO build -o "$BIN" ./cmd/relaycheck

# 仅清理本演示拥有的派生产物，保证脚本可重复执行：否则上一轮已完成的
# recovery.idx 会让 “--stop-after 2” 直接幂等返回 0 而不是预期的 3。
# 不删除整个 $DEMO，以免误伤同目录下的其他文件。
rm -rf "$DEMO/clean" "$DEMO/faulty" "$DEMO/out" "$DEMO/recovered"
rm -f "$DEMO/index" "$DEMO/recovery.idx"

echo "== gen =="
"$BIN" gen --out "$DEMO" --seed 17 --segments 256 \
  --faults gap,rollback,duplicate,truncate,loss,map-conflict

echo "== clean run =="
"$BIN" run --input "$DEMO/clean" --output "$DEMO/out" \
  --index "$DEMO/index" --chunk "$CHUNK"

echo "== faulty run, stop after 2 chunks (expect exit 3) =="
set +e
"$BIN" run --input "$DEMO/faulty" --output "$DEMO/recovered" \
  --index "$DEMO/recovery.idx" --chunk "$CHUNK" --stop-after 2
code=$?
set -e
if [ "$code" -ne 3 ]; then
  echo "FAIL: expected resumable interrupt exit 3, got $code" >&2
  exit 1
fi

echo "== faulty run, resume to completion =="
"$BIN" run --input "$DEMO/faulty" --output "$DEMO/recovered" \
  --index "$DEMO/recovery.idx" --chunk "$CHUNK" --resume \
  > "$DEMO/metrics.run"
cat "$DEMO/metrics.run"

echo "== independent verify =="
"$BIN" verify --input "$DEMO/faulty" --output "$DEMO/recovered" \
  --index "$DEMO/recovery.idx" > "$DEMO/metrics.verify"
cat "$DEMO/metrics.verify"

# run 与独立 verify 必须得到同一个结果摘要。
rd=$(awk -F= '$1=="results_digest"{print $2}' "$DEMO/metrics.run")
vd=$(awk -F= '$1=="results_digest"{print $2}' "$DEMO/metrics.verify")
if [ -z "$vd" ] || [ "$rd" != "$vd" ]; then
  echo "FAIL: results digest mismatch run='$rd' verify='$vd'" >&2
  exit 1
fi

# 验收阈值：sync<=1000us, loss_recovery>=0.95, p95<=250ms, hash_match==1.0。
awk -F= '
  $1=="sync_max_us"        && $2+0 > 1000  {bad=bad " sync_max_us="$2}
  $1=="loss_recovery"      && $2+0 < 0.95  {bad=bad " loss_recovery="$2}
  $1=="latency_p95_ms"     && $2+0 > 250   {bad=bad " latency_p95_ms="$2}
  $1=="payload_hash_match" && $2+0 < 1     {bad=bad " payload_hash_match="$2}
  END {
    if (bad != "") { print "FAIL threshold(s):" bad > "/dev/stderr"; exit 1 }
  }
' "$DEMO/metrics.verify"

echo "== ACCEPTANCE PASSED (digest $vd) =="
