# mediamix

Self-describing frame-sequence **alignment, compositing and resumable
validation** pipeline for remote production teams. It ingests studio,
location and editorial-proxy frame sequences, reconciles drifting clocks and
wrapping counters, merges variable-frame-rate tracks onto one integer time
base, renders an offline composite, and independently re-verifies the export.

Everything runs locally on Python 3.12 with PyAV, NumPy and soundfile. There
is no GUI and no network dependency.

## Data flow

```
probe -> clock -> timeline -> render -> validate
                  └──────── resume checkpoints at every stage boundary ───────┘
```

* `model.py` — shared integer time base (nanosecond ticks), records, resource
  limits and errors that always carry the raw source byte offset.
* `probe.py` — streaming parser/writer for the binary frame format; verifies
  payload lengths and SHA-256, anchors every error to file + frame + offset.
* `clock.py` — counter unwrap (rejects indistinguishable multi-wraps) and an
  exact rational drift fit with retained residuals.
* `timeline.py` — maps VFR tracks to master ticks, resolves overlaps by track
  priority and emits traceable diagnostics.
* `render.py` — NumPy offscreen compositing, PyAV PNG encoding, soundfile
  audio sidecar and the JSON manifest.
* `validate.py` — independent re-read: missing frames, duplicate time points,
  dimension and hash mismatches, time continuity.
* `resume.py` — SQLite checkpoints, input fingerprints, atomic writes; stale
  inputs or changed parameters are rejected.
* `pipeline.py` — stage orchestration and interruptible/resumable execution.
* `fixtures.py` — code-generated normal, wrapped, drifting, truncated and
  corrupt-digest samples.

## Binary frame format

All integers are big-endian.

```
MAGIC "MMX1" (4) | file_header (31) | frame*
file_header = track_id(16) clock_rate(Q) clock_bits(B) seq_bits(B) reserved(B)
frame       = frame_header(60) | payload(payload_len)
frame_header = seq(I) clock(I) fps_num(H) fps_den(H) width(H) height(H)
               primaries(4s) transfer(4s) payload_len(I) sha256(32s)
```

## Setup and acceptance

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install 'av>=12,<13' 'numpy>=2,<3' 'soundfile>=0.12,<0.13'
.venv/bin/python -m pip install -e .
.venv/bin/python -m mediamix.cli demo   --workdir /tmp/mediamix-demo
.venv/bin/python -m mediamix.cli verify --workdir /tmp/mediamix-demo --json
```

## Exit codes

| Code | Meaning |
| ---- | ------- |
| 0 | processing and validation passed |
| 2 | corrupt input or bad parameters |
| 3 | resume/checkpoint state conflict |
| 4 | export validation failure |

## Tests

```bash
.venv/bin/python -m unittest tests.test_pipeline -v
```
