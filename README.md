# bcastqc

Deterministic quality control and repackaging for broadcast raw bitstreams.

`bcastqc` ingests local H.264 / H.265 / AV1 **raw** video plus independent
PCM/WAV audio, validates keyframe indexes, composes an explicit timeline
offscreen, and exports a reviewable frame sequence with per-frame provenance
and a JSON manifest. Statistics cover audio/video sync, packet-loss recovery
and block latency.

Everything runs locally: no network access, no external services, no execution
of input-supplied code. Only local fixtures or user-provided files are read;
paths outside the timeline directory are rejected.

## Requirements

* macOS (arm64 or x86_64), Python 3.12
* Homebrew FFmpeg and libsndfile (`brew install ffmpeg libsndfile`)
* All Python dependencies live in the project venv

## Setup

```sh
brew install ffmpeg libsndfile
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

## Acceptance demo

```sh
.venv/bin/python scripts/demo.py --seed 41 --drop-rate 0.02 \
    --queue-frames 8 --consumer-delay-ms 15 --out /tmp/bcastqc-demo
.venv/bin/python -m bcastqc.cli verify --manifest /tmp/bcastqc-demo/run/manifest.json
```

The demo generates deterministic fixtures (clean H.264/H.265/AV1 programs,
PCM at 48 kHz and 44.1 kHz, plus dropped, truncated, corrupt and bad-index
variants), runs the pipeline, and prints:

| field | target |
|---|---|
| `sync_error_ms_p95` | ≤ 20 ms |
| `packet_recovery_rate` | ≥ 0.90 |
| `latency_ms_p95` | ≤ 500 ms |
| `peak_queue_frames` | ≤ 8 |
| `frame_hash_mismatches` | 0 |

`exit_code` is `0` when the run passes. The demo also reports the failure
cases it exercised:

| case | expected result |
|---|---|
| truncated stream | exit 3, tail concealed with `max_gap_ms` |
| fully unrecoverable loss | exit 3, `packet_recovery_rate` 0 |
| injected +40 ms audio offset | measured as −1 frame |
| corrupt access unit | exit 3, ≥1 illegal unit, 0 recovered packets |
| dependency cycle | exit 2 with the cycle path |

The run also includes `alignment`: duplicated/concealed frames, per-frame
placement against a clean reference pass, and the independently measured
video and audio displacement.

## Exit codes

| code | meaning |
|---|---|
| 0 | pass |
| 2 | invalid input or timeline |
| 3 | unrecoverable media error |
| 4 | resource or output error |

## Commands

```
bcastqc scan   --stream FILE [--codec h264|h265|av1] [--index INDEX] [--policy scan|reject]
bcastqc index  --stream FILE --index INDEX [--policy scan|reject] [--write-recovered OUT]
bcastqc run    --timeline TL.json --out DIR [--drop-rate R] [--queue-blocks N] [--resume]
bcastqc verify --manifest DIR/manifest.json
bcastqc demo   --seed S --out DIR
```

All commands print one JSON document to stdout; diagnostics go to stderr.

## Timeline format

```json
{
  "time_base": "1/90000",
  "frame_rate": "25/1",
  "width": 160,
  "height": 90,
  "output_sample_rate": 48000,
  "tracks": [
    {"id": "v1", "kind": "video", "path": "program_h264.bin", "codec": "h264"},
    {"id": "a1", "kind": "audio", "path": "program_audio.wav",
     "sample_rate": 48000, "channels": 2, "offset_ticks": 0}
  ],
  "clips": [
    {"id": "c1", "track": "v1", "start_ticks": 0, "source_in_ticks": 0,
     "duration_ticks": 270000},
    {"id": "c2", "track": "a1", "start_ticks": 0, "source_in_ticks": 0,
     "duration_ticks": 270000, "depends_on": ["c1"]}
  ]
}
```

* All positions are **integer ticks** in the master time base; no floating
  point accumulates.
* `depends_on` forms a DAG. Cycles are rejected with the full path
  (`a -> b -> a`).
* Clips on one track may not overlap; the overlapping interval is reported.
* Tracks declared but not referenced by any clip are **excluded** and listed
  in `excluded_tracks`, never silently composed.

## Architecture

| module | responsibility |
|---|---|
| `clock.py` | exact rational `TimeBase` / `MediaTime` / `MediaWindow`, explicit rounding |
| `errors.py` | serialisable error taxonomy with recovery policy and exit codes |
| `bitstream.py` | incremental Annex B / AV1 OBU access-unit parsing, keyframe-index validation, access-unit conformance checks |
| `timeline.py` | timeline and track/clip graph loading, dependency ordering, coverage and usage validation |
| `decode.py` | display-ordered video decode with bounded reorder, deterministic loss model, retry/conceal, PCM reader and exact resampling |
| `stream.py` | bounded queue, back-pressure, slow-consumer timeout, cancellation |
| `render.py` | offscreen composition with per-frame source spans, A/V marker tracking |
| `report.py` | sync, recovery rate, latency quantiles, alignment measurement, exit codes |
| `pipeline.py` | render → bounded queue → PNG export → hashing → manifest, with resume |
| `fixtures.py` | deterministic fixture generation (clean and damaged variants) |
| `cli.py`, `demo.py` | command line and acceptance scenario |

### Behavioural guarantees

* **Bounded memory.** Decoded frames move in fixed-size blocks through a queue
  with a hard capacity; peak memory is independent of programme length (a
  900-frame programme peaks around 70 MB RSS).
* **Deterministic output.** The same inputs, timeline and parameters produce
  byte-identical frame hashes and manifest hashes across runs.
* **Resumable.** A run interrupted between complete blocks resumes from the
  first missing block. The manifest records the input digest, timeline digest
  and parameter digest; any mismatch is rejected (exit 2), and the resumed run
  is hash-identical to a clean run.
* **Honest recovery accounting.** A retried access unit is decoded from intact
  bytes and marked `recovered: true` with `recovery_source: retry`; a
  concealed frame is marked `recovered: false` with the gap duration and is
  never counted as a recovery. Concealed frames are pixel duplicates of the
  last good frame and are reported by the alignment pass.
* **Reviewable provenance.** Every exported frame records its timeline time,
  contributing track and clip, source byte range, recovery/concealment flags
  and pixel hash.

## Tests

```sh
.venv/bin/python -m compileall -q src tests
.venv/bin/python -m pytest -q
```
