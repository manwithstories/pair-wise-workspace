# avcheck — 低延迟多轨互动音频离线验收流水线

面向移动网络（地铁、热点切换、弱覆盖）下多人协作排练场景的**纯本地、确定性**
音频处理与验收工具。它在统一采样时间线上处理多声道 PCM/WAV 采样块，模拟
重复、乱序、突发丢包、超窗迟到和持续带宽变化，输出可复现的逐帧哈希与 MP4 摘要。

无设备采集、无网络服务、无图形界面；所有样例由代码按 seed 生成。

## 技术栈

- Go 1.23（在 Darwin arm64 / amd64 验证）
- `github.com/Eyevinn/mp4ff v0.47.0`：分片 MP4 时间线封装
- `github.com/pion/rtp v1.8.11`：RTP 序号与包解析
- 标准库 `testing` / `context` / `io` / `hash/fnv`

## 目录结构

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 统一采样刻度（48 kHz）、`Block` 类型、可分类错误模型 |
| `internal/wav` | 16/24 位、1–8 声道 PCM/WAV 解析与生成，字节偏移定位 |
| `internal/ingest` | RTP 接入，32 位时间戳回绕映射到 64 位统一刻度 |
| `internal/jitter` | 有界重排窗口，RFC 3550 序号距离、去重、迟到分类 |
| `internal/recovery` | 连续 ≤3 包线性插值恢复，超长缺口静音，保持采样总数 |
| `internal/flow` | 字节上限背压队列、阻塞写、2 秒 EWMA 与 20–200 ms 自适应窗口 |
| `internal/timeline` | 多轨混合、削波、30 fps 帧对齐、1 采样同步容差 |
| `internal/render` | 离屏逐帧 FNV-1a 哈希、每轨样本数、MP4 时间戳摘要 |
| `internal/export` | mp4ff 分片 MP4（fMP4）封装与回读 |
| `internal/metrics` | 同步误差、恢复率、P95 延迟、队列峰值汇总与达标判定 |
| `internal/fixture` | 六类确定性样例生成 |
| `internal/pipeline` | 端到端确定性编排与排空-重试背压 |
| `cmd/avcheck` | 终端验收适配器（退出码 0/1/2） |

## 构建与测试

```sh
go mod tidy
go build ./...
go test ./...
```

便捷目标：`make build` / `make test` / `make race` / `make accept`。

## 验收运行

```sh
go run ./cmd/avcheck \
  -duration 10s -tracks 3 -seed 7 \
  -loss-burst 3 -reorder 8 -duplicate 5 \
  -queue-bytes 262144 -out /tmp/avcheck.mp4
```

输出（示例）：

```
sync_error_samples=0 loss_recovery_rate=1.000 p95_latency_ms=0.000 \
peak_queue_bytes=259200 frame_count=300 frame_hash=0x...
```

达标线：同步误差 ≤1 采样、恢复率 ≥90%、P95 延迟 ≤200 ms、峰值队列 ≤ 队列上限。

命名夹具：

```sh
go run ./cmd/avcheck -fixture corrupt-wav
# error=truncated_wav offset=40   (退出码 1)

go run ./cmd/avcheck -fixture queue-boundary
# backpressure_events=... accepted_blocks=... lost_blocks=0   (退出码 0)
```

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 指标达标 |
| 1 | 输入损坏（如截断 WAV）或资源边界违规（队列超限、丢块） |
| 2 | 命令参数错误 |

## 确定性保证

整条流水线在单 goroutine 内以"排空-重试"模型驱动背压，避免真实调度时序；
逐帧哈希只依赖采样字节与固定参数。相同输入与参数重复运行得到相同
`frame_hash` 与逐字节相同的 MP4（由 `TestDeterministicSummaryAcrossRuns`、
`TestDeterministicBytes` 守护）。
