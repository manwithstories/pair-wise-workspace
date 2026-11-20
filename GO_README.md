# media-audit

电视网多码率母版的分片边界审计与播出清单生成工具。纯本地、终端运行、
无外部服务、无 GUI。所有媒体时间统一使用整数微秒，依赖固定在 `go.mod`。

## 技术栈

- Go 1.23（在 1.25 工具链下同样可构建）
- `github.com/abema/go-mp4 v1.4.1`：MP4 盒结构读写（sidecar 元数据）
- `github.com/pion/rtp v1.8.11`：复用 RTP 32 位时间戳回绕处理语义
- 标准库 `testing`：单元、集成与故障恢复测试

## 构建与验收

```bash
go mod download
go build -o /tmp/media-audit ./cmd/media-audit
go test ./...
```

## 端到端演示

```bash
go run ./cmd/media-audit demo \
  --codec av1 --representations 3 --frames 18000 \
  --block-frames 600 --inject-corruption-at 9127 \
  --interrupt-after-block 8 --resume
```

程序自行生成确定性临时素材并打印：

| 指标 | 含义 | 验收期望 |
| --- | --- | --- |
| `corrupt_offset_bytes` | 注入损坏的字节偏移 | — |
| `location_error_bytes` | 诊断位置与实际损坏的字节差 | 0–4 |
| `resume_ms` | 中断后恢复阶段耗时 | < 3000 |
| `segments` | 生成分片数 | 30–90（18000 帧/三层） |
| `alignment_max_ms` | 层间切点最大偏差 | ≤ 40 |
| `manifest_sha256` | 清单内容校验和 | 连续两次运行一致 |

### 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 完成且校验通过 |
| 2 | 输入或索引损坏已精确定位 |
| 3 | 恢复状态无效或输出校验不一致 |
| 4 | 参数或本地 I/O 错误 |

## 架构与数据流

```
fixture(合成裸流/sidecar)
  → stream(增量扫描 AU/关键帧, 源字节区间, 块 SHA-256)
  → clock(有理数时间基准, 32 位回绕展开, 漂移/VFR)
  → index(关键帧索引交叉校验, 缺失/过期/重复/越界, 安全重建)
  → align(多表示层共同随机访问切点, 40ms 阈值, 超限显式失败)
  → manifest(稳定排序清单, 边界摘要, 原子替换, 双层 SHA-256)
checkpoint(输入摘要 + 已完成块 + 输出校验和, 中断恢复)
runner/pipeline(并发编排, 取消传播, 内存/文件数限制)
cmd/media-audit(终端适配器)
```

### 关键设计

- **回绕 vs 漂移 vs VFR**：按 RFC 3550 §A.1 展开 32 位时间戳并区分反向
  跳变；滑动窗口最小二乘估计漂移并限幅 ±1000 PPM；确认真实 VFR 后拒绝
  把帧率平滑成恒定值。
- **不静默挪动切点**：任一层无 40ms 内的随机访问帧即标记 `Failure`，
  失败切点不进入清单。
- **可复核恢复**：检查点记录输入大小+哈希与每块校验和；恢复时验证
  schema 版本、自校验与输入摘要，仅复用逐块哈希通过的块，损坏或版本
  不匹配整体回退重扫。
- **原子输出**：清单与检查点均临时文件 + fsync + 回读校验 + 重命名。

## 模块

| 包 | 职责 |
| --- | --- |
| `internal/clock` | 时间基准、回绕展开、漂移/VFR |
| `internal/stream` | H.264/H.265/AV1 裸流扫描与块校验 |
| `internal/sidecar` | go-mp4 ftyp+maud 元数据盒 |
| `internal/fixture` | 确定性素材、损坏注入、回绕/漂移/VFR 场景 |
| `internal/index` | 关键帧索引校验与重建 |
| `internal/align` | 多表示层切点对齐 |
| `internal/manifest` | 分片清单与原子写出 |
| `internal/checkpoint` | 块状态持久化与恢复回退 |
| `internal/runner` | 并发、取消、资源限制 |
| `internal/pipeline` | 端到端编排与中断恢复 |
| `cmd/media-audit` | CLI |

## 约束

- 支持 macOS ARM64 与 x86_64；不调用外部转码器、不读网络素材。
- 不写入仓库外路径（验收临时输出除外）；单任务内存预算 512 MiB。
- 样例数据随代码确定性生成，不引用仓库外素材。
