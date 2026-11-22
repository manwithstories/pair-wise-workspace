# mediascan

`mediascan` is a terminal-only tool for inspecting local media pipelines:
MP4 sample structure, keyframe intervals, RTP packet sequences, SRT/WebVTT
subtitles, and a unified timeline that commits segments behind an event-time
watermark and deterministically replays late events.

Everything is generated in memory from a seed. No network access or external
assets are used.

## Requirements

- Go 1.23+
- macOS (tested on Darwin/arm64)

## Build and test

```sh
go mod tidy
go build ./...
go test ./...
```

### Dependency version note

The task specifies `github.com/abema/go-mp4 v1.28.0`. That version does not
exist: the module proxy returns 404 for it and the newest published release is
`v1.7.4`. `go.mod` therefore pins **`v1.7.4`**, the latest available release.
`github.com/pion/rtp` is pinned to the requested **`v1.8.11`**.

## Demo

```sh
go run ./cmd/mediascan demo --seed 41 --late-event 1
go run ./cmd/mediascan demo --truncate-mp4 17 --drop-index 2 --late-event 3
```

Output includes:

| Field | Meaning |
| --- | --- |
| `parse_roundtrip` | fraction of cues surviving serialize → reparse (1.000) |
| `keyframe_hit_rate` | fraction of expected keyframes present (≥ 0.98) |
| `segments` | committed timeline segments (order 10^1) |
| `corrupt_ranges` | locatable damaged spans (order 10^0) |
| `recovered_ranges` | intact packet runs after faults |
| `replay_equal` | late-event replays reproduced the canonical run |
| `sha256=` | first 12 hex chars of each segment's digest |

Per-segment rows print `start_ns`, `end_ns`, `recovery` and `sha256`. Re-running
any command reproduces every segment hash.

### Exit codes

- `0` — summary and consistency checks passed
- `2` — input contained localized, recoverable faults
- `1` — processing failed or a consistency check failed

(`go run` collapses a child exit code to 1; use a built binary,
`go build -o mediascan ./cmd/mediascan`, to observe the real 0/1/2 codes.)

## Layout

```
internal/media     nanosecond time base, rational frame rates, SMPTE timecode,
                   unified Severity/Class/Locator/Issue/Range error model
internal/subtitle  SRT + WebVTT parsing and byte-preserving round trip
internal/stream    MP4 sample analysis and RTP sequence analysis
internal/index     keyframe index build/verify/rebuild
internal/timeline  watermark segmentation and deterministic late-event replay
internal/segment   canonical per-segment SHA-256
internal/pipeline  orchestration of all of the above
internal/fixture   deterministic in-memory asset generation
cmd/mediascan      CLI entry point
```

## Design notes

- **Time is exact.** All times are `int64` nanoseconds. Frame-rate arithmetic
  and tick scaling use 128-bit integer ops (`math/bits`); no float is used for
  time accumulation, so repeated runs are bit-identical.
- **Faults are localized.** Every issue carries a severity
  (`info`/`recoverable`/`fatal`) and a locator (track, byte range, packet
  index/sequence, cue index, or time). Damaged samples are isolated to byte and
  time ranges; other tracks continue to aggregate.
- **Replay is deterministic.** Segments are recomputed from the full event set
  behind the watermark, so a late event yields exactly the same boundaries and
  hashes as if it had arrived on time. `RunReplayed` verifies this against an
  independent canonical run.
- **Hostile input is bounded.** Subtitle byte/cue counts, RTP packet count and
  payload size, MP4 input size and declared sample counts are all checked
  before allocation.
