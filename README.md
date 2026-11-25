# mediapipe

A broadcast-grade stream-copy QC and remux pipeline for Matroska/WebM. It
demuxes local files, verifies every packet, applies a deterministic track
mapping, runs an event-time watermark with bounded retraction, and rewrites the
media into Matroska, WebM, or fragmented MP4 — without decoding, transcoding,
or touching an external service.

The terminal entry point (`cmd/mediapipe`) is a demonstration adapter; the
reusable parsing, timeline, flow-control, and muxing cores live under
`internal/`.

## Requirements

- Go 1.25.6 (`go.mod` pins both `go` and `toolchain`)
- `github.com/abema/go-mp4 v1.7.3`
- `github.com/pion/rtp v1.10.6`

```sh
brew install go
go install golang.org/dl/go1.25.6@latest
go1.25.6 download
go1.25.6 mod download
```

## Acceptance run

```sh
go1.25.6 mod tidy && go1.25.6 build ./... && go1.25.6 test ./... && \
go1.25.6 run ./cmd/mediapipe demo --seed 73 --budget-mib 32 --slow-consumer 25ms
```

`scripts/verify.sh` runs the same sequence, asserts every delivery threshold,
and re-runs the demo to prove the replay digests are stable.

## Demo output

```
case=success target_bps=64320 actual_bps=64320 delta_pct=0.0000 payload_hash_mismatch=0 crc_errors=0 manifest_skew_ns=0 buffer_peak_bytes=23736 replay_digest=2d13… exit=0
case=late      … (3 windows recomputed; manifest windows differ from a strict watermark)
case=truncated … corrupt_offset=73550 exit=2
case=beyond-horizon … exit=3
case=resume    resume_digest_equal=true exit=0
case=unsupported … exit=3
```

Thresholds checked by `scripts/verify.sh`:

| metric | limit |
| --- | --- |
| `delta_pct` | within ±0.5 % |
| `payload_hash_mismatch` | 0 |
| `crc_errors` | 0 |
| `manifest_skew_ns` | ≤ 1 000 000 |
| `buffer_peak_bytes` | ≤ 33 554 432 (and ≤ `--budget-mib`) |
| `replay_digest` | identical across repeated runs |
| `resume_digest_equal` | true |

## Exit codes

| code | meaning |
| --- | --- |
| 0 | success |
| 1 | unexpected internal failure |
| 2 | corrupt or truncated input |
| 3 | unsupported mapping, or lateness beyond the retraction horizon |
| 4 | recoverable interruption (resume from checkpoint) |

## Layout

| path | responsibility |
| --- | --- |
| `internal/media` | integer-nanosecond clock, shared track/packet model, unified `Error` with byte offset, track, and recovery class |
| `internal/ebml` | EBML VINT/ID/size reader and writer, unknown-size containers, depth and length limits |
| `internal/demux` | Matroska/WebM block splitting, all lacing modes, unknown-size Segment/Cluster, cluster CRC spans |
| `internal/mapping` | deterministic track routing by number → language → type, codec support per target |
| `internal/timeline` | event-time watermarks (250 ms slack, 2 s retraction), stable-key dedup, replay digests |
| `internal/stream` | byte-budgeted bounded queue with producer backpressure and cancellation |
| `internal/remux` | Matroska/WebM and fMP4 writers, checkpoint flush/resume, output re-verification |
| `internal/rtp` | local RTP arrival-record parser (pion/rtp), loss and discontinuity detection |
| `internal/audit` | manifest assembly and commit gates |
| `internal/checkpoint` | atomic checkpoint store with tamper detection |
| `internal/testkit` | deterministic fixture generator (clean, late, truncated, unknown-length) |
| `internal/integration` | end-to-end tests across the modules |
| `cmd/mediapipe` | terminal demo adapter |

## Design notes

**Stream copy only.** Packets move as opaque byte slices. Remuxers copy payloads
verbatim; the only values rewritten are container timestamps converted through
the shared nanosecond clock. Re-demuxing the output and comparing payload CRCs
is the proof.

**Determinism.** Fixtures are seeded, track mapping is totally ordered, cluster
contents are sorted by `(DTS, track)`, and watermark windows digest events sorted
by `(time, key)`. The same input therefore produces the same bytes, the same
replay digest, and the same manifest.

**Checkpoints.** `Flush` only checkpoints at completed cluster/fragment
boundaries and carries the marshalled SHA-256 state plus byte count. A resumed
run truncates the output back to that boundary and continues; its final digest
matches an uninterrupted run (verified in tests and the demo).

**Bounded memory.** Queue admission is charged in bytes, not items. A slow
consumer blocks the producer, and cancellation drops buffered items and releases
all waiters, so occupancy never exceeds the budget.

## Limitations

- Only local files and self-generated fixtures; no network, no external services.
- No decoding, transcoding, or encryption handling.
- The fMP4 target carries AVC and Opus/AAC video/audio only; subtitles are
  dropped by rule before writing.
- The demo relabels the synthetic AVC track as VP9 for the WebM case because the
  fixture is not real encoded video; the payload path is unchanged.
