# rcx

`rcx` is a deterministic, terminal-only transcode pipeline for a seeded
narrative-game reel. It generates a lossless luma bundle, probes each segment,
fits a bitrate/QP model, allocates QPs under one total bitrate budget, encodes
segments concurrently, verifies decoded quality, and reports baseline
regressions.

## Requirements

- Go 1.22 or newer
- The only external module is `gonum.org/v1/gonum v0.15.0`

## Quick start

```sh
go run ./cmd/rcx reel gen --seed 7 --segments 12 --out testdata/reel7

go run ./cmd/rcx encode --reel testdata/reel7 \
  --target-bps 1500000 --min-qp 18 --max-qp 44 \
  --concurrency 4 --job out/j1

go run ./cmd/rcx report --job out/j1 --json-out out/j1/report.json

go run ./cmd/rcx encode --reel testdata/reel7 \
  --target-bps 1100000 --job out/j2

go run ./cmd/rcx regress --baseline out/j1 --candidate out/j2 \
  --drop-threshold 0.8
```

Interrupted-job demonstration:

```sh
go run ./cmd/rcx encode --reel testdata/reel7 \
  --target-bps 1500000 --crash-after-segment 4 --job out/j3
go run ./cmd/rcx resume --job out/j3
go run ./cmd/rcx verify --job out/j1
```

The generated reel contains 12 segments, 24 frames per segment, 320x180 luma,
24 fps. Equal seeds produce byte-identical bundles.

## Exit codes

| Code | Meaning |
| ---: | --- |
| 0 | Success |
| 1 | Internal or usage error |
| 2 | Infeasible budget or rate deviation above 2% |
| 3 | Quality regression detected |
| 4 | Corrupt reel/segment bitstream |
| 5 | Interrupted and resumable |

Note that `go run` itself reports a non-zero child status as shell exit code 1;
`go build` the binary when testing exact process exit codes.

## Pipeline

- `reel` uses a deterministic seeded script, fixed little-endian headers, and
  per-frame CRC32.
- `codec` uses intra-only 8x8 DCT, QP step quantization, run-length/Exp-Golomb
  coding, zero-block skipping, frame SHA-256 hashes, and segment/header CRC32.
- `rate` fits `log2(bits)` against QP with gonum least squares, then allocates
  integer QPs under the total budget and per-segment QP bounds.
- A calibration pass re-encodes planned QPs before finalization, using measured
  bits and PSNR anchors to converge within 1% of the requested budget.
- `task` provides an explicit state machine, append-only journal with partial
  tail recovery, bounded concurrency, backoff retries, and cancellation.
- `job` keeps every generated path under the job directory, writes segments via
  temporary files plus atomic rename, and reuses the persisted plan on resume.

## Layout

```text
cmd/rcx/main.go             CLI and exit-code mapping
internal/reel/              bundle format and seeded scene generator
internal/codec/             DCT, quantization, entropy codec, segment checksums
internal/rate/              least-squares model and QP allocator
internal/quality/           PSNR and regression comparison
internal/task/              state machine, journal, scheduler
internal/job/               orchestration, calibration, resume, verification
internal/report/            stable-order JSON report
```
