# avpipe

Local A/V pipeline diagnostics for a tactical team that has to keep low-latency
voice and video in sync while phones hop between cellular and Wi-Fi.

avpipe generates its own multichannel PCM/WAVE input, brings it to a negotiated
format, pushes it through a bounded queue with an explicit slow-consumer policy,
packetizes and receives it over a lossy, jittery RTP simulation, aligns the
audio and video timelines on one 90 kHz clock, and prints a JSON report with the
quantified result. Everything runs offline: no network, no browser, no camera or
microphone permission, no checked-in binary fixtures.

## Requirements

- macOS with Xcode Command Line Tools
- Go 1.23 (`brew install go@1.23`)

## Build, test, run

```sh
export PATH="$(brew --prefix go@1.23)/bin:$PATH"
go mod download
go test ./...

go run ./cmd/avpipe selftest --seed 42 --loss 0.08 --jitter-ms 35 \
  --consumer-delay-ms 18 --queue-bytes 262144

go run ./cmd/avpipe run --input generated --sample-rate 48000 \
  --layout stereo --target-lufs -16 --report report.json
```

Exit status:

| code | meaning |
|------|---------|
| 0 | processing completed and every metric passed |
| 2 | parameter or input error |
| 3 | a metric missed its threshold |
| 4 | internal processing failure |

## What the report contains

`run` and `selftest` print a terminal summary; `--report path.json` also writes
the same data as JSON. The fields the acceptance checks read are:

| field | meaning |
|-------|---------|
| `sync_error_ms` | p95 audio/video sync error after drift correction |
| `latency_p95_ms` | p95 block residence time: queue wait plus the consumer's own playout latency |
| `packet_recovery_rate` | share of RTP loss repairable inside the recovery window |
| `queue_peak_bytes` | peak charged bytes in the bounded queue |
| `rms_before`, `rms_after` | RMS before and after loudness control |
| `root_error` | first root cause (code, op, track, byte offset) or `null` |

A report never contains raw audio samples, packet payloads or file contents;
tests assert this.

### Acceptance profiles

The same thresholds cannot apply to a clean run and to a deliberately hostile
one, so `run`/`selftest` select a profile from the injected conditions and print
it in the header:

| profile | selected when | sync p95 | latency p95 |
|---------|---------------|----------|-------------|
| `normal` | default | < 20 ms | < 120 ms |
| `drift-jitter-boundary` | jitter > 20 ms or drift > 2000 ppm | < 80 ms | < 120 ms |
| `slow-consumer` | consumer delay >= 100 ms | < 80 ms | < 250 ms |

`packet_recovery_rate` must stay >= 0.90 and `queue_peak_bytes` must stay within
`--queue-bytes` (262144 by default) in every profile.

## Architecture

```
cmd/avpipe            terminal entry point: flags, exit codes, summary
internal/pipeline     orchestration, acceptance profiles, pacing model
internal/fixture      generated WAV blocks, video timestamps, MP4 clip
internal/audio        WAV parse/decode, layout map, resample, loudness
internal/stream       bounded queue (bytes + duration, block/drop/drop-oldest)
internal/rtp          pion/rtp packetize, reorder, dedup, recovery window
internal/sync         audio/video timeline alignment on the 90 kHz clock
internal/track        dependency graph, topological order, fault propagation
internal/clock        the single 90 kHz media timescale and conversions
internal/report       JSON report and thresholds
internal/fault        unified error model and exit-status mapping
```

The data path is: generated WAV -> parse (bit depth, rate, block align, chunk
bounds) -> decode to normalized blocks -> layout map (explicit silence fills) ->
deterministic resample -> bounded queue -> loudness -> RTP simulation ->
timeline alignment. Every stage shares the 90 kHz clock and the error model, so
the first root cause reaches the report even when a failure cancels dependent
tracks.

### Determinism

All randomness comes from `internal/rng` (splitmix64) seeded by `--seed`. Two
runs with the same arguments produce byte-identical reports; tests assert this.

## Fault scenarios

`selftest` additionally runs six isolated fault paths and prints one line per
scenario:

- `corrupt_wav` — a PCM payload truncated inside the data chunk is rejected with
  the byte offset of the overrunning chunk.
- `recovery_window` — a loss burst wider than the recovery window is reported as
  unrecoverable and drops `packet_recovery_rate`.
- `queue_close` — a producer blocked on a full queue is woken by `Close` and
  receives a closed-queue error instead of hanging.
- `graph_cycle` — a dependency cycle is detected before any track runs.
- `drift_jitter` — the drift-and-jitter boundary sample stays inside the 80 ms
  budget after correction, with the raw error visibly larger.
- `slow_consumer` — a slow consumer against a jittery stream and a shallow
  queue stays inside the byte limit by dropping excess blocks instead of
  growing an unbounded backlog.

## Scratch data

The diagnostic MP4 clip and any intermediate data are written only to the
directory given by `--work-dir`; when it is omitted a private temporary
directory is created and removed before the command returns.

## Dependency note

`go.mod` pins `github.com/pion/rtp v1.8.11` as specified. It pins
`github.com/abema/go-mp4 v1.7.4`, which is the latest tag published upstream —
there is no v1.29.0 release of that module; v1.7.4 is the version that resolves
from the proxy and is used for both writing and probing the MP4 clip.
