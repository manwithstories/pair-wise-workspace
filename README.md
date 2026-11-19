# archivist

A local-only command line tool for restoring cross-era news-reel and
documentary masters. It reads an edit-decision timeline, the MP4 masters it
points at, and an RTP capture of the same material, then decides which cuts can
be reconstructed losslessly, which byte ranges are damaged, and how far each
recovered boundary sits from the intended one.

Nothing is transcoded and no source file is modified. The fixtures are generated
from a fixed seed, so every number below is reproducible.

## Build and test

```sh
go mod tidy
go build ./...
go test ./...
```

Requires Go 1.23 or newer. The only dependencies are
`github.com/abema/go-mp4` and `github.com/pion/rtp`; there is no network or
service dependency at run time.

## Quick start

```sh
go run ./cmd/archivist generate --out /tmp/archive-fixture
go run ./cmd/archivist inspect --input /tmp/archive-fixture/timeline.json \
    --capture /tmp/archive-fixture/stream.rtp
go run ./cmd/archivist recover --input /tmp/archive-fixture/timeline.json \
    --capture /tmp/archive-fixture/stream.rtp --iterations 30 \
    --json /tmp/archive-fixture/result.json
```

`generate` writes:

| file | contents |
| --- | --- |
| `timeline.json` | four tracks, six clips, two crossfades |
| `timeline-cycle.json` | same media, audio tracks depend on each other |
| `stream.rtp` | recorded packets with loss, reordering, duplicates and one 33-bit wrap |
| `video.mp4` | 240 frames at 30 fps, keyframe every 10, one zero-filled sample |
| `audio.mp4` | 8 s of 48 kHz audio bed |
| `proxy.mp4` | 24 VFR frames with deliberately uneven durations |
| `master.mp4` | 16 s long-form audio master |
| `proxy-truncated.mp4` | the proxy cut short mid-sample |
| `manifest.json` | the planted facts, so a reviewer can check the verdicts |

### Commands

```
archivist generate --out DIR [--seed N] [--video-frames N]
archivist inspect  --input timeline.json [--capture stream.rtp] [--json FILE]
archivist recover  --input timeline.json [--capture stream.rtp] [--iterations N] [--json FILE]
archivist bench    --input timeline.json [--capture stream.rtp] [--iterations N] [--json FILE]
```

`recover` and `bench` clamp `--iterations` to at least 30 and report latency
percentiles over that many full pipeline runs. `inspect` and single-shot
`recover` report the wall-clock latency of their one run.

Exit codes:

| code | meaning |
| --- | --- |
| 0 | processing completed |
| 2 | invalid arguments or input |
| 3 | an unrecoverable segment was found |
| 4 | internal error |

## Acceptance

The three-check sequence prints a pass/fail flag next to every measured value:

```
$ go run ./cmd/archivist recover --input /tmp/archive-fixture/timeline.json \
      --capture /tmp/archive-fixture/stream.rtp --iterations 30 \
      --json /tmp/archive-fixture/result.json
summary tracks=4 corrupt_ranges=1 recovered_ranges=1 unrecoverable_ranges=0
acceptance tracks=4 corrupt_ranges>=1:true wraps=1 wraps>=1:true constant_fps=false
acceptance sync_error_p95_ms=1.776 sync_error_p95_ms<=20:true
acceptance packet_recovery_rate=0.954 packet_recovery_rate>=0.90:true
acceptance latency_p95_ms=4.073 latency_p95_ms<=250:true (iterations=30)
metrics keyframe_snap_p95_ms=16.666 crossfade_skew_p95_ms=0.000 boundaries recovered=12 blocked=0
```

The generated capture wraps the 33-bit clock once (`wraps=1`) and its clock
drifts 120 ppm, which exceeds the 80 ppm review threshold, so the report
carries a `clock_drift` fault and lists `V1` under `drift_review_tracks`. The
proxy track is VFR, so `constant_fps=false`.

`inspect` also prints the per-track sample table summary, keyframe-gap
percentiles and the corrupted byte intervals:

```
  video.mp4 brand=iso6 bytes=217626 tracks=1
    track=1 kind=vide codec=avc1 timescale=90000 samples=240 keyframes=24 vfr=false
    keyframe_gap_ticks p50=30000 p95=30000
    corrupt bytes=[112326,113225]
  proxy.mp4 brand=iso6 bytes=8530 tracks=1
    track=3 kind=vide codec=avc1 timescale=90000 samples=24 keyframes=4 vfr=true
    keyframe_gap_ticks p50=23250 p95=25500
```

Run `inspect` against `timeline-cycle.json` to see the cycle path; it exits 3
with `dependency_cycle` faults while the independent video and proxy branches
are still recovered.

## How it works

```
timeline.json ──► model.Timeline ──┐
MP4 masters   ──► mp4.Scan        ─┼─► graph.Build ──► recover.Run ──► report.Metrics
stream.rtp    ──► rtp.Reorder     ──┘        ▲              │
                     │                        │              ▼
                     └──► clock.Unwrap ───────┘        CLI digest / JSON
```

`internal/pipeline` wires these stages; each package is independently tested.

### Time base

All timeline arithmetic uses `model.Rational`, an integer fraction kept in
lowest terms. Multiplication and addition detect `int64` overflow and fall back
to arbitrary precision, so no duration ever accumulates floating-point error.
Microseconds are the only presentation unit, and even those are integer.

### Clocks (`internal/clock`)

Timestamps are unwrapped against a configurable modulus (2^33 by default). The
detector holds a high-water mark of normalized time: a backward hop larger than
half the modulus advances the epoch, while a large forward hop is recognized as
a late packet from the previous epoch and normalized backwards without
disturbing the epoch, so one wrap is never counted twice.

Drift is the least-squares slope of arrival time against drift-corrected media
time, computed in exact rational arithmetic and rounded to 0.001 ppm. Only
packets that arrive in media order take part; a reordered packet's arrival lag
is transport jitter, not clock skew. Drift beyond 80 ppm raises a review flag
and the track is still processed, because the normalized clock remains
monotonic.

The capture stores the full 33-bit clock in an RTP header extension, since the
RFC 3550 timestamp field is only 32 bits.

### Packets (`internal/rtp`)

A bounded receive window accepts packets that trail the high-water sequence
number by at most the window size. Duplicates are dropped, in-window reordering
is repaired, and a packet arriving outside the window is counted as
unrecoverable loss. The reported recovery rate is
`accepted / (accepted + missing)` over the observed sequence span, and every
missing sequence number is listed.

### Media (`internal/mp4`)

The box walker is hand-written rather than delegated to the library's probe so
that truncation becomes a located fault instead of an error. Sample offsets,
decode times and durations are rebuilt from `stts`/`stsc`/`stco`/`stsz`.

- A sample with no usable duration is isolated: it does not advance the
  timeline and is reported, never filled in with an assumed frame rate.
- VFR is simply the per-sample durations; boundaries snap using them.
- A sample is corrupt when its bytes fall outside the file or when the probed
  prefix (at most 8 KiB) is dominated by a zero run. Payloads are
  pseudo-random, so a zero run is unambiguous damage.
- The scanner is fuzzed: any byte sequence yields faults, never a panic.

### Dependencies and recovery (`internal/graph`, `internal/recover`)

Tracks, clips and crossfades form one directed graph. Tracks depend on other
tracks; clips depend on their track and their parent clip; a crossfade depends on
both of its sides.

Recovery runs in phases:

1. Each track must normalize its clock and verify at least one decodable
   keyframe. A track with no readable media fails this gate.
2. Cycle members and tracks with an unrecoverable range are marked failed, and
   that failure propagates only along their outgoing edges.
3. Clip edges are recovered in dependency order. An in-point snaps to the last
   decodable keyframe at or before it; an out-point snaps to the first keyframe
   at or after it, falling back when the clip runs past the last keyframe.
   Snap distance is reported per edge.
4. A damaged run is bracketed by searching outward for decodable keyframes. The
   report records the original byte offsets of both brackets, the recoverable
   byte interval, and an error bound derived from the distance between the
   damage and its brackets. If neither side has a decodable keyframe the range
   is fatal and the process exits 3.
5. Crossfades compare the recovered media positions of both sides and report
   the skew.

### Memory

The scan reads at most one sample prefix at a time and caps clock history in the
JSON at 4096 timestamp pairs. A full acceptance run peaks at roughly 31 MiB
resident, well inside the 512 MiB budget.

## JSON output

`--json` writes indented JSON with stable field order. `recover` writes the full
analysis document:

- `timeline` — the parsed input
- `scans` — per-file tracks, samples, keyframe gaps and corrupt ranges
- `capture` — received/accepted/duplicate/reordered/late counts and missing
  sequence numbers
- `clocks` — per-track wraps, drift, review flag, maximum skew, and the retained
  original/normalized timestamp pairs
- `plan` — ranges, boundaries, crossfade verdicts, per-track states and faults
- `metrics` — the acceptance numbers
- `faults` — every located fault

`bench` writes `{command, bench, metrics, faults}`, where `bench` carries the
latency samples and percentiles.

## Fault codes

| code | severity | meaning |
| --- | --- | --- |
| `invalid_input` | fatal | unreadable timeline or source |
| `truncated_box` | error | a box extends past the end of the file |
| `corrupt_range` | error | a sample's bytes are damaged or out of range |
| `missing_duration` / `vfr_missing_duration` | warn | a sample has no usable duration |
| `timestamp_wrap` | info | a modulus boundary was unwrapped |
| `timestamp_regression` | warn | normalized time moved backwards |
| `clock_drift` | warn | estimated drift exceeds the review threshold |
| `duplicate_packet` / `out_of_order_packet` | info | handled inside the window |
| `late_packet` | warn | arrived outside the window, unrecoverable |
| `packet_loss` | warn/error | sequence numbers absent from the capture |
| `dependency_cycle` | fatal | a dependency cycle was found |
| `missing_parent` | error | a clip's parent does not exist |
| `audio_under_cover` | error | an audio bed does not cover its video clip |
| `blocked_dependency` | error | an upstream track failed |
| `keyframe_snap` | warn | a cut was moved further than the tolerance |
| `unrecoverable_range` | fatal | no safe keyframe boundary around the damage |

## Layout

```
cmd/archivist/main.go          CLI, flags, exit codes, terminal digest
internal/model/                rational time base, timeline, faults
internal/clock/                33-bit unwrap and drift estimation
internal/rtp/                  capture decoding, reorder window, dedup
internal/mp4/                  tolerant box scan, sample tables, corruption
internal/graph/                dependency graph, cycles, coverage
internal/recover/              boundary search, planning, isolation
internal/report/               metrics aggregation and formatting
internal/pipeline/             stage wiring, bench harness
internal/testkit/              deterministic fixture generation
```
