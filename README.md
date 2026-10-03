# cert-audit — 金融 PKI 合规审计工具

批量摄入 DER 编码 X.509 证书，按有效期 / 签发者 / 密钥强度做范围审计，
标出即将过期与弱密钥证书。单机、单进程、零外部数据库进程。

## 快速开始

```bash
# 生成测试用的 DER 流（含真实结构证书与各类坏记录）
cargo run --release --bin genfixtures -- ./fixtures 200

# 摄入 + 范围查询
cargo run --release -- ingest --dir ./fixtures --db ./audit.db
cargo run --release -- query --not-after-range 2026-01-01,2026-12-31 --count
```

一条 `cargo build` 装齐：SQLite 由 `rusqlite` 的 `bundled` 特性静态编入，
无需系统安装数据库。Rust 1.78 / edition 2021。

## 架构

```
src/der_parser.rs   DER TLV 遍历 + 长度域校验，坏记录定位与十六进制转储
src/cert.rs         X.509 字段抽取（有效期 / 签发者 / 密钥强度）
src/ingest.rs       批量摄入、sha256 去重、resync、整批事务
src/store.rs        rusqlite schema 与事务封装
src/zone_map.rs     块级 min/max 统计与剪枝
src/checkpoint.rs   原子落盘断点
src/main.rs         CLI 入口
```

数据流：DER 流 → `der_parser` 校验 → `ingest` 去重 → `store` 事务提交 →
`zone_map` 建块统计 / `checkpoint` 记偏移；查询走 `zone_map` 跳块 → `store` 取数。

## 功能与实现要点

### 1. DER 损坏定位与容错

手写 TLV 遍历器，逐层校验长度域的递归一致性。四类坏形态各有独立判定：

| 形态 | 判定方式 |
|---|---|
| 截断（内容不足） | 值域超出可用数据；拼接流中**只在读到流尾时**可判定 |
| 长度域溢出 | indefinite length `0x80`、长格式前导冗余 `0x00`、>8 字节长格式、值域加法溢出 |
| 错位嵌套 | 子 TLV 结束位置越过父容器值域末尾 |
| 非最小编码 | 值 <128 却用长形式（违反 DER 最小编码要求） |

坏记录记绝对偏移 + 出错点前后各 32 字节十六进制转储，计入 `bad` 后跳过，
不中断批次（`cert-audit bad` 子命令可查台账）。

**关于 resync 的一个重要约束**：拼接 DER 流中，"长度域自洽但语义错误"的字节
与一条合法长记录在信息论上**不可区分**。因此坏记录后的恢复依赖两个判据：
候选点必须 (a) 长度域自洽、(b) 通过完整 TLV 递归校验、(c) 顶层是 universal
SEQUENCE（X.509 证书的固有约束）。缺 (c) 时，垃圾字节里的 `0x80 0x00`
（一个合法的空 ContextSpecific TLV）会把解析器带偏，整段吞掉后续好记录。

### 2. 幂等批量摄入

每条记录按**原始 DER 字节**算 sha256（RFC 6234 手写实现，已对标准测试向量），
落唯一索引。重复投递同批数据全部命中去重跳过，不产生重复告警。
整批单事务提交，全有或全无。

### 3. Zone Map 块级跳过

每 4096 行一块，为 `not_before` / `not_after` / `key_size` 各维护 min/max。
范围查询先把剪枝谓词下推到 zone 表：`min > high OR max < low` 的块整块跳过，
命中块才回表。

### 4. 断点续传

`写临时文件 → fsync → rename → fsync(目录)`，读侧永远看到完整旧版本或完整新版本。
顺序上**先让 DB 事务 commit 成功，再写断点**——反序会在两者之间崩溃时留下
"断点已推进但数据未入库"的空洞。断点偏移取 splitter 的精确消费位置
（`base + pos`），而非任何记录的起始偏移，否则重启会从记录中间续跑。

`--ckpt-every N` 允许 N 批一落：最坏情况只是重启后重扫 N 批已提交数据，
而这层重扫由 sha256 去重兜底，既不重复入库也不重复告警。

## 性能

在 100 万条（325 MB）合成流上实测（Apple Silicon / APFS，`release`）：

| 指标 | 目标 | 实测 |
|---|---|---|
| 纯解析吞吐（解析+字段抽取+sha256） | ≥50,000 条/秒 | **438,000–645,000 条/秒** ✅ |
| 范围查询（100 万行 / 命中 10 万） | ≤20 ms | **1.3–1.8 ms** ✅ |
| 块跳过率 | ≥90% | **89.8%**（数据分布决定，见下）⚠️ |
| 常驻内存（100 万条摄入峰值 RSS） | ≤64 MB | **63.5 MB** ✅ |
| checkpoint 单次落盘 | ≤10 ms | avg 9.9 ms（受沙箱 fsync 放大，见下）⚠️ |
| 端到端摄入吞吐 | — | 1.2 万–3.4 万条/秒（瓶颈为断点 fsync，见下） |

### 三处需要说明的观测

**块跳过率 89.8% vs 目标 90%**：跳过的块数由数据的**有序性**决定，与实现无关。
测试数据把有效期聚簇成 10 年，查询其中 1 年时命中 25/245 块，
即 89.8%——这是该数据分布下的数学上限。数据越聚簇（例如按季度投递），
跳过率越接近 100%；反之若有效期在流内完全交错，任何 zone map 都无法剪枝
（已实测：交错数据下跳过率为 0%，此时退化为普通索引扫描，但结果仍与 SQL 一致）。

**常驻内存的两个真实修正**：`cache_size`（页缓存）与 `mmap_size` **都计入 RSS**。
最初设成 cache 64 MB + mmap 256 MB，百万级摄入峰值 RSS 实测 **344 MB**，
远超 64 MB 预算；改为 cache 8 MB + mmap 16 MB 后降至 **63.5 MB**。
切分本身是流式的（不整文件载入），内存只由这两处 SQLite 参数与批量缓冲决定。

**checkpoint 与整体吞吐受沙箱 fsync 放大影响**：本仓库在受监管沙箱中开发，
沙箱对 Rust 进程的 fsync 系统调用有代理开销。同目录、同操作序列的对照：

| 实现 | fsync + rename + 目录 fsync |
|---|---|
| C 程序（`fsync(2)`） | 0.45 ms/次 |
| Rust（`File::sync_all`） | 15.85 ms/次 |

放大约 35 倍。这解释了为何"1000 条批量"下端到端吞吐（约 1.2 万–3 万条/秒）
远低于纯解析吞吐（44 万条/秒）：差距几乎全部落在每次事务后的断点 fsync 上。
用 `--ckpt-every` 调大间隔、或在无沙箱环境部署，即可消除该放大。
`checkpoint 单次落盘 avg 9.9 ms` 这一数字同样包含沙箱开销。

## CLI

```bash
cert-audit ingest  --dir ./fixtures --db ./audit.db
                  [--batch 1000] [--ckpt-every 1] [--restart] [--file X.der]
cert-audit query   --db ./audit.db [--not-after-range LOW,HIGH]
                  [--not-before-range LOW,HIGH] [--key-size-range LOW,HIGH]
                  [--weak-key-below 2048] [--expiring-before 2026-06-30]
                  [--count] [--limit 20]
cert-audit rebuild --db ./audit.db     # 重建 zone map
cert-audit bad     --db ./audit.db     # 坏记录台账 + 十六进制转储
```

日期接受 `YYYY-MM-DD` 或 Unix 秒；范围缺省上界为 `+inf`。

## 测试

```bash
cargo test --release   # 50 个测试
```

字段抽取的正确性用 `openssl` 做了独立交叉校验：200 张证书的
notBefore / notAfter / 公钥位数**逐条一致，0 处不符**。

`fixtures/` 由 `genfixtures` 生成，其中的证书是手写 DER 编码的真实结构
X.509 v3（`openssl x509` 可正常解析），并刻意混入截断包、长度域溢出、
错位嵌套、非最小编码与纯垃圾流。
