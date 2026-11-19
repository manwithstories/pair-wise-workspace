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
