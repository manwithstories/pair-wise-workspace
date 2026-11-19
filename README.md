# archive — 影视资料馆母版时间线恢复与分片清单

从分散的磁带数字化副本和旧存储盘恢复长期归档母版：重建显式整数时间线、按轨道依赖恢复边界、
生成可对齐、可追溯的多码率分片清单与恢复报告。纯本地运行，无外部服务。

## 运行环境

- Go 1.23+（macOS arm64 / amd64 均验证）
- 依赖：`github.com/abema/go-mp4 v1.4.1`、`github.com/pion/rtp v1.8.9`

```sh
go get github.com/abema/go-mp4@v1.4.1 github.com/pion/rtp@v1.8.9
go build ./...
go test ./...
```

## 验收命令

```sh
go run ./cmd/archive demo --seed 17 --duration 120 --bitrates 800,1600,3200 --loss 0.03 --json
go run ./cmd/archive demo --case corrupt-index --json
go run ./cmd/archive demo --case wrap-vfr-drift --json
```

成功样例应满足：`sync_error_ms ≤ 20`、`loss_recovery_rate ≥ 0.90`、`p95_latency_ms ≤ 500`、
`segments > 0`、`alignment_delta_ms ≤ 40`。

`--json` 输出同时包含便于机器验收的 `metrics` 与可追溯的 `issues`、
`issue_counts`、`segment_list`、`timeline`、`streams`、`index`、`tracks`、`alignment`、`rtp`、`resume`。

### 样例说明

| case | 内容 | 预期退出码 |
|------|------|-----------|
| `success` | 三码率母版 + 时钟漂移 + 计数器回绕 + 3% 丢包 | 0 |
| `corrupt-index` | 索引含越界偏移与缺页，验证定位与按码流修复 | 3（输入损坏但已生成部分报告） |
| `wrap-vfr-drift` | 32 位计数器回绕、重复/回退时间戳、VFR、采集时钟 +0.5 s 重置 | 0 |

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 处理完成 |
| 2 | 参数错误 |
| 3 | 输入损坏但已生成部分报告 |
| 4 | 无法恢复的依赖、摘要不匹配或内部错误 |

`ok` 字段与退出码保持一致：仅当退出码为 0 时为 `true`。

## 架构

```
cmd/archive/main.go          参数解析、退出码、终端输出
internal/model               整数纳秒时间线、轨道图基础类型
internal/errs                统一错误分类、稳定 issue code 与定位字段
internal/clock               回绕展开、重复/回退修复、分段漂移拟合
internal/stream              H.264 / H.265 / AV1 单元扫描与字节偏移
internal/index               关键帧索引校验、按码流修复、恢复来源审计
internal/graph               轨道依赖排序、缺失依赖与环路分支停止
internal/segment             关键帧落点的目标时长切片
internal/align               多码率表示层统一切点对齐
internal/report              稳定 JSON 报告与退出码映射
internal/media               MP4 母版构建与解析（go-mp4 读取）
internal/testkit             自生成样例、索引页与丢包 RTP 夹具
internal/pipeline            按依赖顺序编排全部阶段
internal/resume              输入/配置摘要绑定，拒绝复用陈旧产物
```

数据流：码流扫描与索引校验 → 统一整数时间基准 → 轨道依赖图 → 依序恢复时间线 →
生成分片 → 对齐表示层 → 清单与指标。所有模块共用 `errs.Class` / `errs.Code` /
`errs.Location`，因此报告中每个问题都能定位到文件、轨道、样本与字节偏移。

## 关键行为

- **回绕与异常时间戳**：仅当跳变量接近计数器的半个量程时才判定为回绕；重复或小幅回退的
  时间戳会被记录并修复，绝不静默丢帧（输出样本数始终等于输入样本数）。
- **漂移拟合**：用锚点做最小二乘仿射拟合；残差跳变超过阈值时分段，断点取真实时钟跳变所在的
  样本，禁止跨断点外推。拟合漂移超出上限时该分支出错。
- **索引修复**：区分截断头、非法长度、越界偏移、重复样本与偏移重叠；缺页用扫描到的候选关键帧
  补齐，并保留 `source`（original / scan / pending_review）与置信度；无法确定者标为待复核。
- **轨道依赖**：缺失依赖或环路只停止相关分支，并在报告中列出涉及的全部轨道。
- **分片与对齐**：切点落在关键帧上；多码率采用统一程序时间切点，无法满足时报告最大边界偏差
  并将该边界标为待复核。
- **续跑**：`--out DIR --resume` 时按输入码流与配置摘要判定；摘要或产物不匹配则拒绝复用并输出
  稳定 JSON 错误记录（退出码 4），否则直接复用已写出的清单。

## 索引文件格式

`internal/index` 使用固定 little-endian 布局（样例由 `internal/testkit` 生成）：

```
header: magic "IDX1" (4) | version u16 | reserved u16 | timescale u32
entry : sample u32 | offset u64 | size u32 | pts u64      (24 bytes)
```

偏移相对于 MP4 `mdat` 载荷，与码流扫描使用同一坐标空间。

## 资源约束

单文件、索引条目数、样本数、单元数与扫描字节数均有上限（见各包 `Max*` 常量），越界输入会
返回分类错误而非无界分配。
