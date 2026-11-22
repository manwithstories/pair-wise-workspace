# mediasim — 可复用实时媒体处理内核

面向竞技训练平台弱网共同观看场景的纯本地媒体处理内核。它在**同一条统一媒体时间线**
上提供抖动缓冲、丢包恢复与音画同步，并用可复现的网络轨迹稳定重现重复、乱序、突发
丢包与带宽波动，不依赖任何真实设备或在线服务。

## 运行环境

- Go 1.23（macOS arm64 / amd64）
- 依赖：`github.com/abema/go-mp4 v1.3.0`、`github.com/pion/rtp v1.8.11`

```bash
brew install go@1.23
export PATH="$(brew --prefix go@1.23)/bin:$PATH"
go mod tidy
go build ./...
go test ./...
go run ./cmd/mediasim -scenario burst -seed 41 -report stdout
```

## 模块结构

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 48kHz 统一采样时钟与视频时间刻度、RTP 时钟换算 |
| `internal/model` | 可分类/可追踪媒体错误、事件、音频块、对齐帧与汇总指标 |
| `internal/wav` | WAV 头解析、PCM 位深/采样率/声道/块对齐校验与测试夹具生成 |
| `internal/rtp` | RTP 包头序列化/校验解析、同流去重、同序号冲突隔离、丢包间隙检测 |
| `internal/jitter` | 按事件时间排序的有界抖动窗口，超容量丢弃过期事件 |
| `internal/timeline` | 水位推进封存稳定区间，固定窗口对迟到事件确定性回溯重算 |
| `internal/recovery` | 短丢包静音填补、突发丢包邻域插值、不可恢复采样标记 |
| `internal/sync` | 视频时间戳缺失/回退/跨边界降级策略与音画漂移约束 |
| `internal/bandwidth` | EWMA 带宽估计与有界自适应缓冲目标 |
| `internal/mp4clip` | 基于 go-mp4 的短片段生成与逐采样时间戳核验 |
| `internal/sim` | 同 `(scenario, seed)` 可复现的网络轨迹与故障视频夹具 |
| `internal/pipeline` | 串联全部模块的确定性处理引擎，输出指标与结果哈希 |
| `cmd/mediasim` | 演示入口：生成输入、执行流水线、打印指标 |

## 数据流

```
WAV 解码器 ┐
           ├─ 统一时钟事件 ─ 抖动窗口 ─ 水位/回溯 ─ PCM 恢复 ─┐
RTP 解析器 ┘                                                  ├─ 音画对齐 ─ 指标/哈希
带宽估计器 ───────────── 目标缓冲参数 ───────────────────────┘
互动端视频帧（带事件时间戳，含故障注入）──────────────────────┘
```

## 场景与退出码

- `nominal`：干净网络，门限 `av_sync_ms_p95 ≤ 20`、`latency_ms_p95 ≤ 180`
- `corrupt`：重复/乱序/孤立与突发丢包，必须报告非零不可恢复采样
- `recovery`：可括号重建的丢包，`loss_recovery_rate ≥ 0.8`
- `burst`：拥塞综合压力场景，无固定指标门限

退出码：成功 `0`；输入损坏或指标越界 `2`；参数错误 `64`。

## 确定性

同一 `(scenario, seed)` 下轨迹逐动作一致，流水线无随机源，最终 `result_hash`
跨进程、跨重复运行完全相同。所有缓冲（抖动窗口、故障日志、回放历史、RTP 去重窗口）
均有硬容量上限；所有外部长度字段先校验后分配，防止恶意长度触发大额分配。
