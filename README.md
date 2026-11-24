# mediaroute

区域直播分发链路的本地验收工具。它在纯本地环境生成四类 MP4 样例、建立时间线索引、按索引流式处理音视频、模拟 RTP 丢包与乱序恢复，并输出可独立复核的 JSON 指标。

## 环境

- macOS / Darwin
- Go 1.25.6（`go.mod` 的 `go` 指令固定为 `1.25.6`）
- 依赖固定版本：`github.com/abema/go-mp4 v1.3.0`、`github.com/pion/rtp v1.8.19`
- 无外部服务、无在线媒体源；所有夹具由 Go 代码生成

## 快速开始

```sh
go mod tidy
go build ./...
go test ./...

# 主验收命令
go run ./cmd/mediaroute demo --seed 42 --loss-burst 12 --truncate-tail 4096 --json

# 重跑稳定性：两次产物摘要一致，index_rebuilt 为假
go run ./cmd/mediaroute demo --seed 42 --repeat 2 --json

# 一键验收（构建二进制以观测真实退出码）
./scripts/acceptance.sh
```

## 架构

| 模块 | 职责 |
| --- | --- |
| `internal/clock/timeline.go` | 精确有理数时基、非零起点、往返误差 ≤ 1 ms、溢出检查 |
| `internal/errors/model.go` | 带轨道、字节偏移、可恢复性的统一错误模型与退出码映射 |
| `internal/manifest/read.go` | 清单读取与轨道引用校验，失败时报告字节偏移 |
| `internal/scan/mp4.go` | 样本表、编辑列表、稀疏关键帧扫描；截断/越界样本隔离且保留有效前缀 |
| `internal/index/store.go` | 以输入摘要、扫描器版本、清单摘要为键的索引缓存；原子替换、校验和与陈旧检测 |
| `internal/audio/convert.go` | 重采样至 48 kHz、声道布局归一（含明示回退）、滑窗响度 −16 LUFS、真峰值 ≤ −1 dBTP |
| `internal/rtp/recover.go` | 扩展序列号、乱序窗口恢复、重复不计入恢复、窗口外丢包计数 |
| `internal/stream/run.go` | 按索引分块流式读取（内存不随时长增长）、损坏区间隔离、音频处理与 RTP 调度 |
| `internal/metrics/report.go` | 同步偏差、恢复率、延迟分位数等指标汇总 |
| `internal/fixtures` | 纯 Go 生成的 MP4 夹具及四类故障变体 |
| `internal/demo` | 端到端编排、故障边界校验、JSON/表格输出 |
| `cmd/mediaroute` | 终端入口：`demo`、`scan`、`verify`、`version` |

扫描子系统读取清单和媒体字节生成时间线索引；处理子系统消费索引并调用音频转换与 RTP 恢复；度量子系统接收处理事件并输出可独立复核的指标。时间换算与错误定位由扫描和处理模块共用。

## 四类自生成样例

| 变体 | 故障 | 预期 |
| --- | --- | --- |
| `normal` | 无 | `corrupt_ranges = 0`，退出码 0 |
| `truncated` | 尾部截断 | 保留截断前的有效索引，截断样本进入 `corrupt_ranges`，退出码 2 |
| `out-of-range` | 样本偏移越过 EOF | 定位到具体样本与字节偏移，退出码 2 |
| `missing-pages` | 样本页被清零 | 识别为零页并隔离，退出码 2 |

每个故障变体同时校验扫描索引与流式执行的边界一致（`stream_boundary_ok`）。

## 指标与阈值

`demo --json` 至少输出：

- `sync_error_ms_p95` ≤ 20
- `packet_recovery_rate` ≥ 0.95（窗口内乱序包）
- `latency_ms_p95` ≤ 250（本机生成样例）
- `indexed_samples`、`corrupt_ranges`、`index_rebuilt`

此外输出音频响度/真峰值、布局回退原因、逐样本同步明细（源时间、编辑映射、输出时间）及故障变体校验结果。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功且指标达标 |
| 1 | 清单或配置非法 |
| 2 | 输入可部分恢复但存在损坏 |
| 3 | 未处理的内部错误 |

> 注意：`go run` 会把任意非零状态折叠为 1。需要观测真实退出码时请 `go build` 后运行二进制，或使用 `scripts/acceptance.sh`。

## 验证缓存失效

缓存键包含完整输入摘要。修改生成素材后应触发重建：

```sh
dir=$(mktemp -d)
go run ./cmd/mediaroute demo --seed 42 --dir "$dir" --quiet
printf '\xff' | dd of="$dir/normal-video.mp4" bs=1 seek=$(( $(stat -f%z "$dir/normal-video.mp4") - 1 )) conv=notrunc 2>/dev/null
go run ./cmd/mediaroute demo --seed 42 --dir "$dir" --reuse --quiet --json | grep index_rebuilt
# 期望 "index_rebuilt": true
```

## 大时长与内存

`--duration-scale` 可放大样例时长。实测在素材时长放大 5× 到 40× 时，进程峰值 RSS 稳定在约 17–20 MB，处理的样本数从 1035 增长到 8280；`go test ./internal/demo -run TestLiveHeapBounded` 会在一次运行后回收并断言存活堆不随时长增长。

## 约束与边界

- 仅支持本地 MP4 输入与 RTP 包序列模拟，不接入真实跨区网络。
- 扫描与处理均可取消（`context`，`Ctrl-C` / `--timeout`）。
- 缓存原子写入、限制单文件大小，拒绝越界偏移、整数溢出与超限轨道数（`MaxTrackCount = 32`）及样本数（`MaxSamplesPerTrack = 500000`）。
- 示例夹具全部由 Go 代码生成，交付仅含源码、测试与脚本。
