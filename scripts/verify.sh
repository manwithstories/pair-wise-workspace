#!/bin/zsh
# 验收脚本：复核 demo 命令输出与退出码。
# 退出码：0=全部校验通过 2=输入损坏 3=缓存或检查点不可信 1=其他失败。
set -u
cd "$(dirname "$0")/.."

fail() { print -u2 "FAIL: $1"; exit 1; }
need() { grep -q "^$1=" "$2" || fail "missing key $1 in $2"; }
val() { grep "^$1=" "$2" | cut -d= -f2; }

print "== go mod tidy / build / test =="
go mod tidy || fail "go mod tidy"
go build ./... || fail "go build"
go test ./... || fail "go test"

# go run 不透传非零退出码，验收退出码须用构建产物
BIN=$(mktemp -u)/mediaprobe
go build -o "$BIN" ./cmd/mediaprobe || fail "build mediaprobe"

WD=.mediaprobe
OUT1=$(mktemp); OUT2=$(mktemp); OUT3=$(mktemp)
trap "rm -f $OUT1 $OUT2 $OUT3 $OUT1.n $OUT2.n $OUT1.m $OUT3.m; rm -rf $(dirname $BIN)" EXIT

print "== 1. 成功样例（含 --resume，首次运行）=="
rm -rf $WD
"$BIN" demo --seed 17 --loss-burst 12 --bandwidth-kbps 420 --resume >$OUT1
[ $? -eq 0 ] || fail "first run exit code"
for k in source_sha256 recovered_packets lost_packets late_packets target_kbps \
	actual_kbps bitrate_error_pct quality_psnr_db manifest_start_ms \
	timeline_error_ms cache_rejected checkpoint_resumed; do
	need $k $OUT1
done
[ "$(val timeline_error_ms $OUT1)" -le 20 ] || fail "timeline error > 20ms"
awk -v e="$(val bitrate_error_pct $OUT1)" 'BEGIN{exit !(e+0 <= 8)}' || fail "bitrate error > 8%"
[ "$(val cache_rejected $OUT1)" = 0 ] || fail "unexpected cache_rejected"
[ "$(val checkpoint_resumed $OUT1)" = 0 ] || fail "unexpected checkpoint_resumed"

print "== 2. 重复运行：检查点续跑且摘要一致 =="
"$BIN" demo --seed 17 --loss-burst 12 --bandwidth-kbps 420 --resume >$OUT2
[ $? -eq 0 ] || fail "second run exit code"
[ "$(val checkpoint_resumed $OUT2)" = 1 ] || fail "checkpoint_resumed != 1"
grep -v '^checkpoint_resumed=' $OUT1 >$OUT1.n
grep -v '^checkpoint_resumed=' $OUT2 >$OUT2.n
diff -q $OUT1.n $OUT2.n >/dev/null || fail "summaries differ across reruns"

print "== 3. 损坏样例：截断输入，报告错误偏移并非零退出 =="
SZ=$(stat -f%z $WD/input.webm)
HEAD=$((SZ / 2))
{ head -c $HEAD $WD/input.webm >$WD/input.webm.tmp && mv $WD/input.webm.tmp $WD/input.webm; } || fail "truncate"
"$BIN" demo --seed 17 --loss-burst 12 --bandwidth-kbps 420 >$OUT3
RC=$?
[ $RC -eq 2 ] || fail "corrupt input exit code $RC, want 2"
need error_offset $OUT3
[ "$(val error_offset $OUT3)" -gt 0 ] || fail "error_offset not positive"

print "== 4. 摘要篡改：cache_rejected=1 并重算，退出码 3 =="
rm -rf $WD
"$BIN" demo --seed 17 --loss-burst 12 --bandwidth-kbps 420 >/dev/null || fail "regenerate"
VICTIM=$(ls $WD/cache | grep -v '\.meta$' | grep -v quarantine | head -1)
[ -n "$VICTIM" ] || fail "no cache object"
print "tampered" >$WD/cache/$VICTIM
"$BIN" demo --seed 17 --loss-burst 12 --bandwidth-kbps 420 --resume >$OUT3
RC=$?
[ $RC -eq 3 ] || fail "tampered cache exit code $RC, want 3"
[ "$(val cache_rejected $OUT3)" = 1 ] || fail "cache_rejected != 1"
[ -f "$WD/cache/quarantine/$VICTIM" ] || fail "tampered object not quarantined"
# 重算结果与首次一致
grep -v -E '^(cache_rejected|checkpoint_resumed)=' $OUT1 >$OUT1.m
grep -v -E '^(cache_rejected|checkpoint_resumed)=' $OUT3 >$OUT3.m
diff -q $OUT1.m $OUT3.m >/dev/null || fail "recomputed summary differs"

print "== 5. 检查点损坏：退出码 3 =="
rm -rf $WD
"$BIN" demo --seed 17 --loss-burst 12 --bandwidth-kbps 420 >/dev/null || fail "regenerate"
print "{bad json" >$WD/checkpoint.json
"$BIN" demo --seed 17 --loss-burst 12 --bandwidth-kbps 420 --resume >$OUT3
RC=$?
[ $RC -eq 3 ] || fail "corrupt checkpoint exit code $RC, want 3"
[ "$(val cache_rejected $OUT3)" = 1 ] || fail "cache_rejected != 1"

rm -rf $WD
print "PASS: all checks passed"
exit 0
