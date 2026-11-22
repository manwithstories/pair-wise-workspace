# mediacore

竞技游戏低延迟互动音视频的本地媒体核心：从 HLS/DASH 清单与 fMP4 分片中切出可复用片段、标记静音区间、去除跨轨重复内容，并在任务中断或素材异常后判断可恢复范围、追溯每个输出的来源。纯终端形态，无外部服务依赖。

## 构建与测试

```sh
go mod tidy
go build ./...
go test ./...
```

## 演示

```sh
go run ./cmd/mediacore demo --seed 42 --resume-dir .demo-state --report stdout
# 再跑一次：摘要完全一致，重复提交数为 0
go run ./cmd/mediacore demo --seed 42 --resume-dir .demo-state --report stdout
# 损坏检查点恢复：输出 error_code / recoverable / recovered_nodes
go run ./cmd/mediacore demo --seed 42 --resume-dir /tmp/mc-corrupt --scenario corrupt
# 输入质量失败（负时长清单）：退出码 2
go run ./cmd/mediacore demo --seed 42 --resume-dir /tmp/mc-bad --scenario bad-input
```

退出码：0 成功；2 输入质量失败；3 不可恢复状态或 I/O 错误（`go run` 会把非零码包装成 1，请用 `go build` 后的二进制核验退出码）。

报告字段：`target_bitrate_kbps`、`actual_bitrate_kbps`、`bitrate_deviation_pct`（≤5.0）、`silence_segments`、`duplicate_segments`、`quality_samples`、`manifest_alignment_ms`（≤40）、`recovered_nodes`、`first_missing_dependency`、`duplicate_commits`。

## 架构

| 模块 | 职责 |
| --- | --- |
| `internal/clock` | 统一有理数时间刻度（溢出检查）与共享错误模型 |
| `internal/manifest` | HLS EXTINF 与 DASH SegmentTemplate/SegmentTimeline 归一；拒绝负时长、非法基准、路径逃逸、超限清单、不支持编解码 |
| `internal/timeline` | 显式时间线、跨清单对齐、流水线依赖图拓扑排序 |
| `internal/media` | fMP4 分片读写（go-mp4）、截断检测、RTP 时间戳解卷（pion/rtp） |
| `internal/segment` | 按关键帧规则切分区间并报告边界偏差 |
| `internal/audio` | 流式静音检测：可配置窗口/阈值，短于窗口的尾段不误报 |
| `internal/fingerprint` | 跨轨窗口指纹：保留最早来源、哈希碰撞二次校验、静音豁免 |
| `internal/store` | 幂等键检查点，临时文件 + fsync + rename 原子提交 |
| `internal/provenance` | 来源清单摘要、分片范围、时间基准、算法版本、错误链 |
| `internal/pipeline` | 依赖调度；损坏检查点回滚到最后已提交节点并记录恢复位置 |
| `internal/fixture` | 自生成样例：正常、截断、重复、清单缺项、时间戳溢出 |
| `internal/demo` | 演示流水线节点与报告装配 |
| `cmd/mediacore` | 参数入口 |

## 关键语义

- **码率**：按有效媒体时长统计，清单空洞与截断分片被排除（缺片不会伪装成低码率），同时输出目标偏差与样本数。
- **幂等**：检查点键 = 输入摘要 + 参数摘要 + 模块版本；重复执行不重复写结果，参数变化产生新结果。
- **恢复**：只接受校验完整的原子结果；损坏检查点回滚受影响子图，记录首个缺失依赖与恢复位置。
- **溯源**：每个输出记录源清单摘要、分片范围、时间基准、算法版本与错误链；同一输入与参数重跑产生相同摘要（记录不含墙钟字段）。
- **有界内存**：静音与指纹按窗口流式处理，内存随并发轨道数与窗口大小有界。
