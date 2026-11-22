# media-check

区域电视台多轨交付包的本地批量校验与再封装工具。统一音视频与定时元数据
（字幕、章节、ID3、emsg、SEI）的时基，核对 MP4 box 结构、样本时长、关键帧
间隔与 RTP 丢包/乱序，把时间戳异常、关键帧违规和局部码流损坏关联到同一条
毫秒时间线，并在有界内存下完成再封装。纯本地、无外部服务、合成样例全部由
代码生成。

## 技术栈与依赖

- Go 1.23（在 `go.mod` 固定 `go 1.23`），支持 macOS arm64 / amd64
- `github.com/abema/go-mp4 v1.3.0`：MP4 结构解析
- `github.com/pion/rtp v1.8.11`：RTP 报文解析

## 构建、测试与演示

```sh
go mod tidy
go build ./...
go test ./...
go run ./cmd/media-check --generate --profile broadcast \
  --loss-burst 12 --slow-sink-ms 20 --memory-limit-mib 32
```

## 模块划分

| 包 | 职责 |
| --- | --- |
| `internal/model` | 带轨道、时间范围与恢复状态的故障 / 区段模型、退出码 |
| `internal/clock` | 多采样时钟统一到毫秒；负时长、溢出、32 位 RTP 回绕 |
| `internal/ingest` | 有界读取，超限输入被拒绝而非无界缓存 |
| `internal/rtp` | 序号去重、窗口内乱序按序交付、溢出记缺口、迟到包不改写已提交时间线 |
| `internal/inspect` | MP4 box 层级 / 截断检测、样本时长、关键帧间隔（含前后边界） |
| `internal/metadata` | 字幕 / 章节 / ID3 / emsg / SEI 对齐，同刻稳定排序，孤儿事件保留为错误 |
| `internal/repair` | 故障合并为区段，决定保留（recovered）或隔离（quarantined） |
| `internal/stream` | 队列数 + 字节双预算，慢消费者反向背压，取消时释放并报告提交边界 |
| `internal/remux` | 再封装并写入显式 GAP 标记（不静默掩盖缺口），输出确定性摘要 |
| `internal/fixture` | 正常、四类损坏与边界合成样例生成器 |
| `internal/app` | 连接各子系统的端到端编排 |
| `cmd/media-check` | 命令行入口 |

## 故障 profile 与退出码

`--profile` 可选 `normal | broadcast | truncated | corrupt | no_keyframe |
drift | boundary`。四类可复现故障：截断 box、损坏样本、丢失关键帧、跨轨时钟
漂移；损坏区段隔离后，后续有效区段仍被处理。

| 退出码 | 含义 |
| --- | --- |
| 0 | 完成且无未恢复错误（如 `normal`） |
| 1 | 存在已报告的损坏区段（如 `broadcast` 的硬丢失、各损坏 profile） |
| 2 | 参数或输入结构无效（未知 profile、内存上限非正等） |
| 3 | 内部处理失败（如输出写入失败） |

## 输出字段契约

摘要以 `字段名 值` 逐行输出，重复运行结果一致：

- `av_sync_max_ms`：正常样例 ≤ 20
- `loss_recovery_rate`：可恢复丢包 ≥ 0.90
- `latency_p95_ms`：本地合成样例 ≤ 500
- `peak_buffer_mib`：≤ 内存上限（默认 32 MiB）
- `fault_segments` / `committed_segments`：故障区段与已提交区段数

再封装输出以 `free` box 写入 `GAP track=… [start,end) reason=…` 标记，原始
缺口始终可追溯，不会被静默掩盖。
