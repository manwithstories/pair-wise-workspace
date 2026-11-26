# bq — 播出素材时序核对与再封装工具

区域电视台每日批量核对广告、预告片和节目母版的时序并再封装的纯本地、
纯终端工具。它把供应商素材清单合成为多轨整数时基时间线，模拟丢包并用 XOR
校验组恢复，离屏导出可复核帧序列，最后输出带溯源证据的 JSON 报告。

## 运行环境

- macOS（Darwin），Go 1.25.6（`go.mod` 的 `go` 指令固定为 `1.25.6`，无 toolchain 指令）
- 本地 FFmpeg/ffprobe（Homebrew：`brew install ffmpeg`），仅用于探测、解码与再封装
- 核心依赖：标准库 + `github.com/disintegration/imaging v1.6.2`
- 无外部服务、无联网取样，所有样例随代码确定性生成

## 安装与构建

```bash
brew install ffmpeg
go mod download
go build -o /tmp/bq ./cmd/bq
go test ./...
```

## 演示与校验

```bash
# 生成 30 分钟、三条视频轨 + 一条音频轨的自描述素材并跑完整流水线
/tmp/bq demo --minutes 30 --seed 41 --inject-loss 0.02 --inject-wrap --out /tmp/bq-out

# 复核已生成的清单
/tmp/bq verify --manifest /tmp/bq-out/manifest.json --json
```

### 退出码

| 码 | 含义 |
|----|------|
| 0  | 通过（指标在阈值内） |
| 1  | 指标超限 |
| 2  | 不可恢复输入（损坏清单、不支持的编码等） |
| 64 | 参数错误 |

### 验收指标（正常样例）

- `av_sync_max_ms` 不高于 20
- `loss_recovery_pct` 不低于 90
- `latency_p95_ms` 数量级为百至千毫秒
- `frames`、`frame_sha256` 给出帧序列规模与组合哈希

### 损坏样例字段

`error_count`、`first_source_offset`、`recovered_packets`、`unrecovered_packets`。
四类损坏（损坏清单、PTS 回绕、重复包、尾段截断）样例生成在
`<out>/damage/`，回绕与丢包会传播到同步验证证据。

## 架构

三个子系统组共用整数时基（`clock.MasterHz = 90000`）与统一错误模型
（`internal/model`，含阶段、流号、原始字节偏移、原始 PTS）。

```
cmd/bq            终端入口（demo / verify），参数解析与退出码
internal/model    统一错误模型与确定性排序
internal/clock    有理数整数时基、溢出拒绝、回绕展开（零相位单调轴）
internal/randx    确定性 PCG-XSH-RR 随机源（同种子可复现）
internal/manifest 自描述清单类型与严格解析（损坏即致命，带字节偏移）
internal/timeline 多轨合成、稀疏关键帧定位、分段漂移估计与锚点冲突
internal/transport 序号包流、XOR 校验组恢复、重复包与尾段截断统计
internal/ffmpeg   本地 ffmpeg/ffprobe 封装（VFR/稀疏GOP/再封装/抽帧）
internal/fixture  确定性夹具：四轨、VFR、漂移、真实取模回绕、注入计划
internal/transform 按时间线编辑列表逐轨流拷贝，越界/缺轨隔离处理
internal/render   离屏自描述 RGBA 帧导出，PNG 与 SHA256
internal/verify   AV 同步/恢复率/P95 指标、证据传播与报告
internal/pipeline 编排三阶段子系统的端到端流水线
```

### 数据流

1. **清单 → 时钟/时间线**：带源偏移的包经回绕展开（减去起始基线）归一时基。
2. **恢复 → 变换/帧导出**：恢复后的包按序号排序，进入逐轨再封装与离屏渲染。
3. **验证**：读取统一时间线、输出文件与错误记录，计算指标并核对异常传播。

## 关键设计约束

- 全程整数有理数运算，乘法/加法溢出即拒绝并记录 `INTEGER_OVERFLOW`，不用浮点走时基路径。
- 回绕展开保持单调并保留原始 PTS；漂移校正只读，绝不静默改写源偏移。
- 锚点冲突或残差超阈值标记为不可恢复区间，而不是强行拟合。
- 同种子重复运行得到相同帧哈希与错误排序（墙钟延迟字段除外）。
- 仅支持清单声明的 H.264/AAC MP4 与自描述 RGBA 测试样本；产物只写在 `--out` 目录下。
