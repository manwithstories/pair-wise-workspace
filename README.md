# mediarun

A reusable media-recovery and transaction library for a touring-production
workflow, plus a small CLI used for acceptance and demonstration.

Each camera uploads HLS or DASH manifests and media segments from an unstable
venue network. Some segments arrive late, some are genuinely missing, some are
corrupt, and cameras' clocks drift. `mediarun` distinguishes those cases,
records what it decided, and proves that an interrupted run recovered to the
same bytes a full clean rerun produces.

## Quick start

```sh
go mod tidy && go mod download
go build ./...
go test ./...
go run ./cmd/mediarun demo --seed 42 --fault reorder,truncate,drift --resume
```

The demo generates all sample data itself. Nothing is downloaded and no
network service is contacted.

## Packages

| Path | Responsibility |
| --- | --- |
| `internal/manifest` | HLS media playlists and DASH `SegmentTimeline` → one validated segment model |
| `internal/clock` | Integer-nanosecond axis, RTP-style wrap/duplicate/regression detection, drift fitting, unified error taxonomy |
| `internal/rtp` | Sequence reordering, bounded retransmit, explicit final gap records |
| `internal/rtp/wire` | Real RTP datagrams via `pion/rtp`, 16-bit sequence extension |
| `internal/buffer` | Adaptive jitter buffer with memory budget and immutable committed intervals |
| `internal/sync` | Audio↔video alignment with traceable corrections and threshold stops |
| `internal/index` | MP4 keyframe index, every offset verified against media data |
| `internal/txn` | Write-ahead log, signed records, atomic checkpoint/commit, rollback |
| `internal/provenance` | Output → segment → manifest → input-hash lineage |
| `internal/pipeline` | Wires the stages and drives them through the transaction log |
| `internal/fixture` | Deterministic sample generation (manifests, RTP, MP4) |
| `cmd/mediarun` | Terminal entry point |

## Behaviour that matters

- **Unified time base.** Every timestamp is an `int64` nanosecond count. A
  `clock.TrackClock` unwraps 32/33-bit counters, flags wraps separately from
  duplicates and from regressions, and never moves the cursor backwards.
- **Refusing to guess.** `clock.FitDrift` performs ordinary least squares but
  returns a `KindUnrecoverable` error when the slope is not uniquely
  determined (too few points, duplicate reference times, no baseline, or
  residuals outside the bound). Ambiguity is surfaced, never silently applied.
- **Late vs missing.** The reorderer tolerates duplicates, holds a gap open
  for a configured window, attempts bounded retransmission, and records every
  run that survives that window as an explicit `Gap`. A packet arriving after
  its slot was finalized is counted as late rather than rewinding output.
- **Immutable committed intervals.** The jitter buffer freezes an interval
  once the commit horizon passes. A later packet is counted, not merged.
- **Alignment is auditable.** Each interval gets one median-based correction
  with the anchors it was derived from. Exceeding `MaxCorrectionNS` or lacking
  enough anchors produces a typed error that stops that interval's commit.
- **Transactions.** Each stage writes a `.part` file, hashes it, renames it to
  a verified artefact, appends a signed checkpoint record, and only then
  derives any final file. A failure removes the partial file and records a
  rollback; no reader can ever observe a half-written final manifest.
- **Resume equals rerun.** Replay discards a torn trailing record and any
  record whose own hash does not verify. A resumed run reuses verified
  artefacts, so the final manifest is byte-identical to the clean rerun's.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | all invariants hold |
| 2 | input or manifest rejected |
| 3 | timeline unrecoverable |
| 4 | recovery or hash verification failed |

## Sample summary fields

`roundtrip_rate`, `keyframe_hit_rate`, `segment_hash_match`, `recovered_gaps`,
`final_gaps`, `rollback_count`, `resume_hash_equal`, `provenance_complete`,
`keyframes`, `committed_intervals`, `segments`, plus `drift_slope_ppb` when a
drift fit was requested.

## Limits and budgets

- One manifest: 100,000 entries; one segment: 256 MiB.
- 10-minute, three-camera sample: well under the 2-minute / 512 MiB budget on
  a 4-core Mac (`internal/pipeline/perf_test.go`, `memory_test.go`).
- Everything runs locally; build artefacts are not committed.
