# mediabridge — 影视母版无损恢复与归档

一个纯终端的媒资恢复工具：把旧容器（MP4/MOV 主轨 + RTP 辅助轨）中的编码包
**逐字节复制**到统一交付流，并生成可独立核验的来源清单。全程不解码、不重编码。

## 快速开始

```sh
go mod tidy
go build ./...
go test ./...
go run ./cmd/archiver demo --seed 41 --interrupt-after 120 --resume \
    --loss 0.03 --reorder 0.02 --out /tmp/archive-demo
```

`demo` 会自生成样例语料（干净素材、非零起始素材、损坏素材），模拟中途断电后
恢复，打印验收指标。退出码：`0` 通过、`2` 输入/映射错误、`3` 转封装/核验失败、
`4` 检查点不可恢复。

## 验收指标

| 指标 | 阈值 |
| --- | --- |
| `sync_error_ms` | ≤ 1.0 |
| `packet_recovery_rate` | ≥ 0.97 |
| `p95_latency_ms` | ≤ 250（本地样例） |
| `copied_packet_hash_match` | 100% |
| `resumed_duplicate_packets` | 0 |
| `provenance_coverage` | 100% |

损坏素材会额外输出 `error_code`、`source_offset`、`recovered_assets`。

## 架构

```
cmd/archiver          终端入口与 demo 编排
internal/clock        统一纳秒时基（溢出检查、预算、限额）
internal/fault        带源位置的结构化错误模型与退出码映射
internal/safeio       根目录受限的路径解析（拒绝符号链接逃逸）
internal/media        适配器与内核共享的类型
internal/adapter      版本化适配器注册表
internal/adapter/mp4  MP4/MOV 解复用（stts/ctts/stsc/stsz/stco/stss、编辑列表）
internal/adapter/rtp  RTP 辅助轨整理（乱序/重复/丢失/序号回绕）
internal/manifest     清单加载与路径限定
internal/maptrack     轨道映射规则
internal/core         无损转封装内核与来源清单
internal/journal      原子检查点、续跑与隔离
internal/verify       独立核验与验收指标
internal/fixture      确定性样例生成（MP4 与 RTP）
internal/pipeline     端到端编排与 demo 语料
```

### 数据流

1. **清单**（`internal/manifest`）限定根目录，逐个引用本地文件，拒绝逃逸。
2. **适配器**（`adapter.Registry`）按 `format@version` 解析；未知版本报
   `unknown_version` 并携带源路径/轨道/偏移。
3. **MP4 读取器**展开样本表，应用编辑列表与非零起点（裁剪时间轴原点之前的
   样本），把 DTS/PTS/时长换算到纳秒。
4. **RTP 读取器**以长度+CRC 分帧，区分损坏与丢包；按序号重组，窗口内等待空洞，
   超窗计入丢失，去重并把 32 位时间戳扩展为单调纳秒轴。
5. **映射**（`maptrack`）选择视频/音频主轨与命名辅助槽；缺失或重复映射时拒绝
   该素材，其他素材继续。
6. **内核**（`core`）按交付顺序复制编码包，逐包计算哈希，写入自描述流并记录
   来源（源素材、轨道、包序号、字节偏移）。
7. **检查点**（`journal`）按素材+轨道记录已提交的输入包范围与配置摘要；写入
   采用临时文件 + 原子替换。续跑时把输出截断到最后一个完整包边界，已提交的
   前缀字节保持不变，因此不会重复计入。
8. **核验**（`verify`）独立重读输出字节、重算哈希、测量跨轨 PTS 偏差，并检查
   重复包；与内存计数解耦。

### 中断与恢复

`--interrupt-after N` 在第 N 个包提交后模拟断电。检查点保存前缀的字节长度与
摘要；下一次运行校验前缀摘要后从该边界继续，输出与一次干净运行**逐字节一致**
（由 `internal/pipeline` 的集成测试保证）。

若源摘要或适配器版本变化，旧检查点被移入 `quarantine/`（绝不删除），该素材从头
处理。

## 约束

- macOS arm64/amd64，纯本地，无外部依赖。
- 单素材 ≤ 24 小时、单轨 ≤ 2000 万包、工作集 ≤ 256 MiB。
- 仅处理清单引用的本地文件，路径限定在指定根目录内，拒绝符号链接逃逸。
- 样例数据全部由 `internal/fixture` 按种子生成，不引用外部文件。
