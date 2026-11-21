# mediad — 多轨媒体处理 DAG 流水线

面向远程制作团队的纯本地、可复现媒体处理工具。它把棚内（HLS）、外景（DASH）
和远端解说（HLS）的多轨分片归一到统一有理数时间轴，完成结构校验、关键帧切分、
静音检测、跨轨内容去重，并通过带检查点的 DAG 调度实现中断恢复与确定性重跑。

## 运行环境与依赖

- Go 1.23（纯标准库 `testing`，无外部服务、不联网）
- `github.com/abema/go-mp4 v1.4.0`：MP4 盒结构解码（`tfhd` 关键帧证据）
- `github.com/pion/rtp v1.8.10`：确定性 RTP 分包

```bash
go mod tidy
go build ./...
go test ./...
```

## 命令

```bash
# 生成成功样例并完整处理（退出码 0）
go run ./cmd/mediad demo --out ./tmp/demo

# 生成损坏样例并处理（退出码 2，可恢复）
go build -o ./tmp/mediad-bin ./cmd/mediad
./tmp/mediad-bin demo --out ./tmp/corrupt --corrupt

# 中断后恢复
./tmp/mediad-bin demo --out ./tmp/demo --interrupt-after 3
./tmp/mediad-bin resume --out ./tmp/demo
```

> 注意：`go run` 会把子进程的非零退出码统一包装成 `1`（Go 工具链行为）。
> 需要验证 `2` 这一退出码时请使用 `go build` 产出的二进制。

### 退出码

| 码 | 含义 |
|----|------|
| 0  | 完整成功 |
| 2  | 可恢复的不完整结果（损坏分片、部分时间线、中断） |
| 1  | 参数错误或不可恢复错误 |

### 报告字段

`report.json` 与终端输出包含契约字段：`segments`、`silence_ms`、`duplicates`、
`first_bad_offset`、`blocked_node`、`resume_ms`、`elapsed_ms`、`sha256`。

## 模块划分

| 包 | 职责 |
|----|------|
| `internal/model` | 溢出检查的有理数时间基准、时间线、媒体错误类型 |
| `internal/manifest` | HLS media playlist 与 DASH SegmentTemplate/Timeline 归一化 |
| `internal/segment` | 流式 MP4 盒遍历、字节级损坏定位、关键帧切分 |
| `internal/audio` | 流式 int16 PCM 静音检测 |
| `internal/fingerprint` | 确定性 RTP 指纹与跨轨去重（截断隔离） |
| `internal/dag` | 拓扑图、幂等键、并发调度、重试与阻塞传播 |
| `internal/store` | 原子文件检查点 |
| `internal/verify` | 可复现校验报告（稳定字段 sha256） |
| `internal/pipeline` | Go 生成夹具与全流程引擎 |
| `cmd/mediad` | 终端命令适配 |

## 关键设计

- **有理数时间**：HLS 十进制时长直接转分数，杜绝浮点误差；所有运算拒绝
  int64 溢出与负/零时长。
- **精确损坏定位**：walker 仅用 seek 与小头读取，峰值内存与分片大小无关；
  二分查找真实可读边界，首个坏字节定位误差不超过 1 字节，并区分可恢复性。
- **不伪造切点**：窗口落在分片内部或缺少关键帧证据时返回
  `ErrBoundaryUncertain`，绝不臆造切点。
- **截断隔离**：截断分片使用独立指纹分组键，绝不与完整内容合并。
- **幂等与恢复**：节点键由自身摘要与全部上游键哈希而成；检查点命中即跳过，
  输入摘要变化级联失效；同键重复产出被拒绝。
- **确定性**：报告 sha256 仅覆盖稳定内容字段（不含易变计时），重跑与
  中断恢复后摘要字节完全一致。
- **内存边界**：所有大文件均流式分块处理（64 KB 缓冲），满足 1 GB 输入、
  4 并发节点下峰值内存远低于 512 MB 的目标。

## 测试

```bash
go test ./...                 # 全量
go test -race ./internal/dag  # 调度并发安全
```

错误传播、恢复、损坏偏移、幂等键失效与输出可复现性均有测试覆盖。
