# mediamix

本地 RTP/RTCP 音频汇聚与规范化流水线。把公网到达的实时音频包流整理为统一规格
（48 kHz、立体声、−18 LUFS、真峰值 ≤ −1 dBTP），并核对与视频时间线的同步，
在突发丢包、乱序、慢下游和处理中断后保证可恢复、可重放、可校验。

全程纯本地运行，无任何外部服务依赖；所有 RTP/RTCP 样例均由程序确定性生成。

## 环境要求

- macOS（Apple Silicon 与 Intel 均可），Go 1.25.6、clang、Homebrew
- 原生依赖：FFmpeg 9（通过 go-astiav v0.43.0 绑定）、pkg-config

```bash
brew install ffmpeg pkg-config
export PKG_CONFIG_PATH="$(brew --prefix ffmpeg)/lib/pkgconfig"
go mod tidy
go build ./...
go test ./...
```

或直接执行 `./scripts/setup.sh`。

## 验收演示

```bash
./scripts/demo.sh
# 等价于
go run ./cmd/mediamix demo \
  --case all --seed 41 --seconds 12 \
  --loss 0.02 --reorder 0.05 \
  --queue-mib 8 --slow-consumer-ms 2 \
  --fault after-normalize
```

`--case all` 依次运行三条路径并逐路径输出指标：

| case | 含义 |
| --- | --- |
| `normal` | 无故障基线 |
| `corrupt` | 注入故障触发回滚，模拟损坏/中断后恢复 |
| `interrupt` | 提交前中断，重放到最近提交边界 |

输出字段：`case`、`av_sync_ms`、`loss_recovery_pct`、`p95_latency_ms`、
`queue_bytes_peak`、`output_sha256`。

验收门槛与退出码：

| 指标 | 门槛 |
| --- | --- |
| A/V 同步误差 | ≤ 5 ms |
| 丢包恢复率 | ≥ 95% |
| P95 延迟 | ≤ 250 ms |
| 队列峰值 | ≤ 8388608 字节（8 MiB） |
| 恢复哈希 | 与正常路径一致 |

- `0`：指标与恢复校验全部通过
- `2`：输入格式/参数错误
- `3`：回滚或恢复失败
- `4`：指标超限

> 说明：`go run` 会把程序的非零退出码包装为 1 并打印 `exit status N`；
> 验收以编译后二进制（`scripts/demo.sh` 使用 `bin/mediamix`）的真实退出码为准。

## 架构与数据流

```
RTP/RTCP 夹具
   │  internal/rtp   解析、重排、丢包/回绕识别、SR 时钟校准
   ▼
90kHz 统一时钟 (internal/clock)  +  可恢复错误模型（阶段/序号/可恢复标记）
   │
   ▼
有界队列 (internal/buffer)  字节预算背压，取消/关闭释放全部等待者
   │
   ▼  internal/audio
   │  convert  重采样+声道归一（片段边界不丢样不重复）
   │  plc      确定性丢包补偿（截断尾部显式失败，不伪装成功）
   │  loudness loudnorm -18 LUFS + alimiter -1 dBTP
   ▼
WAL + 原子提交 (internal/txn)  CRC 校验、损坏尾部截断、回滚、重放
   │
   ▼
指标 (internal/metrics)  同步 / 恢复率 / P95 / 队列峰值
```

## 模块

- `internal/clock`：90 kHz 媒体时钟、16/32 位回绕运算、RTCP 仿射时间线、错误类型
- `internal/rtp`：RTP/RTCP 解析与构造、乱序重排、丢包/重复/回绕识别、确定性夹具
- `internal/buffer`：泛型环形有界队列，按条目与字节双限额，支持背压
- `internal/audio`：swresample 重采样、确定性 PLC、loudnorm/alimiter 响度控制
- `internal/txn`：CRC32 WAL、同文件系统原子 rename 提交、回滚与重放校验
- `internal/pipeline`：阶段编排、故障注入、隔离目录干净重放
- `internal/metrics`：验收指标计算与阈值判定
- `cmd/mediamix`：终端验收入口

## 约束与边界

- 单条轨道时长上限 10 分钟，超限返回可定位错误
- 队列内存硬上限 8 MiB，超限参数直接拒绝
- 拒绝非法包长、超限队列参数与 `..` 穿越路径，不留下可被误认的完整交付物
- 损坏 WAL 尾部仅截去未提交记录；已提交清单与 WAL 双向哈希校验
- 仅交付源码、测试与脚本，不含构建产物、密钥或外部样本
