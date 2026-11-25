# mediad：可恢复的低延迟音视频处理流水线

`mediad` 是一个纯本地 Go 1.25.6 工程，将多码率 DASH、带 discontinuity 的 HLS/fMP4
素材统一到可校验、可恢复、可溯源的处理流水线。所有夹具均由代码用固定 seed 生成，
测试不访问网络或外部样本。

## 快速验收

```sh
go mod download
go build -o /tmp/mediad ./cmd/mediad
go test ./...

d="$(mktemp -d)"
/tmp/mediad demo --out "$d/demo" --seed 41 --rate 48000 \
  --layout stereo --lufs -18
/tmp/mediad verify --root "$d/demo"

/tmp/mediad demo --out "$d/recover" --interrupt-after loudness  # 退出码 75
/tmp/mediad resume --root "$d/recover"                           # resume_match=1

/tmp/mediad demo --out "$d/bad" --damage truncated-mdat          # 退出码 1
```

也可直接运行 `./scripts/acceptance.sh`。

正常验收指标为：

```text
roundtrip=1.000 (12/12)
keyframe_hit=1.000 (48/48)
segment_sha256=9
replay_match=1
output_sr=48000
channels=2
integrated_lufs=-18.0
```

## 退出码

| 码 | 含义 |
|---:|---|
| 0 | 运行和复核通过 |
| 1 | 素材、时间线、编解码或溯源校验损坏 |
| 2 | 参数或本地 I/O 错误 |
| 75 | 已在持久化阶段后中断，可执行 `resume` 续跑 |

## 模块结构

- `internal/manifest/dash.go`：DASH/MPD 与 `SegmentTimeline` 展开。
- `internal/manifest/hls.go`：`EXT-X-MAP`、`EXT-X-DISCONTINUITY`、`EXTINF` 精确有理数解析。
- `internal/manifest/fmp4.go`：ISO-BMFF box 边界、init 段和 AVCC NAL 长度校验；使用 `go-mp4 v1.4.1` 做独立遍历。
- `internal/codec/codec.go`：`AdapterV1` 版本/容器/编码边界，仅接受 fMP4、H.264、PCM16LE。
- `internal/codec/keyframe.go`：H.264 Annex-B/AVCC NAL 与关键帧索引。
- `internal/codec/rtp.go`：16 位序号和 32 位 RTP 时间戳回绕扩展；使用 `pion/rtp v1.10.6`。
- `internal/clock/clock.go`、`timeline.go`：溢出检查的整数有理数和 discontinuity 时间段。
- `internal/audio/`：44.1 kHz/5.1 到 48 kHz 立体声的固定归一矩阵、线性重采样、BS.1770-4 风格 K 加权/门限响度和 4x 真峰值控制。
- `internal/recovery/checkpoint.go`：输入哈希、参数和适配器版本派生的检查点键。
- `internal/recovery/ledger.go`：JSONL 溯源台账，记录源片段/初始化段哈希、参数、组件版本、输出哈希、时间范围、关键帧和 RTP 扩展值。
- `internal/recovery/pipeline.go` 与 `stage_*.go`：分阶段、按片段复用的处理和续跑。
- `internal/fixture/`：自动生成 9 个 fMP4 片段、多表示 DASH、含两个 discontinuity/三个时间段的 HLS，以及损坏夹具。

## 损坏夹具

`--damage` 支持：

- `corrupt-mdat`：改写 AVCC NAL 长度，在 mdat 内精确偏移停止。
- `truncated-mdat`：截断 mdat，打印 `box_offset`、`first_box_offset=0`、`offset` 和 `error_code=E_BOX_TRUNCATED`。
- `truncated-box`：截断首个 `styp` box。
- `missing-init`：删除 discontinuity 后的 `EXT-X-MAP`，错误传播到时间线和恢复校验。
- `wrap-ts`：将 RTP 序号/时间戳放在回绕点附近，媒体时间仍保持单调。

## 恢复一致性

检查点键为：

```text
SHA256(adapterVersion | canonicalParams | orderedSourceHashes)
```

相同输入重试时，已完成片段从 `work/segment-NN.pcm16` 和检查点哈希复用；输入或参数变化会生成新键并使旧台账及下游结果失效。中断后续跑与完整运行的 9 个片段制品、`ledger.jsonl` 和最终 `output/audio.wav` 在测试中逐字节比较。
