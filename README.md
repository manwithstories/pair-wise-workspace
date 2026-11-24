# mediascan

Broadcast media-preparation tool for regional TV playout teams. It ingests
multi-vendor HLS/DASH deliveries, repairs timestamps, rebuilds a random-access
index, and proves the result through content-addressed artefacts and resumable
checkpoints. Everything runs locally; all sample data is generated in-process.

## Quick start

```sh
go mod tidy
go build ./...
go test ./...
go run ./cmd/mediascan demo --seed 41 --drop-every 97 --interrupt-after index --resume
go run ./cmd/mediascan verify --seed 41
```

`demo` prints the acceptance summary; `verify` independently re-derives the
source and output digests after a fresh run.

## Exit codes

| Code | Meaning |
| ---- | ------- |
| 0 | every invariant holds |
| 1 | processing or verification failed (including a deliberate interruption) |
| 2 | invalid arguments |

## Summary fields

`demo` prints, among others:

- `AV_SYNC_MS` — residual audio/video offset measured from the rebuilt index
  (shared origin); the accepted threshold is ≤ 20 ms.
- `LOSS_RECOVERY_PCT` — recovered / (recovered + lost) RTP payloads; ≥ 95 % for
  the normal sample.
- `P95_LATENCY_MS` — 95th percentile per-segment manifest-planning latency;
  ≤ 500 ms.
- `INDEX_POINTS` — sync points in the rebuilt index; must be > 0.
- `CACHE_HITS` — stages served from the content-addressed cache.
- `ERROR_SEGMENTS` — segments quarantined by the repair mapping; > 0 only for
  the damaged sample.
- `SOURCE_SHA256` / `OUTPUT_SHA256` — 64-digit hex digests.

## Architecture

```
cmd/mediascan        demo and verify entry point with exit-code handling
internal/clock       shared nanosecond base, 33-bit unwrap, deterministic repair
internal/manifest    HLS and DASH parsers producing one unified segment plan
internal/repair      RTP sequence analysis, redundancy recovery, gap reporting
internal/probe       audio/video layout and colour validation with policy
internal/index       go-mp4 random-access index rebuild and boundary verification
internal/pipeline    content-addressed cache, checkpoints, atomic commit, run
internal/fixture     deterministic HLS/DASH/MP4/RTP sample generation
```

### Timestamp repair

`clock.Build` runs in two passes: it extends every raw timestamp into a single
monotonic timeline (expanding 33-bit wraps and rebasing on a 0-based programme
origin), then walks an expected grid. A missing timestamp is inferred only when
both neighbours are present, valid and untouched by another hole; runs of holes,
trailing holes and corrupt headers are **quarantined** with their expected grid
slot recorded for attribution — never silently spliced. Duplicates are
re-slotted, small jitter is snapped, and bounded backward jumps are repaired;
anything beyond the limit is quarantined.

### Index isolation

Index points are attributed to a repaired segment by interval. A point that
falls inside a quarantined segment is omitted from the index and counted, so a
truncated or damaged input degrades the index instead of corrupting it.

### Cache and resume

Every intermediate artefact is stored under a key derived from the source
identity, input digest and parameter digest. A cache hit is served only after
re-checking the stage, source, input, parameters and payload digest; any
mismatch deletes the entry and the stage recomputes. Checkpoints record each
stage as `running` or `verified`; a `running` stage is never skipped, so an
interrupted run resumes from the first unverified stage and produces the same
`OUTPUT_SHA256` as an uninterrupted run. Files are written to a temporary name
and renamed, so a crash cannot leave a partial artefact in place of a complete
one.

## Damaged sample

```sh
go run ./cmd/mediascan demo --seed 41 --damaged
go run ./cmd/mediascan demo --seed 41 --truncate-tail 200
go run ./cmd/mediascan demo --seed 41 --damaged --reject   # fails by policy
```

The damaged fixture injects corrupt segment headers, a missing timestamp, a
mid-programme 33-bit wrap, a malformed RTP datagram, an inconsistent audio
channel mask and differing colour metadata. `--truncate-tail` produces the
truncated-index sample; `--reject` turns metadata conflicts into hard failures
instead of normalising them.

## Constraints

- Go 1.25.6; `go-mp4` v1.3.0 and `pion/rtp` v1.8.21 are pinned in `go.mod`.
- No network access, no external transcoder; memory stays well under 512 MiB for
  the fixed 60-second, two-rendition, multi-channel sample.
