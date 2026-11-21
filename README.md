# mediacheck

六机位电竞赛事后期素材检查工具：MP4 码流扫描、SRT/WebVTT 字幕校验、
多制式时间码换算与可恢复的分片哈希管线，统一使用整数纳秒时间基准
与统一错误模型（可定位、可恢复错误不静默丢弃）。

## 环境

```sh
brew install go@1.23   # 任意 >= 1.23 的 Go 工具链均可
go mod tidy && go build ./... && go test ./...
```

## 运行

```sh
# 正常路径（输入缺失时自动生成夹具，不依赖预置文件）
go run ./cmd/mediacheck -input ./tmp/generated.mp4 -captions ./tmp/generated.vtt \
  -timecode-fps 30000/1001 -queue 4 -chunk 1048576 -checkpoint ./tmp/run.state

# 自检：自生成正常/截断 MP4、错误字幕、RTP 乱序缺包样例，
# 跑正常、损坏、恢复三条路径并校验全部验收指标
go run ./cmd/mediacheck -selftest -report json
```

## 输出

JSON 报告包含 `roundtrip_rate`（≥0.99）、`keyframe_hit_rate`（≥0.98）、
`segment_count`（≥3）、每段 `sha256`（64 位十六进制）、`corrupt_ranges`、
`resume_matches`、`peak_buffer_bytes`（上限 = 队列容量 × 块大小 + 4 MiB
解析缓冲）。正常、损坏、恢复路径均在 stderr 打印计数与错误类别。

## 退出码

- `0`：指标通过
- `2`：输入损坏但报告已成功生成
- `1`：运行或验收失败

注意：`go run` 会把子进程的非零退出码打印为 `exit status N` 并以 1 退出；
需要精确退出码时请使用 `go build` 产物。

## 结构

- `internal/timeline` — 时间码解析（24/25/30/30000-1001 fps、SMPTE 丢帧）、
  帧率换算、跨日回绕、负偏移
- `internal/stream` — MP4 盒/样本表流式扫描（截断标记可信范围）、
  RTP 重排窗口、关键帧索引与损坏区段关联
- `internal/captions` — SRT/WebVTT 解析，保留原始时间文本，报告截断块、
  逆序与越界时间
- `internal/fault` — 统一错误模型（类别、文件偏移/包序号、可恢复性）
- `internal/pipeline` — 有界通道四级管线（读取→分析→字幕对齐→摘要）、
  背压、取消、原子检查点续跑
- `internal/fixture` — 自生成测试夹具
- `cmd/mediacheck` — 终端入口

约束：单字幕块 ≤ 1 MiB；时间线跨度 ≤ 48 小时；队列容量 1–64；
仅支持本地文件与自生成 RTP 字节样例。
