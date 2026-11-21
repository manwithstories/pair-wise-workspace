# pairflow — 本地电竞多机位媒体处理内核

六机位固定机位帧序列的解析、统一时间轴、乱序重排、关键帧切片、多码率清单、
随机访问索引与交付核验。纯本地运行，无外部服务；所有样例数据由代码生成。

## 安装

```sh
brew install go@1.23 ffmpeg
go version && ffmpeg -version
```

FFmpeg 仅用于本地人工检查生成的容器；流水线本身不调用它。

## 验收

```sh
go mod tidy && go test ./...
go run ./cmd/fixture -out /tmp/pairflow-fixture -frames 1200 -corrupt-every 137
go run ./cmd/mediad -input /tmp/pairflow-fixture -queue 8 -slow-consumer 5ms \
    -bitrates 360p,720p,1080p -out /tmp/pairflow-out
go run ./cmd/mediad -self-check-boundaries
```

`mediad` 输出：

```
roundtrip_rate=0.9933
keyframe_hit_rate=1.000
segment_sha256_checked=600
alignment_error_ticks=0
peak_inflight_frames=24
corrupt_records=48
first_error_offset=1359
```

`-self-check-boundaries` 输出：

```
truncated=1 checksum_mismatch=1 timestamp_wrap=1 duplicate=1
```

退出码：0 全部可核验，1 输入或处理错误，2 参数错误。

## 作为库使用

```go
import "github.com/mediamend/mediamend/internal/run"

rep, err := run.Run(ctx, run.Config{
    InputDir: "/tmp/pairflow-fixture",
    OutDir:   "/tmp/pairflow-out",
    QueueCap: 8,
    SlowConsumer: 5 * time.Millisecond,
    Ladder:   ladder,
})
```

细粒度入口：`pipeline.New(...).Flow`（有界流水线）、`segment.SegmentAll`
（切片与清单）、`index.BuildKeyframe` / `KeyframeIndex.Query`、`verify.Run`。

设计说明见 `docs/DESIGN.md`。
