# archive — 跨年代影视归档修复流水线

面向影视资料馆的本地命令行工具：解析 HLS/DASH 归档清单，解复用 MP4/fMP4
分片，按语言/编解码器/角色确定性映射轨道，无损转封装进内容寻址存储（CAS），
每片原子提交检查点，产出可复核的音画同步误差、批次延迟与来源摘要。

纯本地运行，不依赖在线媒资或独立服务。全部样例数据由代码生成，可复现。

## 环境

- macOS（Darwin），Go 1.23+
- 依赖固定：`github.com/abema/go-mp4 v1.4.1`、`github.com/pion/rtp v1.8.11`

## 构建与测试

```sh
go mod tidy
go build ./...
go test ./...
```

## 演示

```sh
# 生成 120 片合成归档，模拟丢包与第 40 片后的进程中断
go run ./cmd/archive demo --root "$(mktemp -d)" \
    --segments 120 --loss-every 37 --interrupt-after 40

# 用 demo 打印的 root 继续，仅重做未提交分片
go run ./cmd/archive resume --root "$ARCHIVE_ROOT"
```

正常与恢复路径会打印（下例为 resume 完成后的数值）：

```text
sync_error_ms=0.4 recovered_packets=3 recovery_rate=100.0 p95_latency_ms=24.0 cache_verified=120 output_sha256=<64位摘要>
```

即满足验收条件 `sync_error_ms<=1.0`、`recovered_packets=3`、
`recovery_rate=100.0`、`p95_latency_ms<250.0`、`cache_verified=120`，
摘要为 64 位十六进制。

`demo` 把演练参数（片数、丢包间隔）写入 `work/demo.json`，`resume` 据此
重放同一 RTP 序列，因此两条命令都会给出 `recovered_packets`、
`recovery_rate` 与真实 `p95_latency_ms`。`cache_verified` 是该次运行中
经重读 CAS 复核的已提交分片数：demo 中断时是 40，resume 完成后是 120。

`output_sha256` 是本次运行已确认分片集合的摘要：demo 的 40 片与一次性
跑完的 120 片摘要不同是预期的；对同一已提交集合重复运行（含中断后
resume 完成）摘要保持稳定，因为分片产物只依赖输入分片与映射版本。

### 损坏样例

```sh
go run ./cmd/archive demo --root "$(mktemp -d)" --corrupt truncated --segments 120
```

打印并返回退出码 2：

```text
first_bad_segment=media/seg-00060.m4s recoverable_segments=60
```

可选 `--corrupt`：`truncated`（分片截断）、`missing`（缺片）、`duplicate`
（重复引用）、`wrap`（33 位计数回绕）、`conflict`（轨道冲突，致命，退出码 1）。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 完成（含重复引用等可恢复异常） |
| 2 | 输入损坏，已保留确认分片并给出首个不可恢复依赖 |
| 1 | 校验、存储、轨道冲突或恢复失败 |

注意：`go run` 会把非零子进程退出码统一报告为 1。需要区分 0/1/2 时请先
`go build -o archive ./cmd/archive` 再运行二进制。

## 架构

| 路径 | 职责 |
|------|------|
| `cmd/archive/main.go` | 命令适配（demo/resume），输出与退出码 |
| `internal/clock` | 有理数时间基准、33 位回绕展开、按轨时基换算 |
| `internal/fault` | 共享错误模型与恢复等级（recoverable/degraded/fatal） |
| `internal/manifest` | HLS 与 DASH SegmentTemplate/SegmentTimeline 解析 |
| `internal/demux` | go-mp4 解复用，保留原始样本与样本条目 |
| `internal/track` | 轨道确定性映射，冲突报错不覆盖 |
| `internal/remux` | 无损自包含 fMP4 写出（确定性字节） |
| `internal/cache` | 内容寻址存储；命中前重新核验来源与摘要 |
| `internal/checkpoint` | 每片原子提交（fsync + rename） |
| `internal/pipeline` | 增量调度、重复去重、来源记录、退出状态 |
| `internal/metrics` | 独立复核：重读 CAS、重算同步误差与 P95 延迟 |
| `internal/rtp` | 合成 RTP 序号窗口恢复，统计不可恢复数与恢复率 |
| `internal/fixture` | 可复现归档夹具（清单 + fMP4 分片 + 时间线描述） |

## 设计要点

- **确定性与可恢复**：输出只依赖输入分片与映射版本；检查点记录输入摘要、
  映射版本与输出摘要。中断后只重做未提交分片，重跑产物摘要一致。
- **回绕时间戳**：`#EXT-X-ARCHIVE-BASE` 给出“计数零对应的呈现时间”，
  解码时间按源计数器周期归位后锚定，回绕与重排都能落在真实时间线上。
- **来源可追溯**：每片 `work/provenance/` 记录输入/输出摘要、轨道表与时长
  覆盖；指标模块读回并交叉核对，不一致即判失败。
- **安全边界**：拒绝绝对路径、父目录逃逸与超 64 MiB 分片；临时文件绝不会
  冒充已提交结果（检查点与来源记录均先写临时文件再 rename）。
