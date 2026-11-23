# mediasync

Collects studio, field and edit-proxy MXF/MP4 multi-track material into one
media package an editing system can ingest. The tool demultiplexes coded
samples without re-encoding, resolves track mappings, reconciles display
metadata (rotation, clean aperture, colour), recovers RTP input across
reordering and loss, checkpoints progress so an interrupted run can resume,
and writes a timeline manifest plus hash manifests that prove the samples did
not change in transit.

It is a terminal-only program: all output is `key=value` lines on stdout, and
the exit code carries the failure class. It performs no network access and
depends on no external sample files — every input is generated deterministically
from a seed.

## Requirements

- macOS (arm64 or amd64)
- Go 1.25.6 (`brew install go@1.25.6`)

`go.mod` pins the Go directive to `1.25.6` and fixes dependency versions.

## Build and test

```sh
brew install go@1.25.6
go mod tidy
go build ./...
go test ./...
```

## Demo

The acceptance run builds every fixture in a scratch directory, imports the
programme, interrupts once and resumes from the checkpoint store:

```sh
go run ./cmd/mediasync demo \
  --seed 41 --loss-burst 7 --duplicate 3 --reorder 5 \
  --bandwidth-kbps 1800 --target-kbps 2000 \
  --checkpoint-samples 120 --resume
```

Sample output:

```
interrupted_after_blocks=2
interrupt_hash=5c3e011d...
track_count=2
sample_count=780
target_kbps=2000
actual_kbps=1945.5
bitrate_error_pct=2.73
sample_sha256_match=true
quality_mean=0.974727
quality_p95=0.979112
manifest_alignment_ms=0.000000
checkpoint_blocks=7
resumed_blocks=2
rtp_received=56
rtp_duplicates=3
rtp_out_of_order=37
incomplete_intervals=1
track=name:program-video id:1 kind:video samples:300 bytes:2004999 sha256:b6278011...
track=name:program-audio id:2 kind:audio samples:480 bytes:485235 sha256:39de163e...
bitrate_note=within 10% tolerance (2.73%): coded size and fixture dithering account for the gap
final_hash=e0a36838cd1f6f3f2ca7426a3a65739486a6ae67837cc633e2000c051389abf0
warning=1 incomplete RTP interval(s); no samples were fabricated to fill them
```

The `--resume` run reaches the same `final_hash` as an uninterrupted run, and
`resumed_blocks=2` of `checkpoint_blocks=7` shows the second pass continued from
intact state rather than restarting.

### Damaged input

```sh
go run ./cmd/mediasync demo --corrupt truncated-mp4
```

```
error_code=TRUNCATED_BOX
offset=4228
error_message=box "mdat" declares 2490242 bytes, 1243023 available
checkpoint_recovered=0
```

`--corrupt invalid-klv` reports `INVALID_KLV` the same way.

## Exit codes

| code | meaning |
|------|---------|
| 0 | normal completion |
| 1 | parameter error (bad flag, out-of-range value, escaping path) |
| 2 | data error (truncated box, invalid KLV, mapping/crop failure) |
| 3 | unrecoverable (unreadable package, non-finite statistics) |

### A note on `go run` and exit codes

`go help run` states: *"The exit status of Run is not the exit status of the
compiled binary."* In practice `go run` exits `1` whenever the child is non-zero,
replacing the real status. The program therefore also prints a final
`exit_code=N` line on stdout so the true status is machine-readable either way.
To observe the process status directly, build first:

```sh
go build -o mediasync ./cmd/mediasync
./mediasync demo --corrupt truncated-mp4; echo $?   # 2
```

## Flags

| flag | default | meaning |
|------|---------|---------|
| `--seed` | 41 | deterministic fixture seed |
| `--loss-burst` | 7 | RTP packets lost in one interior burst |
| `--duplicate` | 3 | RTP packets transmitted twice |
| `--reorder` | 5 | RTP reordering distance in packets |
| `--bandwidth-kbps` | 1800 | seed for the per-block rate estimate |
| `--target-kbps` | 2000 | declared bitrate target (never applied to samples) |
| `--checkpoint-samples` | 120 | samples per checkpoint block |
| `--resume` | off | interrupt once, then resume from the checkpoint store |
| `--corrupt` | none | `truncated-mp4` or `invalid-klv` |
| `--rotation` | unset | explicit rotation override (0, 90, 180, 270) |
| `--work-dir` | temp | keep the generated inputs and package on disk |

## Packages

| package | responsibility |
|---------|----------------|
| `internal/clock` | rational time base, exact rescaling, rounding rules |
| `internal/model` | track, sample, display-metadata and error vocabulary |
| `internal/mxf` | KLV framing, partition packs, primer, essence import |
| `internal/mp4` | box walker, sample tables, colour/clean-aperture parsing |
| `internal/map` | explicit → track-id → type mapping with hard failures |
| `internal/metadata` | rotation/crop/colour arbitration with provenance |
| `internal/rtp` | sequence-wrap ordering, duplicate counting, explicit gaps |
| `internal/bandwidth` | per-block bitrate estimate used only for sizing |
| `internal/checkpoint` | self-describing atomic blocks, chained hashes, index rebuild |
| `internal/remux` | byte-exact package writer and verification |
| `internal/manifest` | rational timeline and alignment statistics |
| `internal/fixtures` | deterministic MP4/MXF/RTP inputs and damaged variants |
| `internal/pipeline` | stage wiring, resume, reporting |
| `internal/cli` | flag parsing and validation |
| `cmd/mediasync` | terminal entry point |

## Notes on scope

- **go-mp4 version.** The brief asks for `go-mp4 v1.29.0`; that version does not
  exist upstream. The latest published version is `v1.7.4`, which `go.mod` pins.
  `pion/rtp v1.8.11` is pinned as requested.
- **RTP sample identity.** Each RTP packet carries one coded sample. Duplicates
  are counted once, reordering is repaired against a circular sequence base, and
  a loss burst becomes an explicit incomplete interval; no media is fabricated.
- **Bitrate.** The budget is measured per block and reported against the target,
  but it never rewrites, truncates or pads coded samples. Target and actual rates
  are reported separately, with a note naming the difference.
- **Display metadata.** Rotation resolves explicit > track > container; crops
  that would remove the whole frame are rejected; unknown colour codes are
  preserved verbatim with a default interpretation and a warning.
- **Memory.** No single sample, block or package file exceeds 64 MiB; inputs that
  would exceed it are refused with `SIZE_LIMIT`, and paths cannot escape the
  working tree.
