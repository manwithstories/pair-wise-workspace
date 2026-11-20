# mediarepair

纯本地的多轨媒体时间线修复与关键帧索引工具。面向竞技对战场景：把多轨时间线、
剪辑点与转场片段整理成传输端可快速跳转的素材，并在输入损坏、下游变慢或任务中断时
定位问题、可续跑重试。全部数据由程序自生成，不采集设备音视频、不发送任何网络媒体包。

## 环境

- macOS（Darwin）Terminal
- Go 1.23 及以上
- 无外部服务依赖；MP4 容器用 `github.com/abema/go-mp4 v1.3.0`，
  RTP 时间戳/序号用 `github.com/pion/rtp v1.8.13`

## 快速开始

```sh
go test ./...

go run ./cmd/mediarepair fixture --out ./tmp
go run ./cmd/mediarepair repair --input ./tmp/timeline.mp4 \
    --report json --buffer 8 --slow-consumer-ms 20
go run ./cmd/mediarepair verify --input ./tmp/corrupt.mp4
```

`repair` 输出（文本或 JSON）包含验收字段：

```
roundtrip_match=1.0000 keyframe_hit_rate=1.0000 segment_hashes=6 \
peak_buffer_samples=8 backpressure_wait_ms=3736
```

`verify` 对损坏输入输出失败定位：

```
verified=false error_stage=parse source_offset=1762 recoverable=false
```

## 子命令

### `fixture`

自生成夹具到指定目录，输出可复现（相同参数得到字节一致的文件）：

| 参数 | 含义 |
| --- | --- |
| `--out DIR` | 输出目录，默认 `./tmp` |
| `--pattern MODE` | `clean` / `clipjump` / `drift` / `wrap` / `duplicate` / `regression` |
| `--video N` | 视频样本数 `[8,4096]`，默认 120 |
| `--audio N` | 音频样本数 `[0,4096]`，默认 60 |

产物：

- `timeline.mp4` — 双轨（视频 90 kHz / 音频 48 kHz）MP4，含 3 个剪辑 + 1 个转场
- `timeline.json` — 时间线 sidecar（版本标记、轨道、剪辑、转场、样本区间）
- `rtp.dump` — RTP 时间戳映射（`[2 字节长度][RTP 包]`），仅元数据包头
- `corrupt.mp4` — 有效文件的截断前缀，用于验证错误定位

### `repair`

| 参数 | 含义 |
| --- | --- |
| `--input FILE` | 输入 MP4（必填） |
| `--timeline FILE` | 时间线 sidecar，默认同目录 `timeline.json` |
| `--report text\|json` | 报告格式，默认 `text` |
| `--buffer N` | 有界队列容量，默认 8 |
| `--slow-consumer-ms N` | 消费者每样本延迟，用于演示背压 |
| `--policy clamp\|keep` | 修复策略；`keep` 仅标记、保留原始时间戳供审计 |
| `--drift-ppm N` | 时钟漂移修正（ppm） |
| `--resume-from SEG` | 从最近验证边界续跑，跳过该片段及之前的片段 |
| `--out FILE` | 把修复后样本流按稳定编码写入文件 |

### `verify`

校验 MP4 箱结构与时间线 sidecar；失败时输出 `error_stage`、`source_offset`、
`recoverable`，可用 `--report` 之外的参数与 `repair` 一致地定位。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 验证通过 |
| 1 | 素材或校验失败（解析/修复/索引/验证阶段错误） |
| 2 | 参数错误 |
| 3 | 运行时 I/O 或取消 |

## 架构

```
cmd/mediarepair/main.go   子命令解析、退出码、错误定位输出
internal/model            轨道/剪辑/转场 + 统一时间基准（整数换算）
internal/fault            带偏移、轨道、阶段、可恢复性的错误模型
internal/parse            MP4 箱结构与样本表流式解析、RTP dump、sidecar
internal/repair           回绕/重复/倒退/剪辑跳变/漂移修复
internal/index            关键帧随机访问索引（拒绝缺失样本）
internal/pipeline         解析→修复→索引→验证 编排 + 有界队列背压 + 续跑
internal/stream           固定容量有界队列（背压、取消、排空）
internal/report           逐片段 SHA-256 与机器可读报告
internal/fixture          自生成有效/损坏/边界素材
tests/integration_test.go 跨模块行为验证
```

### 时间基准

统一基准为每秒 `1_000_000` tick。所有换算走 `tick * Unified / timescale` 的整数
运算（先约分再乘），并通过 `FromUnified` 做往返校验，避免浮点累计误差；
`roundtrip_match` 即往返一致的样本比例。

### 修复语义

- **回绕**：32 位时间戳域内倒退超过 `2^31` 判为回绕，展开到 64 位并保持单调。
- **重复/倒退**：同一片段内钳制为严格递增；`keep` 策略仅标记。
- **跨剪辑跳变**：片段起点间隔显著大于常规样本时长时标记，并把片段重定基到
  时间线逻辑起点，保证剪辑/转场边界连续。
- 原始 `DTS/CTS` 始终保留在 `OriginalDTS/OriginalCTS`，供审计。
- RTP dump 时间戳映射套用同一修复策略；`rtp` 字段报告回绕/重复/倒退计数。

### 索引与验证

关键帧索引按统一时间排序；每个样本查询最近关键帧，只有命中同片段条目的比例
计入 `keyframe_hit_rate`。指向未注册样本的条目会被拒绝，不做静默跳过。

### 背压与内存

解析器作为生产者把样本推入容量为 `--buffer` 的队列，消费者慢时生产者阻塞并累计
`backpressure_wait_ms`；`peak_buffer_samples` 不会超过容量。取消通过 `context`
传导，阻塞的读写会被及时释放，已验证片段的报告仍会产出。

### 续跑

`--resume-from SEG` 以该片段在统一时间线上的起点为截止，跳过此前所有片段
（及该片段自身）的修复与哈希；剩余片段哈希与全量运行一致，可与上轮报告对账。

## 约束

- 轨道数 ≤ 16、样本总数 ≤ 2^20、单样本 ≤ 4 MiB、时间线文件 ≤ 1 MiB。
- 时间基准换算或长度溢出即拒绝，错误带偏移与阶段。
- 夹具样本负载总量 ≤ 8 MiB。
