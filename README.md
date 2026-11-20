# media_replay

Deterministic offline media-processing pipeline for real-time spectator and
voice traffic under hostile mobile networks (subway, elevator, hotspot
handover). It synthesizes its own H.264/H.265/AV1 and PCM samples, replays a
network trace (duplicates, reordering, burst loss, bandwidth changes), absorbs
jitter, recovers losses, aligns audio and video on one clock, and writes a
chunked, resumable checkpoint index. Everything runs locally with no external
services, network access, camera or microphone.

## Build (macOS, arm64 and x86_64)

```sh
brew install cmake ffmpeg catch2
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
ctest --test-dir build --output-on-failure
```

The FFmpeg integration uses the stable libavformat/libavcodec/libswresample
API and builds against both FFmpeg 7 and FFmpeg 8. Catch2 3.x is discovered via
its CMake package; tests are registered with CTest.

## Acceptance demo

```sh
./build/media_replay demo --codec h264 --seed 41 --loss-burst 7 \
  --bandwidth 1800,700,2400 --checkpoint-dir ./tmp/index

./build/media_replay resume --checkpoint-dir ./tmp/index --inject-interrupt 3
# then resume again with the same parameters to finish the run
```

Reported metrics and thresholds:

| metric | meaning | acceptance |
| --- | --- | --- |
| `sync_error_ms` | P95 audio/video alignment error | <= 40 |
| `recovery_rate` | recovered / attempted loss gaps | >= 0.80 |
| `latency_p95_ms` | P95 jitter-buffer playout latency | <= 250 |
| `packets_duplicate` | duplicates detected and suppressed | informational |
| `packets_reordered` | reordered packets handled | informational |
| `checkpoint_resumed` | durable blocks reused on resume | informational |
| `errors_by_code` | error taxonomy with counts | locatable |

## Exit codes

| code | condition |
| --- | --- |
| 0 | normal completion |
| 2 | corrupt input or index (truncated stream, out-of-range offset, ...) |
| 3 | checkpoint cannot be recovered |
| 64 | command-line / parameter error |

## Commands and flags

```
media_replay demo|resume|fixtures
  --codec h264|h265|av1   --seed N      --loss-burst N
  --bandwidth a,b,c       (kbps, piecewise constant)
  --checkpoint-dir DIR    --inject-interrupt N
  --faulty-index          --truncated
```

`fixtures` generates truncated streams, out-of-range indexes, duplicate and
burst-loss samples and prints the error category together with the affected
block/frame number.

## Module map

| file | responsibility |
| --- | --- |
| `include/media/packet.hpp` | packet, time base and shared error model |
| `src/sample_generator.cpp` | deterministic synthetic streams and audio |
| `src/stream_*.cpp` | raw stream parsing, codec probe, index validation/rebuild |
| `src/network_replay.cpp` | dedup, reorder, burst loss, bandwidth, seq wrap |
| `src/jitter_buffer.cpp` | adaptive bounded depth, reorder, gap detection |
| `src/recovery.cpp` | reference-frame / forward recovery and classification |
| `src/av_sync.cpp` | unified media clock, timestamp policy, resampling |
| `src/checkpoint_*.cpp` | atomic chunked index, verification, rollback resume |
| `src/pipeline.cpp` | end-to-end wiring and metrics |
| `tests/pipeline_tests.cpp` | cross-module and exception-path tests |

All randomness derives from a fixed-seed `mt19937`, so identical input and
network trace produce identical indexes and statistics across runs. Checkpoint
writes use hidden temp files plus atomic rename; temp files are never treated
as valid checkpoints, and a corrupt chunk rolls the resume position back to the
last valid boundary.

---

# mediamux（Go）

远程制作汇聚流水线：流式解析 SRT/WebVTT 字幕与多制式时间码，按统一
90 kHz 有理数时间基准转换，经有界自适应抖动缓冲重排 RTP 包，再把字幕与
音视频时间线对齐，最后用 go-mp4 读取本地 MP4 元数据生成时间对齐清单。
全部样例本地自生成，不联网、无外部服务；相同 seed 必产生相同摘要。

## 构建与验收（macOS，arm64 / x86_64）

```sh
go mod tidy && go build -o /tmp/mediamux ./cmd/mediamux && go test ./... && \
  /tmp/mediamux demo --seed 17 --target-bitrate 2400000 --buffer-ms 180 \
    --memory-mb 32 --slow-consumer-ms 25
```

输出 JSON 行（1 行汇总 + 每夹具 1 行恢复报告）。汇总字段与门限：

| 字段 | 含义 | 门限 |
| --- | --- | --- |
| `bitrate_deviation_pct` | 实际/目标码率偏差 | <= 5% |
| `buffer_peak_bytes` | 抖动缓冲峰值 | < `--memory-mb` MiB |
| `sync_error_ms_p95` | 字幕-音视频 P95 对齐误差 | <= 40 ms |
| `manifest_alignment_ms_max` | 清单最大对齐误差 | <= 1 帧时长 |
| `packet_loss` / `recovered_packets` | 判失 / 乱序恢复包数 | 信息项 |
| `subtitle_valid` / `subtitle_invalid` | 字幕有效 / 可恢复坏记录数 | 信息项 |
| `error_offset` | 首个可恢复错误的原始字节偏移 | 可追溯 |

夹具行报告 `error_offset`（坏记录原始偏移）与 `recovered`（错误之后仍
解析出的有效记录数）。

## 退出码

| code | 条件 |
| --- | --- |
| 0 | 校验通过 |
| 1 | 质量指标超限 |
| 2 | 输入或参数错误 |
| 3 | 不可恢复流水线故障 |

## 模块

| 目录 | 职责 |
| --- | --- |
| `media/` | 统一有理数时间基准（帧率/日界回绕/负偏移）与错误模型 |
| `ingest/` | SRT/WebVTT 流式解析（BOM/CRLF/跨块/单条上限）与多制式时间码 |
| `transport/` | RTP 长度前缀流解析（pion/rtp）与有界自适应抖动缓冲 |
| `pipeline/` | 有界队列多阶段流水线（背压/取消）与同步对齐统计 |
| `manifest/` | go-mp4 轨道元数据读取、时间对齐清单与验证 |
| `testdata/` | 确定性样例与故障夹具生成（丢包/乱序/重复/截断/超长） |
| `cmd/mediamux/` | 终端入口：demo 子命令、质量门限与退出码 |
