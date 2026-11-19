# mediasync

多机位演出素材的同步、切分、静音检测与跨机位判重工具。纯本地运行，不联网，
所有示例素材由程序生成。

## 环境

- Go 1.23 或更高（在 macOS Terminal 直接可用）
- 依赖通过 `go.mod` 管理：`github.com/abema/go-mp4 v1.3.0`、`github.com/pion/rtp v1.8.19`

```sh
go mod download
go build ./...
go test ./...
```

## 快速演示

```sh
go run ./cmd/mediasync demo \
  --seed 42 \
  --packet-loss-burst 4 \
  --bandwidth-kbps 800,2400,600 \
  --out /tmp/media-report.json
```

终端摘要逐项打印验收字段：

```
roundtrip_consistency : 1.0000
keyframe_hit_rate     : 1.0000
segment_count         : 10
segment_sha256        : ...
duplicate_packets     : 57
reordered_packets     : 108
burst_lost_packets    : 12
errors_with_offset    : 6
```

随后按机位逐片段打印 64 位哈希前缀（`segment_sha256` 的前 16 个十六进制字符），
并列出跨机位重复片段。

## 子命令

| 命令 | 用途 |
| --- | --- |
| `demo` | 生成夹具并跑完整流水线，输出终端摘要与 JSON 报告 |
| `inspect --subtitle a.srt,b.vtt --mp4 a.mp4` | 解析调用方提供的文件，逐条报告问题与原始偏移 |
| `timecode FILE --frames 30000/1001` | 把时间码列表换算到统一时间轴 |
| `version` / `help` | 版本与用法 |

退出码：`0` 成功；`2` 存在可恢复错误；`1` 参数错误或不可恢复输入。

## 目录结构

```
cmd/mediasync/          命令入口（demo / inspect / timecode）
internal/media/         统一错误模型：阶段、输入类型、原始偏移、包序号、可恢复标记
internal/timeline/      帧率、时钟域、128 位换算、SMPTE 时间码、日界回绕、tick 展开
internal/subtitle/      SRT / WebVTT 解析，保留每条记录的原始字节偏移
internal/mp4/           基于 go-mp4 的样本表与关键帧索引，缺项时扫描补建
internal/rtprepair/     有界窗口的重排/去重/缺包统计与令牌桶带宽整形
internal/segment/       按统一时间轴切分，边界落空时标注降级原因
internal/audio/         PCM 静音检测，跨块合并、短缺口不误并
internal/fingerprint/   片段确定性 SHA-256 与跨机位去重
internal/pipeline/      串联各阶段、逐阶段验证、生成 JSON 摘要
internal/testdata/      正常与损坏夹具生成器（无外部素材）
```

## 设计要点

**统一时间基准。** 所有输入先换算到纳秒级时间轴：帧率用确切有理数表示
（`24000/1001` 而非 `23.976`），换算使用 128 位中间乘积并四舍五入，避免
长时间线上的累积误差。时间码支持冒号/分号分隔、丢帧规则、24 小时日界回绕与
重复时间戳。

**错误模型。** 每条问题携带阶段、输入类型、原始字节偏移、记录序号与包序号，
并标明是否可恢复。解析器在遇到坏记录时记录位置后继续处理后续记录。

**有界资源。** 字幕 8 MiB / 10 万条、MP4 64 MiB / 200 万样本、RTP 单包 2 KiB、
重排缓冲与窗口可配置且必定冲刷，静音检测 3200 万帧。超限输入直接被拒绝而不是
尝试分配。

**判重不静默。** 片段指纹只覆盖内容（样本摘要、字幕文本、静音掩码），与文件名、
容器布局、绝对偏移无关；同一个演出内容在不同机位即使偏移完全不同也能命中。
样本越界、数据不完整或时间基无法换算时拒绝生成哈希，并在报告中显式说明原因。

**路径与写入。** 输出只允许落在项目目录或系统临时目录，包含 `..` 组件的路径
一律拒绝；写入通过同目录临时文件 + 原子重命名完成。

## 验收

```sh
go test ./...
```

测试覆盖：

- 字幕：非法时间、重叠、截断、CRLF，逐条核对原始偏移与可恢复性
- 时间码：分数帧率、丢帧、日界回绕、重复时间戳、损坏标签
- MP4：干净索引、关键帧查询、chunk 越界、stss 越界重建、缺 stss、零 delta、截断 moov
- RTP：乱序、去重、突发丢失、序号回绕、窗口外强制冲刷、永久缺口不死锁
- 切分：关键帧吸附、跨缺口降级、尾部覆盖、参数校验
- 静音：跨块合并、短缺口不误并、多采样率、PCM 往返
- 指纹：跨机位命中、异内容不命中、不完整数据禁止判重
- 流水线：8 项验收指标、哈希确定性、JSON 形状、退出码语义
- CLI：验收命令、参数校验、路径穿越拒绝
