# mediad — 多机位现场采集快速后期交付流水线

面向大型演唱会 / 体育赛事的本地后期交付工具：汇入 MXF/MP4 素材与 RTP
采集包，对齐到可剪辑时间线，并在网络抖动、输入损坏或进程中断后定位
问题、隔离片段并幂等续跑。纯本地运行，无外部服务依赖。

## 技术栈与依赖

- Go 1.25.6（`go.mod` 的 `go` 指令固定为 `1.25.6`）
- `github.com/abema/go-mp4 v1.3.0`：MP4 box 读写
- `github.com/pion/rtp v1.8.11`：RTP 包结构
- 测试仅用标准库 `testing`

> 说明：规格中 `toolchain` 指令为可选项。当本机工具链与 `go` 指令
> 版本相同时，`go mod tidy` 会移除冗余的 `toolchain go1.25.6` 行；
> 版本仍由 `go 1.25.6` 固定，行为不受影响。

## 一键安装与构建

```sh
go mod download
go mod tidy
go build ./...
go test ./...
```

## 验收演示

演示命令自行生成全部样例与产物，不引用任何外部文件：

```sh
go run ./cmd/mediad demo --seed 41 --loss 3 --crash-after commit
go run ./cmd/mediad verify --manifest ./artifacts/manifest.json
```

可选参数：

- `--seed N`：样例与抖动的确定性种子
- `--loss N`：初始扣留、随后在恢复窗口内补发的突发丢包数
- `--crash-after write|verify|commit`：模拟写入中断 / 校验失败 /
  提交前崩溃
- `--work-dir DIR`：运行期工作目录（默认 `artifacts-run`）
- `--root DIR`：工作区沙箱根，越界路径与符号链接逃逸将被拒绝

### 终端输出字段

| 字段 | 含义 |
| --- | --- |
| `roundtrip_rate` | 独立解析器往返摘要一致率 |
| `keyframe_hit_rate` | 关键帧索引命中率 |
| `segments` | 交付片段数 |
| `segment_hash_match` | SHA-256 重算匹配数 / 总数 |
| `recovered_packets` | 窗口内恢复的丢包数 |
| `av_offset_ms` | 音画偏移（毫秒，容差 20ms） |
| `rollback_count` | 未提交产物回滚次数 |
| `rejected_segments` | 被隔离的损坏 / 不合规片段数 |
| `metadata_time_ratio` | 元数据处理耗时 / 输入时长（目标 ≤ 2） |

退出码：`0` 核验通过，`1` 处理或一致性失败，`2` 参数错误。

## 架构（三条协作链路）

```
internal/
  clock/    统一 RTP 与容器时间基准（32 位时间戳回绕、微秒时基）
  model/    可分类、可重试错误模型
  ingest/   MXF/MP4 解析、夹具生成、容器/轨道描述冲突裁决
  rtp/      抖动缓冲、重排去重、有限窗口丢包恢复与片段隔离
  audio/    多声道布局白名单、采样率与重采样边界校验
  sync/     音画偏移估算与有界对齐（禁止静默丢弃）
  txn/      写前日志、原子提交、校验失败回滚、崩溃续跑
  verify/   逐片段 SHA-256、关键帧命中率、独立往返复核
  safeio/   工作目录沙箱与符号链接逃逸拒绝
  pipeline/ 端到端编排与清单产出
cmd/mediad/ demo / verify 命令入口
```

1. **容器解析与冲突裁决** → 规范化轨道，保留原始值与裁决值；
   无法安全解释的描述冲突直接拒绝。
2. **RTP 缓冲恢复 + 统一时钟** → 乱序、重复、回绕、突发丢包按
   死线窗口输出有序媒体单元；超窗标记不可恢复并隔离片段。
3. **音频映射 + 同步 → 事务 → 核验** → 生成片段计划，写前日志
   记录阶段边界，原子提交落盘，独立重算哈希证明未被改写。

## 关键约束的落实

- **安全拒绝冲突**：非 90° 整数倍旋转、NCLC 与 ICC 混冲均拒绝。
- **不静默丢媒体**：超容差单元标记 `out-of-tolerance` 并保留。
- **256 MiB 单片段上限**：`pipeline.MaxSegmentBytes` 强制校验。
- **路径沙箱**：所有路径经 `safeio.Resolve` 限定在工作目录内，
  逐组件拒绝符号链接。
- **幂等**：已提交片段复用；重复运行产出相同哈希。
- **无构建产物入库**：运行期目录由 `.gitignore` 忽略。

## 损坏夹具

自生成并验证以下失败模式的错误传播与隔离：

- 截断 MP4（box 头不完整）
- 损坏样本表（`stsz` 计数与条目不符）
- 缺失关键帧索引（空 `stss`）
- 异常声道布局（声道数与标签集不一致）
- MXF 损坏标记 / 截断 KLV / 坏魔数
