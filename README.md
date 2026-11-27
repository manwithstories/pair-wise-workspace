# mediacheck

Broadcast programme exchange QC and re-muxing, built as a reusable Go library
with a terminal demo entry point. Suppliers deliver Opus/AAC variable-duration
audio packets next to an H.264 video track; `mediacheck` inspects the packet
structure, separates transport faults from codec damage, fills lost audio
slots with an explicitly labelled placeholder, and writes a deliverable
Matroska file atomically.

Everything runs locally against self-generated material. No external media,
service or credential is referenced.

## Requirements

* macOS (Apple Silicon or Intel) with Xcode Command Line Tools
* Go 1.25.6 (fixed by `toolchain` in `go.mod`)
* FFmpeg 9.x development libraries and pkg-config
* libopus (a Homebrew dependency of FFmpeg, or `brew install opus`)

```sh
brew install go ffmpeg@9 pkgconf opus
```

## Build and test

```sh
GOTOOLCHAIN=go1.25.6 go mod tidy
CGO_ENABLED=1 GOTOOLCHAIN=go1.25.6 go build -o /tmp/mediacheck ./cmd/mediacheck
CGO_ENABLED=1 GOTOOLCHAIN=go1.25.6 go test ./...
```

## Demo

```sh
# Clean sample: exit code 0.
/tmp/mediacheck demo --profile clean --seed 17 --packets 2400 --queue 32 --slow-consumer 1ms

# Damaged sample with a delivered file: exit code 2 (damage found and handled).
/tmp/mediacheck demo --profile damaged --seed 17 --packets 2400 --queue 32 \
    --slow-consumer 1ms --output /tmp/recovered.mkv
ffprobe -v error -show_entries stream=codec_name,duration -of compact /tmp/recovered.mkv
```

The stable output block always contains:

| field | meaning |
| --- | --- |
| `sync_error_ms` | audio/video timeline difference at the end of the run |
| `loss_recovery_pct` | share of lost audio slots that received a placeholder |
| `p95_ms` | 95th percentile per-packet consumer latency |
| `queue_peak` | highest number of in-flight packets |
| `damaged_ranges` | merged damaged spans as `start-end:kind` nanoseconds |
| `output_sha256` | SHA-256 of the delivered file, `none` when not writing |

Additional lines (`packets`, `faults`, `lost_slots`, `recovered_fec`,
`recovered_plc`, `recovered_silence`, `unrecovered`, `keyframe_interval_ms`,
`targets_met`, `exit_code`, capability and injection counters) are diagnostic.

Acceptance targets for the generated samples: `sync_error_ms <= 20`,
`loss_recovery_pct >= 95`, `p95_ms <= 250`, `queue_peak <= 32`. A clean run
has no lost slots, so the recovery target is trivially met.

Exit codes: `0` clean deliverable, `2` damage found and handled, `1` internal
or write failure, `130` cancelled.

### Profiles

| profile | what it injects |
| --- | --- |
| `clean` | nothing |
| `damaged` | dropped audio packets, duplicated packets, a backward PTS |
| `truncated` | audio frames and video access units cut mid-payload |
| `wrap` | sequence numbers renumbered across the 16-bit boundary plus a gap |
| `slow` | clean data read by a deliberately slow consumer |

`--codec aac` generates ADTS-framed AAC-LC instead of Opus. AAC runs report
unfilled slots honestly: FEC/PLC are libopus features and are not applied to a
stream they cannot decode.

## Architecture

| path | responsibility |
| --- | --- |
| `internal/clock/timebase.go` | exact rational media clock and residue-carrying accumulator |
| `internal/model/fault.go` | shared fault kinds, slots and damaged range merging |
| `internal/model/packet.go` | unified packet event and stream descriptors |
| `internal/ingest/source.go` | fixture configuration and hard limits |
| `internal/ingest/encode.go` | real Opus/AAC/H.264 asset generation, ADTS/AVCC framing |
| `internal/ingest/fixture.go` | assembly of packets with exact tick and nanosecond timestamps |
| `internal/ingest/sequence.go` | sequence extension, duplicates, wrap, regressions, gap slots |
| `internal/ingest/reader.go` | bounded packet event replay with fault emission |
| `internal/ingest/profile.go` | deterministic fault injection profiles |
| `internal/inspect/stream.go` | stream statistics, keyframe intervals, damage aggregation |
| `internal/inspect/codec.go` | Opus TOC, AAC ADTS and H.264 AVCC structural parsing |
| `internal/opus/opus.go` | libopus wrapper: encode, decode, FEC, PLC, OpusHead |
| `internal/recover/audio.go` | FEC → PLC → silence slot filling with provenance |
| `internal/pipeline/queue.go` | bounded queue with reverse backpressure and cancellation |
| `internal/pipeline/runner.go` | stage wiring, latency sampling, resource release |
| `internal/remux/writer.go` | transactional Matroska writing and atomic commit |
| `internal/report/metrics.go` | acceptance metrics and exit codes |
| `cmd/mediacheck/main.go` | flag parsing and wiring only |

Data flow:

```
ingest.Reader --Push--> bounded queue --Pop--> inspect --> recover --Emit--> remux
      ^                                          |
      +---------- slow consumer / full queue ----+
```

A full queue blocks `Push`, which blocks the reader's emit callback: that is
the whole backpressure mechanism, and it is why `queue_peak` can never exceed
the configured bound. Cancellation wakes every waiter, the reader stops, the
consumer drains or unblocks, both goroutines are joined, and only then are
libopus and FFmpeg handles released.

### Fault semantics

* **Duplicates** are counted once per retransmission and dropped from the
  carried stream; they never inflate packet counts.
* **Losses** are reported with the exact hole between the previous accepted
  packet and the current one, and recovery fills whole packet-sized slots.
* **Wrap** at 65535 → 0 is reported as an informational fault and does not
  reset the tracker; scanning continues across the boundary.
* **Timestamp regression** is reported with its signed offset and scanning
  continues; the high-water mark only moves forward.
* **Truncated headers and illegal lengths** become damaged slots anchored to
  the packet; the packet is still counted so later packets remain reachable.
  Video access units that cannot be parsed are excluded from the deliverable
  rather than muxed as frames every decoder rejects.

### Recovery honesty

`loss_recovery_pct` counts slots that received *any* placeholder. It does not
claim the original waveform was restored. Per-method counters say which path
produced the audio: `recovered_fec` (Opus in-band redundancy, the only
signal-derived path), `recovered_plc` (libopus concealment), and
`recovered_silence` (a valid quiet frame). Unfillable slots stay counted in
`unrecovered`.

### Reproducibility and atomic output

The fixture encoders are configured bitexact and single-threaded, the muxer
pins creation time and bites exact mode, and the delivered SHA-256 is computed
from exactly the bytes that are renamed into place. Equal seed, profile and
flags therefore produce an identical file hash across runs and processes.

FFmpeg writes into a hidden `*.mkv.part` file in the destination directory.
On success the file is flushed, synced, hashed, renamed and the directory is
synced. On error, cancellation or a cancelled write the temporary file is
removed and `Commit` refuses, so a partially written container is never
visible under the deliverable name.
