# Pairflow 本地媒体处理内核

这是 `cmd/mediad` 背后的可复用 Go 库。命令只是验收驱动，所有行为都由
`internal/*` 包提供。

## 数据流

```
fixture (pairflow)
   │  frame 记录 + SHA-256 侧车
   ▼
pipeline.Runner
   parse ──► reorder ──► transform ──► verify
     │          │            │            │
   SHA-256   clock 90kHz  表示层刻度   摘要比对
   偏移       回绕/重置     10MHz       结构化错误
   ▼
segment (关键帧对齐切片 + 多码率清单)
   ▼
index (关键帧随机访问索引)
   ▼
verify (分片哈希 / 清单往返 / 对齐 / 查询命中)
   ▼
run.Run -> Report (mediad 打印的验收键)
```

## 关键不变量

| 不变量 | 实现位置 | 测试 |
| --- | --- | --- |
| 帧记录自定界，损坏记录带原始偏移 | `internal/frame` | `frame_test.go` |
| 时间戳统一 90kHz，回绕/重置/重复可分类 | `internal/clock` | `pipeline` 集成测试 |
| 乱序窗口外迟到帧不插入错误位置 | `internal/rtp` | `reorder_test.go` |
| 队列与载荷双重限界，取消保留已提交项 | `internal/pipeline` | `runner_test.go` |
| 各码率片段起止时间一致 | `internal/segment` | `writer_test.go` |
| 索引查询必须落到实际分片与帧偏移 | `internal/index` | `keyframe_test.go` |
| 分片哈希/清单往返/对齐核验 | `internal/verify` | `manifest_test.go` |
| 验收键与退出码 | `cmd/mediad` | `main_test.go` |

## 内存模型

- 帧载荷在 parse 阶段完成 SHA-256 后立即释放，跨阶段只传递描述符；
- `MaxInflight` 帧槽位 + `MaxPayloadBytes` 字节预算；
- 重排窗口按帧数与字节双重限界；
- 单帧长度上限 `frame.DefaultMaxRecord`（8 MiB）。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 全部可核验 |
| 1 | 输入或处理错误（含取消后的不完整交付） |
| 2 | 参数错误 |
