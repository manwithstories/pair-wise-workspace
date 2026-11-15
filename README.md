# mixgraph

A deterministic, local-only audio mix graph for interactive narrative games.
Offline rendering and callback-style streaming pull blocks from the same
processor, so gain curves, resampling, and metering cannot drift between QA and
runtime.

## Setup

Requires Python 3.11+, NumPy 1.26, soundfile 0.12, PyYAML 6.0, and pytest 8.0.

```bash
python -m pip install -e '.[test]'
```

## Acceptance Demo

```bash
python -m mixgraph.cli gen-fixtures --out fixtures --seed 7
python -m mixgraph.cli inspect --project fixtures/project.json --json out/report.json
python -m mixgraph.cli render --project fixtures/project.json --mode offline --block 256 --out out/a.wav
python -m mixgraph.cli render --project fixtures/project.json --mode stream --block 97 --out out/b.wav
python -m mixgraph.cli compare --a out/a.wav --b out/b.wav --tol 1e-9
```

The compare command prints `max_abs_diff`, `clip_total`, and `parity_ok`. The
generated fixture mixes three sources at 44.1 kHz, 96 kHz, and 22.05 kHz into a
48 kHz stereo project. The three render schedules produce byte-identical WAVs.

## Manifest

Projects may be JSON or YAML. Paths are resolved relative to the manifest.
`gen-fixtures` writes a complete example using the following shape:

```json
{
  "sample_rate": 48000,
  "channels": 2,
  "duration": 2.4,
  "block_size": 2048,
  "clip_threshold_db": 0.0,
  "max_clip_samples": 0,
  "nodes": [
    {"id": "src", "type": "source", "path": "audio/dialogue.wav",
     "sample_rate": 44100, "channels": 2,
     "event": {"start": 0.0, "end": 2.4}},
    {"id": "src_resample", "type": "resample", "rate": 48000},
    {"id": "src_gain", "type": "gain", "gain_db": -3.0,
     "automation": {"interpolation": "linear",
                    "points": [[0.0, -6.0], [0.731, -11.0], [2.4, -8.0]]}},
    {"id": "src_bus", "type": "bus", "gain_db": 0.0},
    {"id": "sum", "type": "sum"},
    {"id": "master", "type": "bus", "gain_db": -1.0,
     "automation": {"points": [[0.0, 0.0], [0.6, -5.0], [1.2, 0.0]]}},
    {"id": "out", "type": "output"}
  ],
  "edges": [
    {"from": "src", "to": "src_resample"},
    {"from": "src_resample", "to": "src_gain"},
    {"from": "src_gain", "to": "src_bus"},
    {"from": "src_bus", "to": "sum"},
    {"from": "sum", "to": "master"},
    {"from": "master", "to": "out"}
  ],
  "tracks": [
    {"id": "dialogue", "source": "src", "gain": "src_gain",
     "bus": "src_bus", "mute": false, "solo": false}
  ],
  "master_bus": "master",
  "output": "out"
}
```

Node types are `source`, `gain`, `bus`, `sum`, `resample`, and `output`.
Automation points are `[time_seconds, gain_db]`; a point may instead use an
object with `time` plus `db`, `gain_db`, or linear `gain`. Points may be listed
in any order. Linear mode ramps every sample. Exponential mode interpolates
positive linear gain geometrically and rejects non-positive `gain` points.

Each source is clipped to its `event` interval and padded with silence when
the file is shorter than that interval. The output length is the project
`duration`. Resampling ratios are reduced integer fractions and are retained
across block pulls with FIR history.

Any track with `solo: true` disables all non-solo tracks; solo takes precedence
over that same track's `mute` flag. Automation may be attached to gain or bus
nodes, including the master bus for ducking.

## Report

`inspect` reports each track's `peak_dbfs`, `rms_dbfs`, `clip_samples`,
`clip_segments`, `first_clip_sample`, reduced `resample_ratio`, and
`automation_samples` at report block boundaries. It also reports each bus,
`master_peak_dbfs`, and `clip_total`. Clipping segments span block boundaries,
and the first clipped position is zero-based.

## Exit Codes

| Code | Meaning |
| ---: | --- |
| 0 | success |
| 1 | unexpected exception |
| 2 | invalid manifest or parameter, including channel mismatch or NaN |
| 3 | graph construction failure, including cycles or dangling ports |
| 4 | parity difference exceeds tolerance |
| 5 | clipping exceeds `max_clip_samples` (or `compare --max-clips`) |

## Tests

```bash
pytest -q
```

The parity suite renders the generated project at block sizes 64, 97, and 1024,
plus the streamed 97-sample pull path, and asserts agreement within `1e-9`.

