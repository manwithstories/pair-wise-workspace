# mediacore

Deterministic, resumable audio/video timeline processing for mobile clips.
Pure-Python (3.12) on PyAV, NumPy and soundfile; no network, no external
services, no GUI. Everything is driven from the terminal.

The problem it solves: mixing player-view video, team-voice and tactical-cue
tracks means three different timebases, and a naive float-second edit drifts at
clip boundaries. When a file is truncated or a packet is corrupt, an offline
pipeline either fails from the top or silently produces a wrong result. This
project gives every timestamp one integer-microsecond representation, locates
the *first* anomaly by track / packet / PTS, and resumes from an atomic
checkpoint so a resumed run is byte-identical to a full run.

## Setup

```bash
brew install ffmpeg libsndfile
python3.12 -m venv .venv
.venv/bin/python -m pip install av==12.3.0 numpy==2.1.3 soundfile==0.12.1
```

All commands run from the project root with `.venv/bin/python`.

## Acceptance / demo

```bash
.venv/bin/python scripts/demo.py --generate --workdir .demo \
    --sample-rate 48000 --layout stereo --target-lufs -16 --interrupt-after 2
.venv/bin/python -m mediacore.cli resume --workdir .demo --verify
```

The printed report contains the five required values:

| field | meaning | target |
|---|---|---|
| `damage_offset_frames` | first-fault location error, in frames | ≤ 1 |
| `recovery_seconds` | wall time of the resumed run | < 2 s |
| `reused_nodes` | nodes taken from the checkpoint | ≥ 2 |
| `output_sha256` | SHA-256 of the assembled output | 64 hex chars |
| `timeline_drift_us` | worst timestamp rounding residual | ≤ 21 µs |

Exit codes: `0` all checks passed; `2` input was damaged but policy recovered
and the report says so; any other non-zero value means a parameter, dependency
graph or output check failed.

## Architecture

```
src/mediacore/
  timebase.py       unified microsecond timebase, rounding, drift meter
  errors.py         locatable, recoverable error model + exit codes
  timeline.py       explicit clips / transitions, validation, resolution
  probe*.py         PyAV scan, damage classification, first-fault locating
  audio*.py         resample / layout / BS.1770 loudness / deterministic render
  graph.py          dependency plan, cycle detection, failure isolation
  checkpoint.py     atomic checkpoints and input digests
  pipeline*.py      orchestration, assembly, resume
  report.py         JSON summary
  cli.py            terminal entry point (`demo`, `resume`)
  testdata*.py      deterministic fixtures and damage variants
  integrity.py      independent ISO-BMFF box walk + file hashing
```

### Why integer microseconds

`fractions.Fraction` converts a container's rational timebase to integer
microseconds exactly once (nearest microsecond, ties away from zero). Over ten
minutes at 30 fps the worst residual is 1 µs; `DriftMeter` records it and the
report publishes `timeline_drift_us`. Floats would drift measurably and put a
clip boundary on a different sample depending on summation order.

### Why streaming loudness

BS.1770 integrated loudness is measured with 400 ms blocks / 75 % overlap and
the −70 LUFS / −10 LU gates. K-weighting is two biquads evaluated as an FFT
convolution, so a ten-minute pass is near real time. Renders are two-pass:
measure, then apply a single static gain and write block by block. Peak memory
is one second of audio plus the loudness window, independent of clip length
(measured: ~64 MiB RSS for a 10-minute render).

### Why resume is safe

A checkpoint records, per node, the digest of the node's inputs (source
content hash, clip bounds, audio settings) and of its dependencies, plus the
output hash. A node is reused only when all digests match **and** the output
still hashes to the recorded value. Digest equality implies the same
computation, so a resumed run is byte-identical to a full run — asserted in
`tests/test_recovery.py` and re-checked by `resume --verify`, which re-assembles
from the checkpointed segments in a temp dir and compares hashes.

### Damage classes

The probe reports exactly one class per track:

* **truncated** — the file ends early (top-level box past EOF, or demux aborts).
* **index gap** — structurally complete, but a sample is missing from the index;
  located at the first absent packet, within one frame of the real cut.
* **index missing** — the sample-to-chunk table is gone; nothing demuxes.
* **corrupt** — a packet's payload is rejected by the decoder; located by
  decoding from the head so the reported ordinal is exact.

## Tests

```bash
.venv/bin/python -m pytest -q
```

`tests/test_recovery.py` covers the six recovery scenarios and resume
equivalence; `tests/test_audio.py` covers exact boundaries, resampling, layout,
loudness and edge cases (silence, clipping, very short clips);
`tests/test_pipeline.py` and `tests/test_timeline.py` cover graph ordering,
checkpoint atomicity, the report shape and CLI exit codes.

## Scope

No video effect rendering, no encoder development, no network transport. Only
added: one explicit fault per file, so recovery has one repair strategy.
