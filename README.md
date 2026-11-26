# tsrestore — 影视资料馆 RTP/RTCP → MPEG-TS/HLS 恢复工具

纯本地、纯 Go（Go 1.25.6，标准库 + `github.com/Eyevinn/hls-m3u8 v0.6.5`）的
旧采集节点长期节目素材恢复流水线。输入为二字节长度前缀的 RTP/RTCP 数据报，
RTP 承载 RFC 2250 MPEG-TS（90 kHz H.264），输出对齐的多码率 HLS 清单与 TS 片段。

## 构建与测试

```sh
go mod tidy
go build ./...
go test ./...          # 全部核心包独立测试，-race 亦可
```

## 验收演示

```sh
go run ./cmd/restore verify \
  --seed 41 --loss 0.02 --reorder 9 --slow-consumer 8ms \
  --resume --hashes --out "$TMPDIR/archive-demo"
```

终端输出含 `normal`、`damage`、`resume` 三段汇总，全部断言通过退出码 0：

- `roundtrip=100.00%`、`keyframe_hit=100.00%`、`aligned=100.00%`
- `segments=120`（两路码率 × 60 个六秒片段，节目时长 360 秒）
- `peak_buffer_bytes ≤ 16777216`
- 损坏路径含 `isolated=<n>` 与 `recovered=ok`
- 续跑含 `cache_reject=1`、`hash_match=100.00%`
- `--hashes` 逐片段打印 `segment_id` 与 64 位 sha256；重复运行与续跑哈希一致

退出码：`0` 全部断言通过；`2` 不可恢复输入；`3` 来源或检查点完整性错误。

边缘样例：

```sh
go run ./cmd/restore fixture --out "$TMPDIR/fx" truncated   # 截断数据报
# bad-rtcp / tamper-cache / slow-consumer 由 verify 流程内自动生成并验证
```

## 包结构

| 包 | 职责 |
| --- | --- |
| `internal/timeline` | 统一时基：RTP 32 位时间戳回绕扩展、RTCP SR→NTP 映射、Gap/Corrupt/Retryable/Fatal 错误分类 |
| `internal/rtp` | 二字节长度前缀分帧、RTP/RTCP 解析、有界重排（回绕、去重、迟到、缺口） |
| `internal/sample` | 固定种子生成两路码率 TS/RTP/RTCP，注入丢包、乱序、重复及边缘样例 |
| `internal/segment` | 0x47 失步重同步、IDR 检测、六秒切片、45ms 对齐预算、discontinuity |
| `internal/cache` | 内容寻址缓存：源标识 + 来源摘要 + 输出 SHA-256 三重校验，不符即作废重建 |
| `internal/checkpoint` | 落盘+fsync+验哈希后才推进；HMAC 校验和防篡改；续跑不重复序号/不改哈希 |
| `internal/pipeline` | 16 MiB 字节信号量背压、慢消费者不静默丢片、context 取消与续跑 |
| `internal/hls` | Eyevinn m3u8 生成 master/media 清单，discontinuity 标签与写盘往返 |
| `cmd/restore` | 终端命令适配、normal/damage/resume 汇总、断言与退出码 |

## 关键约束

- 样例随代码确定性生成，不读取任何外部素材，无外部服务依赖。
- 在途内存硬上限 16 MiB，由条件变量信号量在并发压力下保证（见
  `internal/pipeline/ceiling_test.go`）。
- 缓存命中必须同时通过源标识、来源摘要、输出 SHA-256 三项校验。
- 检查点仅在片段原子落盘、fsync 并复核哈希后推进。
