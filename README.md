# mediamerge

Deterministic media delivery pipeline for MXF/MP4 camera sources. The tool
generates its own fixtures, normalizes timebase, rotation, crop, colour,
sample rate, channel layout and loudness, streams the result through a
byte-bounded queue, and commits each stage through a write-ahead log with
crash recovery.

Everything runs locally with the Go toolchain; there are no network media
sources and no device dependencies.

## Requirements

- Go 1.23+
- macOS or Linux

```sh
go mod tidy
go build ./...
go test ./...
```

Dependencies are pinned in `go.mod`: `github.com/abema/go-mp4` for MP4 box
encoding/decoding and `github.com/pion/rtp` as the wire carrier for streaming
blocks. Both are used at run time, not as test-only imports.

## Acceptance run

```sh
go run ./cmd/mediamerge demo \
  --seed 42 --chunk-bytes 65536 --consumer-delay-ms 20 \
  --fail-after-stage 2 --resume
```

This performs two passes in one process. The first stops after two committed
stages and leaves an uncommitted staging tail; the second recovers the WAL,
rolls the tail back and finishes the remaining stages.

Expected lines (values from the reference run):

```
PARSE_ROUNDTRIP_RATE 1.000 (6/6)
KEYFRAME_HIT_RATE 1.000 (10/10)
SEGMENT_HASH_MATCH 18/18
WAL_RECOVERED 1
COMMITTED_STAGES 4 parse,audio,mux,final
ROLLED_BACK_BYTES 16384
```

A clean run without `--fail-after-stage` prints the same metrics with
`WAL_RECOVERED 0` and `ROLLED_BACK_BYTES 0`.

## Corrupt inputs

```sh
go run ./cmd/mediamerge demo --seed 42 --corrupt truncated-box
go run ./cmd/mediamerge demo --seed 42 --corrupt truncated-partition
go run ./cmd/mediamerge demo --seed 42 --corrupt non-monotonic
go run ./cmd/mediamerge demo --seed 42 --corrupt bad-crop
```

Each writes a damaged fixture under `<work-dir>/corrupt`, prints a `FAULT`
line with stage, code, track and media range, prints `CORRUPT_LOCATED 1`, and
exits non-zero.

## Exit codes

| code | meaning |
| ---- | ------- |
| 0 | all stages committed and every acceptance threshold met |
| 1 | processing or validation failure (fault printed to stderr) |
| 2 | argument error |

## Other commands

```sh
go run ./cmd/mediamerge verify --work-dir ./.mediamerge
```

`verify` replays the journal, re-checks the committed prefix and recomputes
the parser metrics without writing anything.

Useful flags:

| flag | default | purpose |
| ---- | ------- | ------- |
| `--seed` | 42 | fixture variation seed |
| `--chunk-bytes` | 65536 | streaming block payload size |
| `--consumer-delay-ms` | 20 | simulated slow-consumer latency per block |
| `--fail-after-stage` | 0 | crash simulation after N committed stages |
| `--resume` | false | resume from an existing journal |
| `--work-dir` | `./.mediamerge` | fixtures, journal and delivery file |
| `--memory-limit-bytes` | 1048576 | queue payload budget |
| `--target-rate` | 48000 | audio delivery sample rate |
| `--target-layout` | stereo | audio delivery layout |

## Design notes

- `internal/clock` keeps media time as reduced rationals and refuses inexact
  tick conversions instead of rounding.
- `internal/fault` is the single error vocabulary. Every failure carries a
  stage, an optional track and an optional media span, and is classified as
  fatal, retryable or corrupt.
- `internal/metadata` parses the generated MXF and MP4 carriers, validates
  every box/partition boundary, normalizes rotation to a right angle, checks
  crop against the coded picture, and resolves colour field by field with
  `container < track < explicit` precedence. Conflicts and missing fields
  become structured diagnostics; nothing is silently overwritten.
- `internal/audio` resamples with exact rational interpolation, requires an
  explicit channel mapping, and limits loudness gain at the true-peak
  ceiling with a recorded reason.
- `internal/stream` is a byte-budgeted FIFO. Producers block instead of
  dropping, cancel wakes both sides, and tests assert that no goroutine
  remains blocked.
- `internal/journal` writes CRC-protected records, re-hashes the committed
  prefix on recovery, and publishes each stage through a same-volume staging
  file plus atomic rename.
- `internal/pipeline` measures parse roundtrip and keyframe seek accuracy,
  streams audio in memory-budget-sized windows, and records per-segment
  digests in the journal. Resume replay regenerates the payloads and compares
  hashes, so a resumed run proves reproducibility rather than echo.

Memory during a run stays within the configured queue budget plus the 8 MiB
fixed overhead; `TestMemoryStaysWithinBudget` samples live heap and asserts
it.
