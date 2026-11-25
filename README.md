# avlab — 低延迟互动音视频媒体处理库

纯 Go 标准库驱动的本地媒体实验室，覆盖竞技协作游戏在地铁、电梯与热点切换
场景下需要的四类能力：**清单解析、媒体索引、传输恢复、音画同步**。
不执行任何实际音视频解码，不依赖外部服务，所有样例数据均由 Go 代码生成。

## 技术栈

- Go 1.25.6（`go.mod` 声明 `go 1.25.6`）
- `github.com/abema/go-mp4 v1.7.4`：fMP4 box 解析
- `github.com/pion/rtp v1.10.6`：RTP 包结构
- 标准库 `context`、`testing`、`container/heap`、`math/rand/v2`

> 关于 toolchain：当 `toolchain` 与 `go` 指令版本完全相同时，`go mod tidy`
> （Go 官方工具链，`GOTOOLCHAIN=local` 与 `auto` 行为一致）会把该冗余指令
> 规范化移除。因此工具链固定通过 `go 1.25.6` 表达；如需显式 toolchain 行，
> 可执行 `go mod edit -toolchain=go1.25.6`，但下一次 `go mod tidy` 仍会剥离。

## 模块布局

| 包 | 职责 |
|---|---|
| `internal/media` | 统一 90 kHz 媒体时钟、epoch、带错误码/可恢复性/片段号/序列号的 `Fault`、硬上限 |
| `internal/manifest` | DASH MPD 与含 discontinuity 的 HLS 播放列表解析/渲染、字段级往返比较 |
| `internal/bmff` | 确定性 fMP4 分片写入与 go-mp4 解析，关键帧索引，越界/截断/溢出拒绝 |
| `internal/transport` | 确定性网络轨迹、RTP 去重/乱序整理、有界抖动缓冲、期限内重传与关键帧跳转 |
| `internal/sync` | 48 kHz 音频与 90 kHz 视频时钟映射，漂移 p50/p95/max 与阈值判定 |
| `internal/pipeline` | 有界工作池与按 epoch/片段/序号归并的确定性合并器，context 全链路取消 |
| `internal/fixture` | 三码率清单、8 个对齐 fMP4 分片、四类损坏样例与网络轨迹生成 |
| `internal/lab` | 端到端编排，汇总全部验收指标 |
| `cmd/avlab` | 终端入口，仅做参数解析与结果输出 |

## 硬上限

| 项 | 上限 |
|---|---|
| 清单大小 | 1 MiB（`media.MaxManifestBytes`） |
| 单片段大小 | 8 MiB（`media.MaxSegmentBytes`） |
| 工作池 | 32（`media.MaxWorkers`） |
| 队列深度 | 4096（`media.MaxQueue`） |
| 表示层 / 片段 / 抖动包 | 16 / 1024 / 8192 |

## 演示

```bash
go version
go mod tidy && go build ./... && go test -race ./...
go run ./cmd/avlab verify --seed 17 --workers 4 --queue 64 \
  --packets 4096 --repeat 3
go run ./cmd/avlab traces --seed 17 --packets 4096
```

`verify` 输出字段：

```
roundtrip_equal=2/2
keyframe_hit=8/8
segment_sha256_match=8/8
damaged_rejected=4/4
burst_recovered=16/20
deadline_skip=4
av_drift_p95_ms=14.042
max_inflight=4
queue_peak<=64
repeat_hash_equal=true
cancel_workers=0
status=pass
```

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 指标全部通过 |
| 2 | 参数或输入无效（含超限、损坏清单/分片） |
| 3 | 指标未达阈值 |
| 130 | 主动取消（SIGINT/SIGTERM），队列关闭且 worker 全部退出 |

## 确定性保证

- 所有随机源使用 `math/rand/v2` 的 PCG，种子固定后轨迹、分片字节与漂移序列逐位复现。
- 工作完成顺序不影响结果：合并器按 `(epoch, segment, seq)` 堆排序，
  重复运行的逐片段 SHA-256 与归并摘要一致。
- HLS discontinuity 开启新 epoch；旧 epoch 的迟到包在重排序阶段即被拒绝，
  不会污染新时间线。
