# mediapipeline

纯本地、确定性的实时媒体管线示例：把自生成的 RTP/RTCP 流规整为协商规格，
在丢包、乱序与慢消费者下维持有界内存与可重复的量化结果。无网络、无设备、
无图形界面依赖，仅运行于 macOS 本机与 Go 1.23+。

## 依赖说明

- `github.com/pion/rtp v1.8.11`：RTP 包结构解析与序列化（按规格指定）。
- `github.com/abema/go-mp4`：MP4 容器写入与往返解析。
  规格要求的 `v1.29.0` 在该模块上游不存在（其最新发布标签为 `v1.7.4`，
  已通过模块代理与 GitHub 标签双重确认），因此采用最接近且本机已缓存的
  **`v1.7.4`**，所用 `StartBox/EndBox/Marshal/ReadBoxStructure` API 在该版本稳定。

## 构建与运行

```sh
go mod download
go build -o /tmp/mediapipeline ./cmd/mediapipeline

# 弱网演示（固定种子，输出可重复）
go run ./cmd/mediapipeline demo \
  --seed 42 --loss 0.03 --reorder-window 64 \
  --max-buffer-bytes 1048576 --consumer-delay-ms 8 --segment-ms 500

# 汇总验证：可重复性、往返一致率、关键帧命中率、边界与故障定位
go run ./cmd/mediapipeline verify --seed 42

# 全量测试（testdata 包需显式路径，因 Go 的 ./... 会跳过 testdata 目录）
go test ./... -count=1
go test ./internal/testdata/ -count=1
```

## 退出码

| 码 | 含义 |
|----|------|
| 0  | 全部量化断言通过 |
| 1  | 量化断言失败（一致率/命中率/缓冲水位） |
| 2  | 参数或输入无效 |
| 3  | 处理被取消或发生不可恢复错误 |

## 模块划分

```
cmd/mediapipeline   CLI 入口（demo / verify）与退出码映射
internal/clock      纳秒统一时间基准，32 位时间戳与 16 位序号的 64 位扩展
internal/mediaerr   携带轨道/序号/时间戳/阶段的结构化错误模型
internal/stream     RTP/RTCP 校验、序号回绕/丢包/重复/乱序跟踪
internal/reorder    有界重排缓冲、迟到与冲突隔离、缺口声明
internal/audio      跨块重采样、声道布局归一化、窗口响度与峰值限制
internal/pipeline   背压/取消/双重内存上限的三阶段运行器与集成测试
internal/index      关键帧索引构建与越界校验（零虚假命中）
internal/container  go-mp4 写入、stco 回填与往返解析、截断检测
internal/metrics    量化摘要与阈值断言
internal/testdata   固定种子夹具生成（正常/损坏/边界流）
```

## 关键设计

- **统一时钟**：所有模块共用纳秒轴；异常时间戳返回结构化错误，绝不静默
  改写为连续时间。
- **双重内存上限**：队列同时受包数（channel 容量）与字节（原子计数）约束，
  慢消费者的背压逐级传递；进程缓冲峰值不超过配置上限加 20% 固定开销。
- **部分失败隔离**：截断包、坏 RTCP、重复负载冲突只影响对应片段，后续有效
  片段继续处理并在摘要中以 `partial_failures` 体现。
- **可重复性**：负载由种子 + splitmix 派生，固定种子下逐片段 SHA-256 稳定。
- **关键帧索引**：仅依据显式关键帧标记建索引，越界采样号/偏移被拒绝，
  无关键帧流省略 stss 而非伪造命中。
