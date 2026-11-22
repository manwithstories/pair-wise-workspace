# fieldcut

巡演现场后期分片处理系统：在离线、无外部网络依赖的前提下，汇总舞台、
侧台与观众区机位的 HLS/DASH 分片与 RTP 码流，修复时间戳、重排丢包、
重建可随机访问的确定性索引。纯 Go 1.23 实现，终端形态，无图形界面。

## 模块结构

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 统一 90kHz 时间基准、32/33 位回绕、线性漂移拟合、共享错误类型与硬上限 |
| `internal/manifest` | HLS 媒体清单与 DASH SegmentTimeline 解析，路径越界/序号冲突/超限拒绝 |
| `internal/ingest` | RTP 序号窗口重排与丢包恢复（pion/rtp）、fMP4 box 结构校验（go-mp4） |
| `internal/timeline` | 跨轨事件水位提交、时间戳修复、回看窗口内确定性回算与窗口外隔离 |
| `internal/stream` | 字节数+在途数双限制有界队列、背压传播、取消释放与管线编排 |
| `internal/index` | 规范化 sidx 随机访问索引、SHA256 摘要、关键帧查找与损坏隔离 |
| `internal/fixtures` | 固定 seed 自生成正常/丢片/重复/乱序迟到/回绕/截断 box 样例 |
| `internal/demo` | 把各库编排为一条确定性离线管线（适配器，核心库不依赖它） |
| `cmd/fieldcut` | 终端入口 `fieldcut demo` |

数据流向：`fixtures → manifest/ingest（共用 clock）→ timeline → index`，
各阶段之间由 `stream` 有界队列连接；慢消费者只会阻塞上游，不会无限缓存。

## 不变量与硬约束

- 任何输入的内存占用都受参数硬约束：队列字节数、在途分片数、清单条目数、
  轨道数、回看窗口（上限 300s）、单片大小（64MiB）均有封顶。
- 无法唯一推断的时间戳区段标记 `untrusted`，不静默改写；不可恢复的丢包
  保留时间缺口（gap 事件），绝不伪造帧。
- 索引按 `(Start, TrackID, Sequence)` 规范化并重算连续偏移，同一输入
  重跑产生相同 `index_sha256`。
- 拒绝一切远程素材（`://` scheme、绝对路径、反斜杠、`..` 越界）。

## 构建与测试

依赖版本固定在 `go.mod`：`go-mp4 v1.4.1`、`pion/rtp v1.10.6`。

> 工具链说明：pion/rtp v1.10.6 自身的 go.mod 声明 `go 1.24.0`，而本项目
> 按要求保持 `go 1.23` 指令。Go 1.25 工具链在默认只读模式会要求重写
> go.mod，因此构建/测试统一加 `-mod=mod`。Makefile 已封装：

```bash
make tidy     # go mod tidy 后自动回写 go 1.23 指令
make build    # go build -mod=mod ./...
make test     # go test  -mod=mod ./...
make vet
```

直接使用 go 命令时：

```bash
go mod tidy && go mod edit -go=1.23
go build -mod=mod ./...
go test  -mod=mod ./...
```

## 演示

```bash
go run -mod=mod ./cmd/fieldcut demo \
  --seed 41 --duration 300s --queue-bytes 8388608 \
  --late-window 4s --inject-loss 0.02 --slow-consumer-ms 25
```

输出指标及验收阈值：

| 指标 | 阈值 |
| --- | --- |
| `av_sync_ms_p95` | ≤ 20 ms |
| `packet_recovery_rate` | ≥ 0.95 |
| `pipeline_latency_p95_ms` | ≤ 500 ms |
| `peak_buffer_bytes` | ≤ `--queue-bytes` |
| `index_seek_misses` | 0 |
| `index_sha256` | 固定输入重跑不变 |

故障场景：

```bash
go run -mod=mod ./cmd/fieldcut demo --case truncated-box --seed 41
go run -mod=mod ./cmd/fieldcut demo --case late-outside-window --seed 41 --late-window 1s
# 另支持 loss / duplicate / late-out-of-order / timestamp-wrap
```

退出码：`0` 产物可核验；`2` 输入或参数错误；`1` 处理或不变量失败
（注意 `go run` 会把非零退出统一显示为 1，验收退出码请用 `go build`
产出的二进制）。

## 确定性

所有随机数来自 `math/rand.NewSource(seed)`；时间线排序、索引规范化、
sidx 定长大端编码均与到达顺序无关。`internal/demo` 的重跑测试断言
两次 `Run` 的摘要与计数器完全一致。
