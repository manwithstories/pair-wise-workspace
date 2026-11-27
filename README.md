# avrepair

Repairs synthetic HLS/DASH capture timelines produced during cellular
handover: unwraps 33-bit PTS clocks, aligns audio against video, rebuilds
single lost packets from grouped XOR parity, quarantines anything that
cannot be repaired with a recorded reason, and commits a random-access
seek index that only references valid sync samples.

The tool is terminal-only. It never opens a window, never resolves a
remote URI and never touches the network.

## Requirements

- macOS 12+ (Darwin arm64 or amd64)
- Go 1.25.6 (the `go` directive in `go.mod` is pinned to that version)

Install Go from the official package, for example on Apple silicon:

```sh
curl -fLO https://go.dev/dl/go1.25.6.darwin-arm64.pkg
sudo installer -pkg go1.25.6.darwin-arm64.pkg -target /
```

Intel users replace the file name with `go1.25.6.darwin-amd64.pkg`.

The only third-party dependency is `github.com/Eyevinn/hls-m3u8 v0.6.5`;
DASH, TS, fMP4, FEC and state all use the standard library.

## Build and test

```sh
go mod tidy
go build ./...
go test ./...
go build -o /tmp/avrepair ./cmd/avrepair
```

## Demo

```sh
/tmp/avrepair fixture --dir /tmp/av-cases

# success path
/tmp/avrepair repair --input /tmp/av-cases/lossy \
    --state /tmp/av-state --output /tmp/av-out

# damaged path (one quarantined segment, exit 2)
/tmp/avrepair repair --input /tmp/av-cases/corrupt \
    --state /tmp/av-bad --output /tmp/av-bad-out

# interrupt then resume (exit 75, then exit 0)
/tmp/avrepair repair --input /tmp/av-cases/normal \
    --state /tmp/av-resume --output /tmp/av-resume-out --interrupt-after index
/tmp/avrepair repair --input /tmp/av-cases/normal \
    --state /tmp/av-resume --output /tmp/av-resume-out --resume
```

`scripts/demo.sh [workdir]` runs all of the above and prints the exit
code after each step.

## Output fields

| field | meaning |
| --- | --- |
| `av_sync_max_ms` | worst audio-video offset after alignment (demo: 0) |
| `loss_recovery_pct` | recovered / missing packets (demo: 100) |
| `p95_segment_ms` | p95 per-segment repair cost (demo: 2.4) |
| `seek_index_entries` | index points, all valid sync samples (demo: 96) |
| `quarantined_segments` | isolated segments with a recorded reason |
| `reused_stages` | stages restored from verified checkpoints on resume |

Exit codes: `0` success, `2` quarantined input, `3` internal error,
`75` clean interruption.

## Architecture

| path | role |
| --- | --- |
| `cmd/avrepair` | command adapter (fixture, repair, verify, seek) |
| `internal/media/clock.go` | rational time base and 33-bit unwrap |
| `internal/media/model.go` | timeline model, fault taxonomy, input guards |
| `internal/manifest/hls.go` | HLS loading and master/variant handling |
| `internal/manifest/dash.go` | DASH MPD representation parsing |
| `internal/manifest/dir.go` | merges HLS and DASH manifests in one case |
| `internal/segment/ts.go` | RTP/TS capture, PES timestamps, sync flags |
| `internal/segment/mp4.go` | fMP4 box parsing, tfdt/trun timing |
| `internal/recovery/fec.go` | order, dedupe, grouped XOR packet repair |
| `internal/graph/plan.go` | track dependency graph and schedule order |
| `internal/journal/store.go` | atomic checkpoints, artifacts, trace, digests |
| `internal/index/seek.go` | sync-only seek map, atomic verified commit |
| `internal/pipeline/` | stage orchestration, normalization, outputs |
| `internal/fixture/` | deterministic sample case generation |

A case directory may contain `index.m3u8` (HLS) and `presentation.mpd`
(DASH); their tracks are merged into one normalized timeline. In the
generated fixtures the HLS track carries a discontinuity and RTP/FEC
captures, while the DASH track carries an independent timescale.

## Faults covered by the generated cases

- 33-bit PTS wrap around segment 4 of every timeline
- A/V clock offset (12 ms) corrected by the median offset
- Repeated boundary timestamps shifted by one sample period
- Single-packet losses rebuilt from grouped XOR parity
- Two-packet loss in one group: segment quarantined, others continue
- Truncated fMP4 `moov` in the corrupt case: exactly one quarantine
- Invalid MPD timescale, out-of-range byte ranges and unsafe URIs
- Truncated committed index on disk: rebuilt on resume
- Tampered checkpoint artifact: hash check fails, stage recomputed

## Safety limits

Per-segment payloads are capped at 64 MiB, manifests at 2 MiB and any
timeline at 12 hours. Remote schemes, absolute paths, traversal and
symlink escapes are rejected before a file is opened. There is no
transcoding and no outbound connection.
