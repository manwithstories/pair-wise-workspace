# mediaflow — 区域直播分发的本地媒体处理内核

纯 Go、纯本地的 CMAF/fMP4 + RTP 排障内核。所有样例数据由代码在内存中生成，
不读取任何外部夹具。三表示层各自独立扫描、索引、恢复，再在统一微秒时钟上
交叉核对边界。

## 快速开始

```sh
go mod tidy
go build ./...
go test ./...
go run ./cmd/mediad demo --seed 7 --representations 3 --loss 0.03 --truncate-every 11
```

`demo` 输出结构化 JSON 摘要；`verify` 只打印阈值结论。退出码：

| 码 | 含义 |
|----|------|
| 0 | 演示完成且全部阈值通过 |
| 1 | 验收指标未达标（例如恢复率低于 0.90） |
| 2 | 参数或输入无效 |

常用参数：`--seed`、`--representations 1..8`、`--loss 0..0.5`、
`--truncate-every`（0 关闭）、`--duration`、`--fps`、`--fragment`、
`--window`、`--compact`。

## 模块

| 路径 | 职责 |
|------|------|
| `internal/clock` | 统一微秒时间线、逐轨 timescale 转换、区间覆盖集、共享误差模型、nearest-rank 分位数 |
| `internal/cmaf` | 盒头有界解析、顶层重同步、init/media 增量扫描、样本绝对偏移与越界判定 |
| `internal/gen` | 逐字节构造 ftyp/styp/moov/trak/moof/trun 及损坏夹具 |
| `internal/inspect` | GOP 间隔统计、关键帧冲突保留、损坏区段映射到统一时钟 |
| `internal/repair` | 单表示层有界恢复、确定性摘要、可失效重建索引、跨层边界核对 |
| `internal/transport` | RTP 打包、16 位序号有界重排窗口、丢包/乱序/重复/恢复率、超窗不可恢复计数 |
| `internal/pipeline` | 多码率扇出、共享时钟扇入、JSON 摘要与阈值判定 |
| `cmd/mediad` | 终端入口，参数校验与退出码 |

数据流：`gen → cmaf(扫描) → inspect(检查) → repair(恢复) → transport(RTP) →
pipeline(扇入) → mediad(输出)`。扫描向恢复提供分片与时间戳；传输把 RTP 恢复
结果送回各表示层；扇入汇总边界后交给 `repair.CrossCheck` 核验。

## 摘要字段

顶层 JSON 含验收所需字段：

- `sync_error_ms` — 各表示层相对参考层的最大起止边界差
- `loss_recovery_rate` — `delivered / sent`，范围 0.90–1.00
- `p95_latency_ms` — 单表示层处理墙钟时间的 P95；`latency_scope` 注明范围
- `damaged_ranges` — 每段含 `layer`、统一时钟 `start_us/end_us`、容器 `offset`、`segment`、`kind`
- `gop_interval_ms` — 各层关键帧间隔
- `boundary_mismatch_count` / `boundary_checks` — 超容差层数与逐层差值
- `recovery_digest` / `layer_digests` — 同 seed 可复现
- `truth` — 注入的损坏清单，便于与报告对照

`damaged_ranges[].kind` 取值：`box_size_overrun`、`segment_truncated`、
`sample_out_of_bounds`、`mfhd_gap`、`mfhd_duplicate`、`tfdt_regression`、
`keyframe_conflict`。

## 设计要点

- **有界解析**：段大小、盒大小、单分片样本数、分片数、重排窗口全部有上限，
  损坏长度不会导致越界读取或无界分配。
- **不猜**：索引缺失时从实际分片重算并标 `SourceInferred`；无法定位的损坏
  保留 `position_known=false`，不进入媒体时间运算；关键帧冲突计入
  `uncertain` 而非静默改判。
- **失败隔离**：单层 init 或分片损坏只标记该层；其余层与后续分片照常处理，
  层摘要互不引用。
- **确定性**：夹具与 RTP 损伤都由 seed 派生；摘要哈希排除计时字段，重复运行
  的恢复摘要与损坏区段一致。
- **可核对**：`internal/pipeline/verify_*_test.go` 不复用扫描器，直接用
  go-mp4 重新解码生成的字节，独立复核边界与错误偏移。

## 性能

普通 Apple Silicon 上，30 秒 × 3 表示层（约 2700 样本、5 千余 RTP 包）的
单层处理墙钟时间 P95 约 3 ms，远低于 100 ms 目标。延迟只统计生成之后该层
的扫描、检查、恢复与传输，不含夹具构造。
