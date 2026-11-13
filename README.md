# SOC Correlator

纯本地运行的 DHCP、NAT 与 EDR 离线关联工具。它面向动态地址办公网，用租约区间把 IP 还原为主机，用事件指纹保证重复上传幂等，并通过 300 秒事件窗口、规则引擎与主机小时桶异常检测输出可复现的量化报告。

## 快速开始

```bash
cargo run --release -- demo --seed 42
```

该命令会完成以下步骤：

1. 在 `./out/fixtures/` 生成 48 个上传批次，默认共 120000 行原始日志；
2. 解析、解绕、重排并批量写入 `./out/security.db`；
3. 运行三条关联规则和主机小时桶异常检测；
4. 输出终端指标并写入 `./out/report.json`。

默认 seed 42 的基准结果：

```text
窗口数=48 事件=120000 解析成功=119431 拒绝=569
回绕=1 身份冲突=7 迟到丢弃=112
规则 R1 命中=14 真=11 误报=3; R2 命中=9 真=8 误报=1; R3 命中=22 真=17 误报=5
规则命中率=0.800 误报率=0.200
异常会话数=37 异常主机=6
```

规则命中率定义为 `真命中 / 总命中`，误报率定义为 `误报 / 总命中`。因此默认夹具的 36/45 与 9/45 分别对应 0.800 与 0.200。

## 子命令

```bash
# 只做生成 + 入库（重复执行不会增加事件数）
cargo run --release -- ingest --input ./out/fixtures --db ./out/security.db

# 显式重开已闭合窗口，并把原先被水位线丢弃的事件重新接收
cargo run --release -- ingest \
  --input ./out/fixtures \
  --db ./out/security.db \
  --reopen-closed

# 从已有 SQLite 数据重新生成报告
cargo run --release -- report --db ./out/security.db --out ./out/report.json

# 百万行规模验证（夹具与数据库写入 /tmp，不污染交付目录）
scripts/bench_million.sh
```

本机 release 构建实测（macOS arm64）：1,000,000 原始行的完整 demo 约 15 秒，峰值 RSS 约 468MB；单独执行 `ingest` 约 14 秒、峰值约 280MB。

退出码：`0` 成功；`2` CLI 参数错误（clap）；`1` 运行时 I/O、数据库或数据错误。

## 数据格式

三类日志都使用共享的字节级扫描器 `src/parse/lexer.rs`，支持空白或逗号分隔的 `key=value`、双引号值与转义，不依赖正则库。单行失败只增加拒绝计数，并把前 120 字节摘要写入 `parse_rejections`，不会中断批次。

- DHCP：`ts dev batch seq ip host mac lease_start lease_end event`
- NAT：`ts dev batch seq [src_ip src_port] pub_ip pub_port dst_ip dst_port bytes direction action [signature]`
- EDR：`ts dev batch seq host_id [ip] action [src_ip] [dst_ip] [dst_port] [user] [signature]`
- 旧设备：额外带 `bits=32 anchor=<unix - counter>`，相邻回绕只补一个 `2^32`。

事件指纹由 `设备号 + 批次序号 + 原始事件时间 + 内容摘要` 组成。同一夹具重复入库时，`events` 与审计表均不增长。

## 处理语义

- 窗口：事件时间按 300 秒左闭右开切分；水位线推进到 `窗口结束 + 120 秒` 后写闭合标记。
- 迟到：比当前水位线早于 120 秒的事件写入 `late_drops`，不参与关联；`--reopen-closed` 可显式重开相关窗口并重新接收。
- 租约：使用左闭右开 `[start, end)`；重叠时后生效租约覆盖前租约并记录身份冲突。
- NAT：缺少内网源端时按 `(public_ip, public_port)` 反查；反查失败仍以 `host=unknown` 入库。
- R1：同主机 300 秒内 `scan → auth_fail → login_success`。
- R2：同主机、同目标端口 900 秒内至少 8 个罕见端口出站会话。
- R3：同主机 300 秒内来自至少 6 个不同源地址的认证失败。
- 误报：命中事件涉及 `10.10.100.0/24`、私网白名单或已知探针签名。
- 异常：主机小时桶字节数，α=0.3 的 EWMA 与 MAD 稳健 z；至少有 8 个历史桶且 `|z| > 3.5` 才判定，并区分尖峰与基线崩塌。

## 源码结构

```text
src/main.rs          CLI、退出码、demo/ingest/report 编排
src/gen.rs           确定性夹具生成、乱序/回绕/畸形/租约冲突注入
src/parse/lexer.rs   共享字节扫描器与内容摘要
src/parse/{dhcp,nat,edr}.rs  三类解析器，统一产出 ParseOutcome
src/timewin.rs       32 位解绕、隔离判定、乱序缓冲、水位线与窗口
src/identity.rs      租约区间、冲突消解、NAT 端口反查
src/store.rs         SQLite 建表、批量事务、指纹幂等、闭合/重开
src/correlate.rs     R1/R2/R3 规则与误报标注
src/anomaly.rs       主机小时桶 EWMA/MAD 检测
src/report.rs        终端指标与 report.json
src/pipeline.rs      两遍流式扫描与批处理编排
tests/pipeline_test.rs 端到端幂等、回绕与强制重开回归
```

数据依赖单向流动：生成夹具 → 解析 → 时间处理 → 身份归并 → 入库 → 只读计算 → 报告。单个 SQLite 写连接按批次提交，内存中仅保留当前批次、租约集合与 NAT 反查索引。

## 约束

- 仅读取 `gen` 生成的本地夹具，不读取真实系统日志，不访问外部服务。
- 依赖锁定在 `Cargo.lock`；SQLite 使用 rusqlite 0.31 的 bundled 构建。
- `target/`、`out/`、数据库与 WAL 文件均不进入交付。
