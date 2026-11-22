# mediapack

蜂窝网络切换、带宽骤降场景下，快速生成**可传输、可续跑**媒体片段的本地流水线。
纯 Go、纯终端、无外部服务或设备依赖，不解码也不重编码媒体。

## 构建与测试

```sh
go mod tidy
go build ./...
go test ./...
```

要求 Go 1.23.0；依赖 `github.com/abema/go-mp4 v1.4.1` 与 `github.com/pion/rtp v1.8.12`。

## 演示

```sh
go run ./cmd/mediapack demo \
  --seed 17 --bytes 1048576 --chunk 4096 \
  --slow-ms 20 --interrupt-after 8 --resume

go run ./cmd/mediapack verify --seed 17
```

损坏路径（退出码 2，输出精确字节偏移）：

```sh
go run ./cmd/mediapack demo --seed 17 --bytes 1048576 --chunk 4096 --corrupt
```

### 输出字段

| 字段 | 含义 |
| --- | --- |
| `parse_error_offset` | 首个损坏字节偏移，正常路径为 -1 |
| `peak_buffer_bytes` | 在途内存高水位，不高于 `--bytes` |
| `resume_ms` | 检查点恢复耗时，目标 < 1000ms |
| `source_sha256` / `output_sha256` | 输入与输出摘要，重跑保持一致 |
| `cache_revalidated` | 缓存命中后是否通过三重核验 |
| `packets` | RTP 校验包数（10² 量级） |

### 退出码

`0` 成功 · `2` 输入损坏（含定位） · `3` 摘要/来源校验失败 · `4` 内部 I/O 或恢复失败。

## 模块

```
internal/clock       有理数时基与 16/32 位回绕
internal/fault       含偏移/轨道/阶段的统一错误
internal/codec       H.264/H.265/AV1 Annex B 解析与关键帧
internal/index       关键帧索引边界校验与软异常
internal/demux       固定字节预算的增量解复用
internal/track       轨道映射、回绕重建与时间线摘要
internal/mux         go-mp4 无损转封装（avcC/hvcC/av1C）
internal/rtp         pion/rtp 包序、重复、乱序、SSRC 校验
internal/cache       来源+输入+内容三重核验的内容寻址缓存
internal/checkpoint  fsync 后原子提交的检查点
internal/pipeline    取消、背压、缓存与恢复协同
internal/fixture     确定性样例与故障注入
cmd/mediapack        命令适配器
```

## 设计要点

- **背压**：字节计数信号量限定在途内存；慢消费者自动暂停生产者。
- **取消**：`context.Context` 同时解除生产/消费 goroutine 阻塞。
- **续跑**：片段落盘并 fsync、目录 fsync 后才原子提交检查点；恢复时已存在
  的片段绝不重写。
- **缓存可信性**：命中也重新核验来源标识、输入摘要与缓存内容摘要，任一不符
  即失效重算。
- **无损转封装**：仅把 Annex B 起始码换成 4 字节长度前缀，编码载荷逐字节保留。
