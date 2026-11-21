# mkvarchive — 本地 Matroska/WebM 归档转封装

纯 Go、无外部服务的影视档案封装恢复工具：流式解析 Matroska/WebM，校验轨道
依赖与音视频元数据，在不改变压缩块载荷的前提下生成可长期复核的归档副本、
清单与检查点。

## 依赖说明（与题面版本的偏差）

题面指定 `go-mp4 v1.31.0`，但该模块**不存在此版本**（截至实现时
`github.com/abema/go-mp4` 最新为 `v1.7.4`）。因此固定为可用的最新版
`v1.7.4`；`pion/rtp v1.8.9` 按题面固定。两者均通过 `go.mod` 锁定：

- `github.com/abema/go-mp4 v1.7.4` — 质量证据 MP4 sidecar 的 box 封装/读回
- `github.com/pion/rtp v1.8.9` — 块载荷 RTP 分包/重组完整性校验

## 架构与数据流

```
EBML 元素流（带字节偏移）
  └─ internal/ebml      流式 VINT/读取器、Cluster/块扫描器（未知长度、截断恢复）
        ├─ internal/track   Tracks 解析、依赖图（重复UID/缺失依赖/成环）、轨映射
        ├─ internal/audio   声道布局/采样率规范与显式保留策略
        ├─ internal/video   色彩元数据校验与缺失报告
        ├─ internal/timeline 统一纳秒时钟、确定性舍入、轨道时间范围清单
        └─ internal/remux   计划、无损写出器、SHA-256 质量统计、RTP/MP4 证据
              └─ internal/recovery 可验证阶段检查点与续跑
                    └─ internal/pipeline 编排；cmd/archive 终端适配
```

所有子系统共享 `internal/model` 的错误模型（错误码 + 严重级别 + 精确偏移）。

## 关键约束的落实

- **无损**：压缩帧载荷逐字节保留，仅重写轨号 VINT 前缀；隐式重采样、声道
  折叠、色彩改写在严格策略下拒绝，须显式 `retain` 才以清单警告放行。
- **偏移精确**：截断头报 `EBML_TRUNCATED_HEADER` 并锚定起始字节；越界长度报
  `EBML_OVERRUN` 锚定元素头；受损点前已确认的完整块全部可恢复。
- **确定性**：相同输入与计划重跑产生相同轨映射、清单时间戳与载荷摘要。
- **内存**：源经 `TeeReader` 流式哈希/计数，峰值取决于块缓冲而非整文件；
  超过 64 MiB 显式拒绝（`INPUT_TOO_LARGE`）。
- **安全**：仅处理本地文件，不执行输入内脚本或外部命令；未知元素跳过或
  明确报告。

## 运行

```bash
go mod tidy
go build ./...
go test ./...

go run ./cmd/archive demo --out ./tmp/archive
go run ./cmd/archive verify --input ./tmp/archive/sample.mkv \
    --manifest ./tmp/archive/manifest.json
go run ./cmd/archive demo --case truncated --out ./tmp/damaged
go run ./cmd/archive recover --input ./tmp/damaged/sample.mkv \
    --out ./tmp/recovered
```

> 注：`go run` 会把子进程的非零退出包装成 1；要核验部分恢复的退出码 2，
> 请先 `go build -o arc ./cmd/archive` 再直接运行 `./arc ...`。

### 退出码

| 结果 | 退出码 |
| --- | --- |
| 成功（全部块归档） | 0 |
| 部分恢复（损坏但有完整块可救） | 2 |
| 不可恢复 | 1 |

### 样例案例（全部由 Go 代码生成，无外部文件）

`standard` / `truncated` / `unknown-block` / `channels`（声道数与位置表不符）/
`samplerate`（多轨采样率冲突）/ `color`（色彩字段缺失）。

## 验收输出字段

`target_bitrate_kbps`、`actual_bitrate_kbps`、`bitrate_deviation_pct`、
`payload_sha256_match`、`quality_blocks`、`manifest_alignment_ms`，恢复时
另输出 `recovered_blocks` 与 `first_error_offset`，全部以命令行数值核验。
