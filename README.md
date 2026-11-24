# mediasim

Local, deterministic simulator for tactical replay clips: it demuxes a
synthetic multi-track MP4, recovers what it can from damaged containers,
runs the frames through a lossy RTP channel, and renders an off-screen PNG
sequence with per-frame hashes and a reviewable JSON report.

Everything runs in-process with Go 1.25.6. There is no service, no window, no
browser and no capture device.

## Quick start

```sh
brew install go@1.25.6
go mod tidy
go build ./...
go test ./...

# Clean run: 120 frames, in-sync, in-budget.
go run ./cmd/mediasim demo --seed 17 --loss-burst 4 --jitter-ms 80 --out ./artifacts

# Damaged run: recovers a valid prefix and exits 2.
go run ./cmd/mediasim demo --fixture truncated --out ./artifacts-bad
```

`go run` reports any non-zero program exit as 1; build the binary to see the
real status (`0` complete, `1` error, `2` partial recovery with a report):

```sh
go build -o mediasim ./cmd/mediasim
./mediasim demo --fixture truncated --out ./artifacts-bad; echo $?   # 2
```

## Commands

| Command | Purpose |
| --- | --- |
| `mediasim demo [flags]` | Full pipeline on a built-in fixture |
| `mediasim verify --fixture NAME --out DIR` | Same, with fault-injection defaults |
| `mediasim fixtures` | List the four deterministic fixture classes |

Flags: `--fixture` (`normal`, `truncated`, `corrupt`, `missing-index`),
`--seed`, `--loss-burst`, `--jitter-ms`, `--jitter-max-ms`, `--window`,
`--out`.

## Output

The terminal prints the contract fields:

```
frames=120
av_sync_p95_ms<=20
packet_recovery_rate>=0.90
latency_p95_ms<=150
frame_sha256_count=120
recovered_prefix_frames>0
corrupt_ranges=0
measured: av_sync_p95_ms=0.02 packet_recovery_rate=1.0000 latency_p95_ms=116.68 recovered_prefix_frames=120 recovered_indexed_frames=120 corrupt_ranges=0
exit_code=0
```

Each threshold line is the checklist assertion in its satisfied form; the
`measured:` line carries the actual figures. On a damaged fixture the
corruption line flips to `corrupt_ranges>=1` and the exit code becomes 2.

`<DIR>/report.json` holds the same figures plus per-track recovery gaps, RTP
counters, audio edit diagnostics and every classified error with its
category, span and recovery action. `<DIR>/frames/frame_NNNN.png` are the
rendered frames; hashes cover pixel bytes, not the PNG container, so they are
stable across machines.

## Architecture

```
cmd/mediasim            terminal entry point only
internal/clock          rational timebase, rounding, classified errors
internal/graph          explicit timeline -> validated DAG -> stable plan
internal/mp4            tolerant demux, index gaps, media-boundary scan
internal/rtp            pion packets, reordering, bounded jitter buffer
internal/render         off-screen compositor, per-frame SHA-256
internal/fixture        deterministic synthetic MP4s and fault presets
internal/pipeline       orchestration and reporting
```

The data flow is one direction: an explicit timeline becomes a track graph;
the plan drives MP4 parsing and recovery; one rational timebase and one error
model carry through RTP buffering and compositing; the pipeline summarises the
result.

## Fixtures

All fixtures are generated locally by `internal/fixture`:

- **normal** - two video tracks (30000/1001, 10-frame crossfade) and one 48 kHz
  audio track.
- **truncated** - the file ends mid-sample; a valid prefix survives.
- **corrupt** - three video samples and two audio samples lose their header
  magic inside otherwise valid runs.
- **missing-index** - stsz/stco/stsc are absent; every track is recovered by
  scanning the mdat for self-describing sample headers.

## Guarantees

- Identical input and flags produce identical frame hashes, PNG bytes and
  report JSON.
- A committed frame is never rewritten: rendering the same index twice is an
  error, so a recovery pass cannot change earlier output.
- Buffers are bounded (reorder window, jitter packet cap, frame cap, track and
  segment caps) and every time conversion is overflow checked.
- Recoverable damage produces partial output with exit 2 rather than silence;
  unrecoverable damage exits 1 with a classified error on stderr.
