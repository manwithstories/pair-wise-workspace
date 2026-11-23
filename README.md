# mediakernel

实时语音战术游戏的可复用媒体处理内核：从 Matroska/WebM 录制素材生成
可传输的分包与时间线，量化乱序、抖动、突发丢包后的恢复质量，
支持任务中断续跑与结果可追溯。纯本地运行，无外部服务依赖。

## 构建与验收

```sh
go mod tidy
go build ./...
go test ./...
go run ./cmd/mediaprobe demo --seed 17 --loss-burst 12 --bandwidth-kbps 420 --resume
```

一键复核（输出与退出码）：

```sh
./scripts/verify.sh
```

退出码约定：`0`=全部校验通过，`1`=处理失败，`2`=输入损坏，`3`=缓存或检查点不可信。
注意：`go run` 不透传非零退出码，验收退出码时请使用
`go build -o mediaprobe ./cmd/mediaprobe` 构建后的二进制（verify.sh 已内置）。

## 架构

数据流：EBML 读取器 → 轨道解析器 → 统一时钟；RTP 缓冲与恢复共享时钟
和错误分类 → 音画同步策略；流水线协调缓存、检查点和清单写入。

| 模块 | 职责 |
| --- | --- |
| `internal/ebml` | 流式读取未知长度 Segment/Cluster；截断报告字节偏移与可恢复边界；拒绝越界元素长度 |
| `internal/matroska` | 解析轨道与多轨交错块；负时间戳、非单调块进入统一错误模型，不静默改写 |
| `internal/clock` | 统一纳秒媒体时钟；32 位时间戳回绕扩展；统一错误分类 |
| `internal/rtp` | 抖动缓冲（按网络采样自适应延迟上限，过期计迟到丢弃，乱序仅窗口内重排）；突发丢包优先重传缓存，无法恢复输出带原因的空洞记录，不伪造帧 |
| `internal/sync` | 依据关键帧边界与音频时钟修正漂移，输出清单起止与轨道时间基准 |
| `internal/pipeline` | 增量编排：每检查点记录输入摘要、阶段、输出摘要、来源；恢复时重验缓存对象，不符即隔离并从最近可信检查点重算；重复派发幂等 |
| `internal/cache` | 内容摘要寻址；来源校验；不符对象隔离 |
| `internal/checkpoint` | 原子保存检查点（临时文件+重命名），写失败不破坏旧状态 |
| `internal/manifest` | 输出轨道、时长与码率清单（确定性 JSON） |
| `internal/fixture` | Go 代码生成确定性 WebM 样例与网络轨迹（抖动、尖峰、突发丢包） |
| `cmd/mediaprobe` | 演示入口 `demo` 与 MP4 结构辅助 `mp4boxes` |

## 演示输出

`demo` 打印 `key=value` 摘要：`source_sha256`、`recovered_packets`、
`lost_packets`、`late_packets`、`target_kbps`、`actual_kbps`、
`bitrate_error_pct`、`quality_psnr_db`、`manifest_start_ms`、
`timeline_error_ms`、`cache_rejected`、`checkpoint_resumed`。

- 成功样例：`timeline_error_ms ≤ 20`，`bitrate_error_pct ≤ 8`，重复运行摘要一致；
- 截断输入：打印 `error_offset`，退出码 2；
- 缓存/检查点篡改：`cache_rejected=1`，对象隔离并重算，退出码 3；
- 续跑：`checkpoint_resumed=1`。

工作目录（默认 `.mediaprobe/`）：`input.webm`（样例素材）、`cache/`
（内容寻址对象与 `quarantine/` 隔离区）、`checkpoint.json`、`manifest.json`。

## 约束

仅处理本地自生成及用户提供的 Matroska/WebM 夹具；不联网、不采集设备
数据。单元素上限 64 MiB，乱序重排窗口 32 包，重传缓存每轨 8 包，
样例时长 2s，均可在代码常量中调整。

## 依赖偏差说明

任务书要求 `go-mp4 v1.29.0`，但 `github.com/abema/go-mp4` 不存在该版本
（最新为 v1.7.4），故固定为 `v1.7.4`，仅用于 `mp4boxes` 媒体结构解析辅助。
`github.com/pion/rtp` 按要求固定 `v1.8.11`。
