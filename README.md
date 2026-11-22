# mediad — 本地多轨媒体处理流水线

纯本地、无外部服务依赖的 Go 媒体流水线。面向远程制作团队的多轨时间线
复核场景：定位剪辑点、转场与静音段，从截断/损坏/索引缺页等局部故障中
隔离恢复，并通过内容寻址缓存与检查点支持中断重跑。

## 技术栈

- Go 1.23（仅标准库 + 两个固定版本依赖）
- `github.com/abema/go-mp4 v1.4.0`：MP4 盒结构解析
- `github.com/pion/rtp v1.8.9`：RTP 结构与时间戳
- 持久化仅使用本地文件；示例媒体全部由 Go 测试代码确定性生成

## 模块结构

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 整数有理数时间基准，跨轨换算误差 ≤ 1 tick，拒绝零分母/溢出 |
| `internal/model` | 来源、轨道、样本、片段、清单领域模型与 64 MiB 片段上限 |
| `internal/fault` | 可恢复/致命错误分类，携带错误码与受影响 tick 区间 |
| `internal/mp4` | 盒树解析、截断边界恢复、未知盒原始字节保留、样本表展开、夹具生成 |
| `internal/index` | 关键帧索引重建与校验，缺页扫描、不伪造尾部关键帧 |
| `internal/cut` | 剪辑点切分、转场跨轨引用、损坏区间 incomplete 状态 |
| `internal/audio` | PCM RMS 静音窗口，跨片段拼接，截断窗口不误报 |
| `internal/fingerprint` | 片段字节+时间范围指纹、来源组合摘要、文件摘要 |
| `internal/cache` | 内容寻址缓存，来源/范围/内容三重核对，原子写入，拒绝路径逃逸 |
| `internal/checkpoint` | 原子检查点、重启幂等去重、输入变化失效 |
| `internal/rtp` | RTP 包解析与 32 位时间戳回绕展开 |
| `internal/pipeline` | 增量编排与核验指标 |
| `cmd/mediad` | 终端适配器，退出码 0/1/2 |

## 快速开始

```sh
go mod tidy
go build ./...
go test ./... -count=1
go run ./cmd/mediad demo --seed 41 --tracks 3 --resume
```

演示生成健康、尾部截断、关键帧索引缺页（叠加截断）与坏样本盒、缓存
篡改五类数据，输出形如：

```
first   roundtrip=7/7 keyframe_hit=8/8 segment_hash=7/7 recovered=0 incomplete=2 cache_rejected=1
resume  roundtrip=7/7 keyframe_hit=8/8 segment_hash=7/7 recovered=7 incomplete=2 cache_rejected=1
PASS 全部断言通过：往返一致率 100%，哈希复核 100%，恢复哈希与干净运行一致
```

字段含义：

- `roundtrip`：正常片段往返序列化一致比例（须 100%）
- `keyframe_hit`：关键帧索引命中率（须 ≥ 90%）
- `segment_hash`：逐片段哈希复核比例（须 100%）
- `recovered`：第二次运行从检查点恢复的片段数（去重，不重复计数）
- `incomplete`：落在截断/损坏区间的不完整片段数
- `cache_rejected`：缓存三重核对失败被拒绝的次数

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 全部断言通过 |
| 1 | 核验失败（指标未达下限或恢复哈希不一致） |
| 2 | 参数或输入无效（含路径逃逸、轨道数越界） |

## 故障隔离设计

- 解析层在盒级别捕获截断，跳过声明区间继续解析后续完整盒（如 moov）。
- 样本表展开把局部不一致转为可恢复故障并标记受影响样本，不扩散到邻轨。
- 切分对损坏区间输出 `truncated/incomplete` 片段后继续处理其余区间。
- 静音检测对含截断样本的窗口标注 `incomplete`，绝不判为静音。
- 缓存命中仍重新核对来源摘要、片段范围、内容摘要，任一不符即拒绝重算。
- 检查点仅在片段结果与清单落盘后推进，状态文件经临时文件 + 原子替换。

## 确定性

同一种子与轨道数在独立工作目录重复运行，清单、片段指纹与指标完全一致；
恢复运行的逐片段哈希与干净运行一致。所有随机源仅来自注入的 `seed`。
