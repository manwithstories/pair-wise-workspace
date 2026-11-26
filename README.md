# 多机位回传音视频对齐 QC

纯本地、确定性的 Go 1.25.6 终端流水线：生成弱网 RTP/Opus/AAC 样例，完成序号扩展、去重/冲突隔离、自适应抖动重排、重传恢复、90 kHz 共用时钟 AV 对齐、DAG 幂等调度和指标哈希。

## 验收命令

```bash
go mod tidy
go build ./cmd/qc
go test ./...

go run ./cmd/qc demo --profile normal --codec opus --seed 41
go run ./cmd/qc demo --profile burst  --codec aac  --seed 41 --fail-once audio.repair
go run ./cmd/qc demo --profile corrupt --seed 41
```

也可以运行：

```bash
./scripts/acceptance.sh
```

正常和恢复路径输出：

```text
packets
duplicates
recovered
recoverable_loss
recovery_rate
av_sync_p95_ms
e2e_p95_ms
first_blocked
output_sha256
```

burst 路径额外打印 `propagated_gaps` 与 `av_gap_offset_ms`，并显示：

- `attempt_failed error_type=internal`
- `blocked_dependency=audio.repair`
- `retry_success`

同一参数重复运行的 `output_sha256` 相同；`--fail-once audio.repair` 首次注入失败、第二次幂等重试成功，最终产物哈希与干净 burst 运行一致。

## 退出码

- `0`：成功且阈值通过（`recovery_rate >= 0.80`、`av_sync_p95_ms <= 20`、`e2e_p95_ms <= 180`）。
- `2`：`malformed` 或 `conflict` 等输入/损坏分类错误。
- `1`：内部错误、依赖失败或阈值未通过。

注意：`go run` 会把子进程退出码 `2` 包装成 go 工具自身的状态 `1`，同时打印 `exit status 2`。直接运行 `go build ./cmd/qc && ./qc demo --profile corrupt --seed 41` 时，二进制退出码为 `2`；CLI 测试直接验证该契约。

## 包结构

| 路径 | 职责 |
| --- | --- |
| `internal/media` | 90 kHz 时钟、时间戳回绕、Opus TOC / AAC AU 时长、统一错误模型 |
| `internal/rtp` | pion/rtp 解包、序号扩展、SSRC+序号+负载去重和冲突隔离、长度/缓存限制 |
| `internal/network` | seed 稳定的重复、乱序、回绕、截断、冲突、突发丢失和带宽骤变轨迹 |
| `internal/jitter` | 依带宽轨迹调整的有界等待窗，窗内乱序归位、窗外显式缺口 |
| `internal/recovery` | 仅用有效重传副本修复；不可恢复缺口不生成伪造音频 |
| `internal/sync` | 共用媒体时钟对齐，突发缺口传播到视频偏移统计 |
| `internal/dag` | 稳定输入哈希、JSON Journal、拓扑调度、重试和首个阻塞依赖 |
| `internal/metrics` | P95、阈值、稳定 JSON 产物和 SHA-256 |
| `internal/pipeline` | 确定性样例生成与 DAG 串接 |
| `cmd/qc` | 纯终端入口 |

## 边界限制

- 单数据报上限：`rtp.MaxPacketBytes = 1500`。
- 去重缓存上限：`rtp.MaxSeenPackets = 4096`。
- 抖动队列和重传副本均为有界缓存。
- 不访问真实采集设备、不监听端口、不依赖外部服务；所有样例均由代码和 seed 生成。
