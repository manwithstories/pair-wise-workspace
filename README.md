# mediamerge

无损转封装 MP4/MOV 音视频轨道的本地命令行工具，带写前日志、阶段提交与崩溃恢复。

## 环境

- Go 1.23（`go version` 需为 1.23 或更高）
- 纯本地运行，无网络、无外部服务
- 依赖版本由 `go.mod` 固定：`github.com/abema/go-mp4 v1.4.1`、`github.com/pion/rtp v1.8.11`

## 验收命令

```sh
go mod tidy && go build ./... && go test ./... && \
go run ./cmd/mediamerge demo --dir "$TMPDIR/mediamerge-demo" --tracks 3 --samples 240 --queue 8 && \
go run ./cmd/mediamerge verify --dir "$TMPDIR/mediamerge-demo" && \
go run ./cmd/mediamerge corrupt --dir "$TMPDIR/mediamerge-demo" --kind truncated-box && \
go run ./cmd/mediamerge recover --dir "$TMPDIR/mediamerge-demo" --interrupt-after 2
```

预期输出：

```
tracks=3 samples=720 input_sha256=<64 hex> output_sha256=<64 hex> sample_sha256_match=true
error_offset=<byte offset> locator_delta_bytes=0
recovered_stage=2 resume_ms=<ms> rollback_bytes=<bytes> output_sha256=<64 hex>
```

## 命令

| 命令 | 说明 |
| --- | --- |
| `demo --dir D [--tracks N] [--samples N] [--queue N]` | 生成确定性夹具并完整转封装，打印摘要行 |
| `verify --dir D` | 核对日志、文件长度、输入指纹与逐轨压缩样本 SHA-256 |
| `corrupt --dir D --kind K` | 生成单点损坏夹具到 `input.<kind>.mp4`，打印首个不可解析字节偏移 |
| `recover --dir D [--interrupt-after N] [--queue N]` | 在阶段 N 后模拟崩溃，恢复并续跑 |
| `merge --input I --output O [--map "1=10:video,..."]` | 对任意输入按映射规则转封装 |
| `plan --input I [--map ...]` | 只打印校验后的映射，不写媒体 |
| `summarize --dir D [--json]` | 打印轨道映射、样本数、时间范围与双 SHA-256 |

损坏类型：`truncated-box`、`corrupt-sample-table`、`tail-truncation`、`corrupt-sample`、`journal-mismatch`。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 2 | 输入或映射无效（解复用、样本表、映射、时基错误） |
| 3 | 可恢复处理失败（流、写入、取消、恢复失败） |
| 4 | 日志或输出完整性校验失败 |

## 架构

```
demux/validate → mapping → stream(有界背压) → mux/摘要
                              ↓
                    journal(写前日志) → txn(阶段提交/恢复)
```

- `internal/clock`：整数有理数时间换算，禁止浮点累计；RTP 90000/48000 视图与
  absolute-capture-time 扩展。
- `internal/model`：共享错误结构（阶段、输入偏移、轨道号、可恢复性、最后完整边界）。
- `internal/demux`：流式读取盒与样本表；未知必需盒、盒越界、尾部截断、样本表损坏都
  报告首个不可解析字节。
- `internal/mapping`：轨道选择、重编号、媒体类型校验；重复目标号/缺失必需轨道写出前失败。
- `internal/stream`：容量固定的样本队列，满时阻塞生产者；常驻缓冲不超过容量 + 2 个样本。
- `internal/mux`：所有样本偏移在写入前确定，压缩样本原样复制；中断与不中断输出逐字节一致。
- `internal/journal`：每阶段 begin/commit 记录，commit 带输入指纹、输出长度与前缀摘要。
- `internal/txn`：阶段提交、幂等恢复、未提交尾部回滚、断点续跑。
- `internal/fixture`：确定性 MP4 夹具与四类单点损坏生成器。
- `internal/verify`、`internal/safepath`：产物核对与工作目录约束。

## 约束

- 常驻缓冲 ≤ 队列容量 + 2 个样本；默认样例 ≥ 10 MB。
- 仅支持常见非加密 MP4/MOV 与 AAC、ALAC、H.264、HEVC 压缩样本。
- 不解码、不重编码、不修复损坏媒体。
- 临时产物只写入 `--dir` 指定目录；拒绝路径穿越与符号链接。
- 构建产物不纳入交付，不保存任何密钥或凭据。
