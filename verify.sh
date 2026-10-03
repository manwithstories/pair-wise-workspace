#!/bin/sh
# verify.sh — 端到端验收脚本。
#
# 用法：node build/server.js 启动服务后，另开终端执行 sh verify.sh
# 覆盖：并发灌入 -> 取快照 LSN -> 注入载荷被拒 -> 正常查询窗口正确
#
# 说明：本机可能设置了 http_proxy 等环境变量，curl 默认会走代理并把请求
# 打到外部去（表现为 502）。因此这里统一加 --noproxy '*' 直连本地服务。
set -eu

BASE="${BASE:-http://127.0.0.1:8080}"
CURL="curl -s --noproxy '*' -m 15"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

ok()   { pass=$((pass+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# 摄入层是异步微批：POST 返回 202 只表示已入队，真正落盘并对查询可见要晚几毫秒。
# 因此每一条「写完立刻查」的断言都必须先等 watermark 追平，否则会读到旧快照。
# settle <期望的 watermark 下限>：轮询 healthz 直到 watermark 达标。
settle() {
  want="$1"
  i=0
  while [ "$i" -lt 100 ]; do
    cur=$($CURL "$BASE/healthz" | sed -n 's/.*"lsn":\([0-9]*\).*/\1/p')
    case "$cur" in
      ''|*[!0-9]*) cur=0 ;;
    esac
    [ "$cur" -ge "$want" ] && return 0
    i=$((i + 1))
    sleep 0.05
  done
  return 1
}

# assert_eq <描述> <实际> <期望>
assert_eq() {
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "expected [$3] got [$2]"; fi
}

# ---------------------------------------------------------------------------
step "0. 服务可达性"
HEALTH=$($CURL "$BASE/healthz" || true)
case "$HEALTH" in
  *'"status":"ok"'*) ok "healthz 返回 ok" ;;
  *) bad "healthz 返回 ok" "响应: $HEALTH" ;;
esac

# ---------------------------------------------------------------------------
step "1. 并发灌入 10 个批次 x 20 事件（200 条，20 个并发生产者）"
NOW=$(python3 -c 'import time;print(int(time.time()*1000))' 2>/dev/null || echo 1700000000000)
i=0
pids=""
while [ $i -lt 10 ]; do
  producer="svc-$i"
  events=""
  j=0
  while [ $j -lt 20 ]; do
    ts=$((NOW + i * 100 + j))
    [ -n "$events" ] && events="$events,"
    events="$events{\"producerSeq\":$j,\"kind\":\"metric\",\"ts\":$ts,\"value\":$((i * 100 + j)),\"service\":\"svc-$i\",\"region\":\"cn\",\"tenant\":\"t1\",\"message\":\"load $i/$j\",\"level\":\"info\"}"
    j=$((j + 1))
  done
  (
    $CURL -X POST "$BASE/v1/streams/metrics/events" \
      -H 'content-type: application/json' \
      -d "{\"stream\":\"metrics\",\"producer\":\"$producer\",\"events\":[$events]}" \
      -o "$TMP/ingest-$i.json" -w '%{http_code}' > "$TMP/ingest-$i.code"
  ) &
  pids="$pids $!"
  i=$((i + 1))
done
for p in $pids; do wait "$p"; done

ing_ok=0
i=0
while [ $i -lt 10 ]; do
  [ "$(cat "$TMP/ingest-$i.code")" = "202" ] && ing_ok=$((ing_ok + 1))
  i=$((i + 1))
done
assert_eq "10 个并发批次全部 202" "$ing_ok" "10"

# ---------------------------------------------------------------------------
step "2. 取快照 LSN"
SNAP=$($CURL "$BASE/healthz")
WATERMARK=$(printf '%s' "$SNAP" | sed -n 's/.*"lsn":\([0-9]*\).*/\1/p')
case "$WATERMARK" in
  ''|*[!0-9]*) bad "解析 watermark LSN" "响应: $SNAP" ;;
  *) if [ "$WATERMARK" -gt 0 ]; then ok "watermark LSN = $WATERMARK"; else bad "watermark 应大于 0" "got $WATERMARK"; fi ;;
esac

# ---------------------------------------------------------------------------
step "3. 注入载荷必须被拒（HTTP 400 + 结构化错误码）"

# assert_reject <描述> <filter> <期望错误码>
assert_reject() {
  body=$($CURL -X POST "$BASE/v1/streams/metrics/query" \
    -H 'content-type: application/json' \
    --data-raw "$(python3 -c "import json,sys;print(json.dumps({'filter':sys.argv[1]}))" "$2")" \
    -w '\n%{http_code}')
  code=$(printf '%s' "$body" | tail -n1)
  payload=$(printf '%s' "$body" | sed '$d')
  if [ "$code" != "400" ]; then
    bad "$1" "期望 HTTP 400，实际 $code"
    return
  fi
  case "$payload" in
    *"\"$3\""*) ok "$1 (400/$3)" ;;
    *) bad "$1" "期望错误码 $3，响应: $payload" ;;
  esac
}

assert_reject "分号多语句"        'ts >= 1; DROP TABLE users'        E_QUERY_INJECTION
assert_reject "注释走私"          'ts >= 1 -- x'                     E_QUERY_INJECTION
assert_reject "块注释走私"        'ts >= 1 /* x */'                  E_QUERY_INJECTION
assert_reject "点号原型链穿透"    'service.__proto__.polluted == "1"' E_QUERY_INJECTION
assert_reject "管道命令走私"      'service == "a" | cmd'             E_QUERY_INJECTION
assert_reject "引号走私"          'service == "a\"b" || ts < 0'      E_QUERY_INJECTION
assert_reject "Unicode 同形字段"  'serviсe == "a"'                   E_QUERY_INJECTION
assert_reject "全角字段名"        'ｓervice == "a"'                   E_QUERY_INJECTION
assert_reject "零宽字符字段名"    'servic‌e == "a"'                  E_QUERY_INJECTION
assert_reject "越权字段"          'secret_field == "x"'              E_QUERY_FIELD_NOT_ALLOWED
assert_reject "原型链字段"        'constructor == "x"'               E_QUERY_FIELD_NOT_ALLOWED
assert_reject "数字走私"          'ts >= 0x1f'                       E_QUERY_INJECTION
assert_reject "尾随垃圾 token"    'ts >= 1 garbage'                  E_QUERY_INJECTION

# 写入侧同样必须拦住注入字段值
WCODE=$($CURL -o "$TMP/w.json" -w '%{http_code}' -X POST "$BASE/v1/streams/metrics/events" \
  -H 'content-type: application/json' \
  -d '{"stream":"metrics","producer":"svc-evil","events":[{"producerSeq":1,"kind":"audit","ts":1,"service":"a.b"}]}')
assert_eq "写入侧点号注入被拒" "$WCODE" "400"
PCODE=$($CURL -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/streams/metrics/events" \
  -H 'content-type: application/json' \
  -d '{"stream":"metrics","producer":"__proto__","events":[{"producerSeq":1,"kind":"audit","ts":1}]}')
assert_eq "写入侧 __proto__ 生产者被拒" "$PCODE" "400"

# ---------------------------------------------------------------------------
step "4. 正常参数化查询返回正确窗口"

# 4a. 灌入一条窗口边界清晰的探针事件
FROM=$((NOW - 500000))
PROBE=$((NOW - 500000))
MID=$((NOW - 400000))
TO=$((NOW - 300000))
$CURL -o /dev/null -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' \
  -d '{"filter":"ts >= 1","limit":1}'

probe_events=""
# 探针按本次运行唯一（PID + 时间戳），否则第二次运行时 (stream,producer,seq)
# 幂等键会把探针整批判为重复，断言就会假失败。服务名也带 run-id 以免历史数据混入。
RUN_ID="$$-$(date +%s 2>/dev/null || echo 0)"
PROBE_SVC="probe-svc-$RUN_ID"
PROBE_PRODUCER="probe-producer-$RUN_ID"
PROBE_TENANT="t9-$RUN_ID"
for pair in "$FROM:0" "$MID:1" "$TO:2"; do
  ts=${pair%:*}; sq=${pair#*:}
  [ -n "$probe_events" ] && probe_events="$probe_events,"
  probe_events="$probe_events{\"producerSeq\":$((900 + sq)),\"kind\":\"audit\",\"ts\":$ts,\"value\":$sq,\"service\":\"$PROBE_SVC\",\"region\":\"cn\",\"tenant\":\"$PROBE_TENANT\",\"message\":\"probe-$sq\",\"level\":\"info\"}"
done
$CURL -o "$TMP/probe.json" -w '%{http_code}' -X POST "$BASE/v1/streams/metrics/events" \
  -H 'content-type: application/json' \
  -d "{\"stream\":\"metrics\",\"producer\":\"$PROBE_PRODUCER\",\"events\":[$probe_events]}" > "$TMP/probe.code"
assert_eq "探针事件写入成功" "$(cat "$TMP/probe.code")" "202"
settle $((WATERMARK + 3)) || true

# 4b. 查 [FROM, TO] 应恰好命中 3 条探针
Q1=$($CURL -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' \
  -d "{\"filter\":\"service == \\\"$PROBE_SVC\\\" and ts >= $FROM and ts <= $TO\",\"limit\":100}")
Q1COUNT=$(printf '%s' "$Q1" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "时间窗口 + 字段过滤命中 3 条" "$Q1COUNT" "3"

# 4c. 只取窗口下界，排除上界之外
Q2=$($CURL -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' \
  -d "{\"filter\":\"service == \\\"$PROBE_SVC\\\" and ts >= $FROM and ts < $MID\",\"limit\":100}")
Q2COUNT=$(printf '%s' "$Q2" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "ts < MID 只命中 1 条（下界开区间正确）" "$Q2COUNT" "1"

# 4d. 精确等于单点
Q3=$($CURL -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' \
  -d "{\"filter\":\"service == \\\"$PROBE_SVC\\\" and ts == $MID\",\"limit\":100}")
Q3COUNT=$(printf '%s' "$Q3" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "ts == MID 精确命中 1 条" "$Q3COUNT" "1"

# 4e. AND 多条件
Q4=$($CURL -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' \
  -d "{\"filter\":\"service == \\\"$PROBE_SVC\\\" and region == \\\"cn\\\" and tenant == \\\"$PROBE_TENANT\\\"\",\"limit\":100}")
Q4COUNT=$(printf '%s' "$Q4" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "三条件 AND 命中 3 条" "$Q4COUNT" "3"

# 4f. OR 组合
Q5=$($CURL -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' \
  -d "{\"filter\":\"service == \\\"$PROBE_SVC\\\" and (ts == $FROM or ts == $TO)\",\"limit\":100}")
Q5COUNT=$(printf '%s' "$Q5" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "OR 组合命中 2 条" "$Q5COUNT" "2"

# 4g. limit 生效
Q6=$($CURL -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' \
  -d "{\"filter\":\"service == \\\"$PROBE_SVC\\\"\",\"limit\":2}")
Q6COUNT=$(printf '%s' "$Q6" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "limit=2 生效" "$Q6COUNT" "2"

# 4h. 空窗口返回 0 条而不是报错
Q7=$($CURL -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' \
  -d '{"filter":"ts >= 100 and ts <= 200","limit":100}')
Q7COUNT=$(printf '%s' "$Q7" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "窗口外返回 0 条" "$Q7COUNT" "0"

step "5. 快照一致性：指定 LSN 只看到该 LSN 之前的数据"
# 要点：两次查询必须针对同一个 stream，否则"看不到"可能只是看错了流。
# 步骤：记下当前水位 -> 写入新事件 -> 等落盘 -> 旧 LSN 读不到 / 新水位能读到。
SNAP_STREAM="metrics"
MARK_SVC="snap-probe-$$"
MARKER_LSN=$($CURL "$BASE/healthz" | sed -n 's/.*"lsn":\([0-9]*\).*/\1/p')
SNAP_NOW=$(python3 -c 'import time;print(int(time.time()*1000))')
LATE=$($CURL -X POST "$BASE/v1/streams/$SNAP_STREAM/events" \
  -H 'content-type: application/json' \
  -d "{\"stream\":\"$SNAP_STREAM\",\"producer\":\"p-snap-$$\",\"events\":[{\"producerSeq\":1,\"kind\":\"audit\",\"ts\":$SNAP_NOW,\"service\":\"$MARK_SVC\"}]}")
LATE_LSN=$(printf '%s' "$LATE" | sed -n 's/.*"lsn":\([0-9]*\).*/\1/p')
settle "${LATE_LSN:-0}" || true

# 旧快照（写入前的 LSN）必须看不到这条事件
Q8=$($CURL -X POST "$BASE/v1/streams/$SNAP_STREAM/query" \
  -H 'content-type: application/json' \
  -d "{\"lsn\":$MARKER_LSN,\"filter\":\"service == \\\"$MARK_SVC\\\"\",\"limit\":10}")
Q8COUNT=$(printf '%s' "$Q8" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "旧快照看不到快照点之后的写入" "$Q8COUNT" "0"

# 对照组：最新快照必须能看到。没有这条，上面的 0 可能只是"压根没写进去"，会假通过。
LATEST_LSN=$($CURL "$BASE/healthz" | sed -n 's/.*"lsn":\([0-9]*\).*/\1/p')
Q9=$($CURL -X POST "$BASE/v1/streams/$SNAP_STREAM/query" -H 'content-type: application/json' \
  -d "{\"lsn\":$LATEST_LSN,\"filter\":\"service == \\\"$MARK_SVC\\\"\",\"limit\":10}")
Q9COUNT=$(printf '%s' "$Q9" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "最新快照能看到该写入（对照组）" "$Q9COUNT" "1"

# 顺带确认因果关系：新写入的 LSN 必须真的在旧快照点之后
if [ "$LATE_LSN" -le "$MARKER_LSN" ]; then
  bad "新写入的 LSN 应大于旧快照点" "new=$LATE_LSN marker=$MARKER_LSN"
else
  ok "新写入 LSN ($LATE_LSN) > 快照点 ($MARKER_LSN)"
fi

# ---------------------------------------------------------------------------
step "6. 幂等：重发同一批不会重复落库"
# 用带 $$ 的 stream 名，确保不与历史运行残留的同 (producer,seq) 记录冲突。
# 同时注意：路径里的 stream 必须与 body 里的 stream 一致，否则会被 400 拒绝。
IDEM_STREAM="idem-verify-$$"
IDEM_BODY="{\"stream\":\"$IDEM_STREAM\",\"producer\":\"idem-p\",\"events\":[{\"producerSeq\":1,\"kind\":\"audit\",\"ts\":5000,\"service\":\"idem-svc\"},{\"producerSeq\":2,\"kind\":\"audit\",\"ts\":5001,\"service\":\"idem-svc\"}]}"
FIRST=$($CURL -X POST "$BASE/v1/streams/$IDEM_STREAM/events" -H 'content-type: application/json' -d "$IDEM_BODY")
FIRST_LSN=$(printf '%s' "$FIRST" | sed -n 's/.*"lsn":\([0-9]*\).*/\1/p')
settle "${FIRST_LSN:-0}" || true
BEFORE=$($CURL -X POST "$BASE/v1/streams/$IDEM_STREAM/query" -H 'content-type: application/json' -d '{"filter":"lsn >= 1","limit":100}' | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "首次写入落库 2 条" "$BEFORE" "2"
RETRY=$($CURL -X POST "$BASE/v1/streams/$IDEM_STREAM/events" -H 'content-type: application/json' -d "$IDEM_BODY")
RETRY_DEDUP=$(printf '%s' "$RETRY" | sed -n 's/.*"deduped":\([0-9]*\).*/\1/p')
AFTER=$($CURL -X POST "$BASE/v1/streams/$IDEM_STREAM/query" -H 'content-type: application/json' -d '{"filter":"lsn >= 1","limit":100}' | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
assert_eq "重发批次 deduped=2" "$RETRY_DEDUP" "2"
assert_eq "重发后总条数不变 (${BEFORE})" "$AFTER" "$BEFORE"

# ---------------------------------------------------------------------------
step "7. 统一错误出口：404 / 非法 LSN"
NCODE=$($CURL -o /dev/null -w '%{http_code}' "$BASE/v1/does-not-exist")
assert_eq "未知路由返回 404" "$NCODE" "404"
LCODE=$($CURL -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/streams/metrics/query" \
  -H 'content-type: application/json' -d '{"lsn":99999999,"filter":"lsn >= 1"}')
assert_eq "超前 LSN 返回 400" "$LCODE" "400"
BCODE=$($CURL -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/streams/metrics/events" \
  -H 'content-type: application/json' -d '{"stream":"metrics","producer":"p","events":"not-an-array"}')
assert_eq "类型不符返回 400" "$BCODE" "400"

# ---------------------------------------------------------------------------
printf '\n\033[1m==================== 结果 ====================\033[0m\n'
printf '  通过: \033[32m%d\033[0m\n' "$pass"
printf '  失败: \033[31m%d\033[0m\n' "$fail"
if [ "$fail" -eq 0 ]; then
  printf '\n\033[32m全部通过\033[0m\n'
  exit 0
else
  printf '\n\033[31m存在失败项\033[0m\n'
  exit 1
fi
