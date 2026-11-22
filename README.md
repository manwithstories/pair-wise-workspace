# medialine

`medialine` is a reusable batch quality-verification and repackaging kernel for
multi-track broadcast material. It ingests MP4 fragments and RTP timestamp
samples, normalises every track onto one microsecond timeline, aligns a bitrate
ladder on common switch points, repackages per-segment files deterministically,
and emits a traceable manifest with per-fragment evidence.

Everything is local and self-contained: fixtures are generated from a seed, no
external service is contacted, and only local files are read and written.

## Requirements

- Go 1.23
- macOS (tested on Darwin arm64) or any Linux/macOS terminal

```sh
brew install go@1.23
go mod tidy
go build ./...
go test ./...
```

## Demo

```sh
go run ./cmd/medialine demo \
  --seed 17 --tracks 4 --bitrates 3 --segment-us 2000000 \
  --queue 4 --workers 3 --corrupt tail --cancel-after 5 \
  --out ./tmp-run

go run ./cmd/medialine verify --out ./tmp-run
```

The demo generates source clips under `<out>/src`, walks the pipeline, cancels
mid-run, resumes over the unfinished fragments, and prints:

```
roundtrip_rate=1.000
keyframe_hit_rate=1.000
segment_hash_match=18/18
aligned_boundary_us=0
peak_inflight=7 (limit=7)
recovered_segments=18
completed_segments=18 total_segments=18
status=recovered
status=partial
manifest=tmp-run/manifest.json
```

| field | meaning |
| --- | --- |
| `roundtrip_rate` | share of delivered fragments that re-parse to the same sample list (target 0.98–1.00) |
| `keyframe_hit_rate` | share of committed boundaries that land on a keyframe in every representation (target 0.95–1.00) |
| `segment_hash_match` | verified fragments / completed fragments |
| `aligned_boundary_us` | residual spread of the effective boundary across representations (0 = perfectly aligned) |
| `peak_inflight` | largest number of accepted-but-unfinished jobs; structurally bounded by `queue + workers` |
| `recovered_segments` | valid fragments after the resume pass (each counted once) |

Exit codes:

- `0` — success
- `2` — input was damaged but usable fragments were produced under the recovery policy
- `1` — bad parameters or an unrecoverable write error

`--corrupt` accepts `none`, `tail` (lost trailing index), `length` (oversized
box declaration) and `sample` (tampered payload marker). `--cut-us` may be
repeated to add operator edit points on the timeline.

## Architecture

```
cmd/medialine/            flag parsing, demo/verify orchestration
internal/clock/           microsecond timebase, wraparound and RTP timeline
internal/model/           tracks, samples, cuts, transitions, segments
internal/fault/           shared stage/track/offset error model
internal/mp4/             bounded streaming reader, deterministic writer
internal/index/           keyframe index and snapping
internal/align/           common switch boundaries across bitrates
internal/manifest/        manifest entries, composites, hashing, atomic write
internal/pipeline/        bounded worker pool, stable merge, cancellation
internal/verify/          per-fragment hash and round-trip parse
internal/testkit/         self-generated normal, corrupt and boundary fixtures
```

Data flow:

```
MP4/RTP ──parse──> clock + model ──> keyframe index ──> align ──> manifest
                                                          │
                              worker pool ──> writer ──> hash ──> verify
```

### Bounded memory

- The reader validates every box size against `MaxBoxSize` before seeking and
  reads sample payloads only on demand.
- `StreamSamples` delivers payloads over a fixed-capacity channel, so a slow
  consumer blocks the producer instead of growing a queue.
- The worker pool accepts at most `queue + workers` outstanding jobs; the demo
  reports the observed peak against that limit.

### Determinism

- Segment IDs are derived from representation, track and aligned boundary.
- Merged results are sorted by `(start, track, representation, id)`, never by
  completion order.
- Segment payloads are packed in plan order, so identical plans produce
  byte-identical files and hashes.

### Faults and recovery

Every fault carries its stage, kind, track and byte offset. Recoverable faults
(`truncated_box`, `invalid_length`, `corrupt_sample`, `missing_index`,
`ambiguous_time`, `duplicate_time`, `backwards_time`, `zero_duration`) let the
run continue on other fragments. A failed fragment is recorded under
`failures` in the manifest and never listed as a deliverable entry.

On cancellation the pipeline drains accepted jobs, closes files, writes the
partial manifest, and the demo then resumes only the unfinished fragments, so
completed work is counted exactly once.

## Manifest format

`manifest.json` is versioned and records the segment length, ladder metadata,
transition regions, entries (`status: complete`, `content_hash`) and failures.
`verify` re-reads it, recomputes each hash, and re-parses each fragment.
