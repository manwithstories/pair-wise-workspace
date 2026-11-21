# mediacheck

区域广播机构多机位节目母版与多码率代理轨（360p / 720p / 1080p）的**纯本地**
批量检查、再封装切点规划与可恢复流水线。无图形界面、无外部服务、无网络依赖；
所有 MP4 与 RTP 样本均由 Go 代码确定性自生成。

## 验收流程

```bash
brew install go            # 已安装 Go 1.23+ 可跳过
go mod tidy
go build ./...
go test ./...
go run ./cmd/mediacheck demo \
  --seed 42 --duration 7200s \
  --representations 360p,720p,1080p \
  --segment 6s --checkpoint-every 256 \
  --loss-burst 12 --truncate-tail 1
```

或 `make tidy build test demo`。

### 输出字段与门限

| 字段 | 含义 | 验收门限 |
| --- | --- | --- |
| `av_sync_max_ms` | 音频采样边界相对共享切点的最大残差 | ≤ 20 |
| `packet_recovery_pct` | RTP 乱序/重复/突发丢包后的重组恢复率 | ≥ 85% |
| `p95_segment_latency_ms` | 分段处理 P95 延迟（纳秒计时换算毫秒） | ≤ 500 |
| `segments` | 共享时间轴分片数 | — |
| `aligned_representations` | 成功对齐的表示层数 | = 3 |
| `checkpoint_resumes` | 校验通过的检查点续跑次数 | ≥ 1 |
| `quarantined_ranges` | 无法验证而隔离的区间数 | 截断时 ≥ 1 |
| `manifest_sha256` | 清单摘要，固定种子重复运行恒定 | 字节稳定 |

### 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 生成、统计与独立摘要核验全部通过 |
| 2 | 命令参数或输入不合法 |
| 3 | 不可恢复媒体错误 |
| 4 | 索引写入或恢复校验失败 |

## 架构

```
clock     统一 90kHz 时间轴，纯整数有理数换算，禁止浮点累计漂移
model     跨模块错误码、片段范围、可恢复性与来源引用
fixture   手工编码 ISO BMFF（ftyp/moov/mdat + 完整 stbl/elst）与损坏注入
probe     go-mp4 解析轨道/编辑列表/起始时间戳/索引完整度，边界受限 CRC 校验
segment   共享切点网格、稀疏关键帧边界搜索、表示层有界回退与音频边界吸附
index     临时文件 + 原子替换落盘，256 样本分块检查点、游标与校验和
rtp       pion/rtp 本地乱序/重复/突发丢包样本与序号窗口重组
pipeline  批次调度、上下文取消、检查点恢复与幂等重跑
cmd       终端验收入口
```

关键正确性约束：

- 所有表示层使用**同一组切点**；关键帧稀疏时在窗口内搜索可解码边界，找不到则
  输出带显式起止刻度的缺口，**绝不静默伪造分片**。
- 解析器不越过声明的盒/样本边界读取（`limitedReaderAt`），单样本读取上限
  4 MiB、单文件头部缓存上限 32 MiB（整体远低于 64 MiB 约束）。
- 检查点含版本、输入摘要与游标；重启重算校验和，输入摘要不匹配则拒绝续跑；
  已提交分块与分片不重复计数，清单按输入排序且重复运行字节稳定。
- 容器截断、损坏片段、索引缺页分别使用不同错误码，可恢复前缀写入清单，
  无法验证的尾段标记为 `quarantined`。
- 索引与清单只写入显式工作目录，绝不覆盖输入素材；错误输出对主机路径脱敏。

## 损坏夹具

`internal/fixture` 提供：非零起始时间戳、空/双条目编辑列表、截断尾箱、
伪造 stsz 长度越界、stco 缺页、关键帧间隔超过目标分片时长、单样本 CRC 损坏。
probe 的缺陷会沿 segment/index 两条链路传播，并由独立重算摘要核验恢复结果。
