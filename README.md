# mediascan

`mediascan` validates a regional live-distribution chain end to end. The same
program answers three questions for three roles:

- **Transport** — did every manifest-referenced segment arrive intact? Which
  RTP sequence numbers were lost, and were any recovered?
- **Media** — is the MP4 box structure sound, where are the corrupt byte
  ranges, is the keyframe spacing regular, and how far apart are the audio and
  video clocks?
- **On-call** — can an interrupted run resume where it stopped, and does a
  rerun over unchanged inputs reproduce the exact same conclusions?

Everything runs locally on macOS (arm64 and amd64), needs no network or real
transfer node, and reads/writes only the `--work` directory.

## Quick start

```
go mod tidy
go build ./...
go test ./...
go run ./cmd/mediascan demo   --work ./tmp/demo --loss-every 17 --interrupt-after 3
go run ./cmd/mediascan resume --work ./tmp/demo
go run ./cmd/mediascan verify --work ./tmp/demo
```

`demo` generates its own inputs: HLS media playlist, DASH MPD, fragmented MP4
segments, and an RTP capture. Nothing is downloaded. It then scans, analyzes,
writes a checkpoint and appends a JSONL provenance trail. `resume` continues an
interrupted run. `verify` recomputes every digest and re-runs the analysis.

Exit codes:

| code | meaning                                                  |
|------|----------------------------------------------------------|
| 0    | success, or a resumed run completed                      |
| 1    | bad arguments, unreadable inputs, or a failed re-scan    |
| 2    | input or result digest mismatch (stale checkpoint/provenance) |

## What the demo injects

| fault                 | where                                   | expected finding |
|-----------------------|-----------------------------------------|------------------|
| missing media         | segment 7 referenced, file absent       | `missing=1`, `first_missing` names it; partial timeline retained |
| impossible box size   | segment 3 `trun` declares size 3         | `corrupt_ranges>=1`, `box_bounds` fault with byte range |
| truncated file        | segment 4 last 37 bytes removed          | `truncated_box` fault whose range ends at EOF |
| no keyframe           | segment 9 has no sync sample             | `keyframe_interval`, `max_gop_ms` about 6000 |
| A/V clock offset      | audio +10 ms from segment 5, +30 ms at 8 | `av_sync_max_ms=30`, `clock_offsets` |
| RTP loss / reorder    | every 17th packet dropped, two reorders  | `packets_lost>=1`, `loss_recovery_pct`, `duplicates` |

## Architecture

| package            | responsibility |
|--------------------|----------------|
| `internal/model`   | exact rational media clock with overflow checks; the shared fault model |
| `internal/manifest`| HLS + DASH parsing, path confinement, presence checks |
| `internal/index`   | streamed per-segment hashing, partial timelines, resumable index |
| `internal/stream`  | bounded MP4 box validation via go-mp4; RTP reorder/loss analysis |
| `internal/analyze` | A/V alignment on the shared clock, skew classification, GOP measurement |
| `internal/recover` | checkpoint binding, validation and resume position |
| `internal/report`  | reproducible JSONL provenance |
| `internal/pipeline`| end-to-end orchestration and verification |
| `internal/fixture` | deterministic, self-contained demo inputs |
| `cmd/mediascan`    | terminal entry point |

Every timeline value is a normalized fraction, so HLS decimal seconds and DASH
integer ticks compare exactly. Negative durations, zero or oversized timescales
and int64 overflow are rejected at the boundary, not silently wrapped.

## Reproducibility

A run's `result_sha256` is computed from a fixed projection of the
conclusions — missing media, corrupt ranges, A/V maximum, GOP maximum, RTP
counters and the fault list — never from timings or timestamps. A full run and
an `demo --interrupt-after 3` followed by `resume` produce the same result
digest. `verify` recomputes both the input and result digests and exits 2 on
any mismatch, including a provenance file that records a different input.

Latency (`p95_latency_ms`) is measured live and reported separately; it is not
part of the digest.

## Resource limits

- 12 segments at up to 16 MiB each stay under 128 MiB resident; only one moof
  is buffered at a time and hashing streams.
- Manifest size, segment size, box size, sample count and RTP record count are
  all capped.
- Manifest URIs are resolved inside `--work`; absolute paths, `..`, NUL bytes
  and symlink escapes are rejected.
