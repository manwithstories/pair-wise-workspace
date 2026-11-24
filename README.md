# bcast

本地运行的广播媒资交付流水线：流式解析带时间戳的 MP4/MOV，重建关键帧索引，
按时间线切分节目、检测静音、计算逐段指纹并去重，再把可解析样本封装成可交付
文件。整条链路带依赖调度、有限重试与检查点续跑，失败时报告首个阻塞依赖。

样例素材、损坏变体和 RTP 侧车全部由 Go 代码按种子生成，运行时不读取外部媒体
文件、不访问网络或外部服务。

## 环境

- Go 1.25.6（`go.mod` 的 `go` 指令锁定为 `1.25.6`）
- macOS / Linux / 任何 Go 支持的平台

依赖通过 `go.mod` 固定：`github.com/abema/go-mp4 v1.4.1` 解析与写出容器，
`github.com/pion/rtp v1.8.11` 解析 RTP 时间戳侧车。

## 构建与测试

```sh
go mod tidy
go build ./...
go test ./...
```

`go test` 覆盖每个子系统；`pipeline` 包还包含端到端与 1 GiB 内存上界测试
（`-short` 会跳过后者）。

## 演示与验收

```sh
go run ./cmd/bcast demo \
  --seed 17 --segments 12 \
  --damage truncate,broken-box,missing-index \
  --checkpoint /tmp/bcast-checkpoint.json

go run ./cmd/bcast verify --checkpoint /tmp/bcast-checkpoint.json
```

`demo` 的输出（键值稳定，逐行可 grep）：

```
roundtrip_match_rate=1.000
keyframe_hit_rate=0.909
segment_hashes=12
damaged_ranges=3
blocked_dependency=parse:corrupt
recovered_nodes=2
segment_hash_digest=0c3db00871d87147fa5675005201ae85
```

- `roundtrip_match_rate` 主交付每个分段再封装后逐样本与源字节一致的占比。
- `keyframe_hit_rate` 非开头切点落在关键帧上的比例（开头切点天然对齐，不计入）。
- `segment_hashes` 主交付的逐段内容哈希数量。
- `damaged_ranges` 本次批次记录到的不可恢复字节区间总数（含各损坏变体）。
- `blocked_dependency` 最早被阻塞节点回溯到的根因（此处为故意损坏的输入）。
- `recovered_nodes` 依赖可恢复结果或重试完成的节点数。
- `segment_hash_digest` 有序逐段哈希的摘要，用于证明重复运行指纹一致。

同一参数重复运行，上述每一行（包括 `segment_hash_digest`）完全一致；改变
`--seed`、`--segments`、`--damage`、窗口或阈值会改变参数签名，相关节点按新
幂等键重算。

输出目录默认为检查点同级的 `<name>-out/`，可用 `--out` 指定。其中：

- `clean/seg-NNN.mp4` 主交付分段（其它损坏变体各有同名子目录）；
- `damaged-ranges.txt` 逐条损坏区间清单（交付名、字节区间、原因）。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 处理与核验完成（包括主交付健康、损坏输入被正常隔离） |
| 2 | 输入不可恢复或依赖阻塞（主交付自身失败） |
| 1 | 参数、I/O 或内部错误 |

`verify` 在交付文件被篡改、短读或缺失时以非零退出并指出文件。

## 架构

| 包 | 职责 |
| --- | --- |
| `media` | 统一时间基准 `Time`、样本与来源偏移、故障分类 `Issue`、错误模型（可恢复/致命/阻塞 + 首个阻塞依赖） |
| `demux` | 增量解析 MP4/MOV box 与样本表，重建关键帧索引，解析 RTP 时间戳侧车 |
| `segments` | 按目标时长切分，切点吸附到真实视频帧、记录关键帧距离与帧吸附误差，音画共用时间基准 |
| `silence` | 按窗口统计音频 RMS 能量，未完整覆盖或跨损坏区间一律判“未知”，绝不判静音 |
| `fingerprint` | 逐段内容哈希（排除时间戳）、去重分组、幂等键 |
| `output` | 再封装（保留 stsd/ctts/绝对位置）、原子落盘、逐样本往返校验 |
| `pipeline` | DAG 调度、并发执行、依赖失败传播、有限重试、检查点续跑、批次编排与指标 |
| `internal/fixture` | 代码化生成干净/截断/坏 box/缺索引素材与 RTP 侧车 |
| `cmd/bcast` | 终端入口：`demo` 与 `verify` |

### 数据流

MP4/MOV 字节流 → `demux`（统一时间基准的样本 + 关键帧索引）→ `segments` 切分
→ `silence` / `fingerprint` 共用样本时间与错误模型 → `pipeline` 调度这些节点，
把可恢复结果交给 `output` 再封装并往返校验。

### 关键不变量

- **不猜。** 截断、坏 box、缺 moov、索引不完整分别归类；缺少 moov 直接判为
  不可恢复并以退出码 2 结束。可重建的索引标注来源 `stss`/`rebuilt`；无法重建
  时阻塞下游并指出首个阻塞依赖。
- **不改时间。** 样本保留容器中的 DTS/CTS 与来源偏移；再封装原样写回时间戳与
  压缩数据，绝不重新编码或补帧。
- **不伪造。** 缺失字节只以损坏区间呈现，输出体积不会凭空增大。
- **可重复。** 所有结果由输入与参数唯一决定；幂等键 = 输入摘要 + 参数 + 段序号，
  首写生效，重复执行不改变哈希、不重复写入。
- **可续跑。** 节点状态持久化到本地检查点（临时文件 + fsync + rename），进程
  重启只重跑未完成节点，结果与完整重跑逐字节一致。
- **有界内存。** 载荷按需从源读取，从不整体驻留；内存随样本数而非字节数增长。

## 约束

- 单进程处理 1 GiB 输入峰值内存 < 512 MiB（`pipeline/memory_test.go` 实测约
  181 MiB 总分配）。
- 仅识别 MP4/MOV box 与音视频轨道；无法可靠修复的字节保留为损坏区间。
- 写文件一律同目录临时文件 + 原子替换。
