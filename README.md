# clipgraph

Deterministic offline/streaming renderer for a small dialogue, music, SFX, and
foley mix graph. The same `Engine` evaluates the same absolute frame ranges for
both a full WAV render and variable-size stream pulls.

## Requirements

- Python 3.11+
- NumPy 1.26
- soundfile 0.12

No service, browser, or graphics dependency is used. `python -m clipgraph`
works directly from the repository root through the source-tree launcher.

## Demo

```bash
python -m clipgraph gen-fixtures --out fixtures --seed 7
python -m clipgraph render --graph fixtures/demo.mix.json --out out/mix.wav --raw out/offline.f32 --report out/render.json
python -m clipgraph stream --graph fixtures/demo.mix.json --blocks 1024,777,333,4097 --raw out/stream.f32 --report out/stream.json
python -m clipgraph compare --a out/offline.f32 --b out/stream.f32 --tol 1e-9
python -m clipgraph analyze --input out/mix.wav --window-ms 400 --report out/analysis.json
```

`render` also writes `out/mix.wav.stems.json`. It contains the graph snapshot
and per-stem metrics used by `analyze` when no graph is supplied on the command
line.

## Graph format

The root object has `project`, `nodes`, and `outputs`. Times are seconds and
gain values are dBFS unless a curve explicitly uses the linear domain.

```json
{
  "project": {"sample_rate": 48000, "channels": 2, "duration": 6.0},
  "nodes": [
    {
      "id": "dialogue_src",
      "kind": "source",
      "source": {
        "path": "assets/dialogue.wav",
        "sample_rate": 32000,
        "channels": 1,
        "start_sec": 0.0137
      }
    },
    {
      "id": "dialogue",
      "kind": "track",
      "inputs": ["dialogue_src"],
      "curves": [
        {"id": "ride", "domain": "db", "points": [[0.0, -6.0, "linear"], [2.0, -4.0, "cosine"], [5.5, -5.0, "hold"]]}
      ]
    },
    {
      "id": "music_bus",
      "kind": "bus",
      "inputs": ["music"],
      "curves": [],
      "sidechain": {"source": "dialogue", "attack_sec": 0.01, "release_sec": 0.18, "max_duck_db": 8.0, "threshold_db": -30.0}
    },
    {"id": "master", "kind": "bus", "inputs": ["dialogue_bus", "music_bus"], "curves": []}
  ],
  "outputs": {"master": "master", "tracks": ["dialogue", "music"], "buses": ["music_bus"]}
}
```

Curve points are `[time, value, mode]`; mode is `hold`, `linear`, or `cosine`
for the segment starting at that point. Several curves on one node are added in
dB. Curve time points must be strictly increasing.

Node kinds:

- `source`: one readable audio file and its declared rate/channel count.
- `track`: exactly one input, optional static gain and curves.
- `bus`: one or more inputs, optional curves and sidechain detector.

## Rendering model

- Every source is resampled to the project rate with a windowed-sinc kernel.
  Downsampling lowers the kernel cutoff to avoid aliasing.
- Fractional starts are retained in the source-position phase. Alignment
  reports expose the exact start in project samples and the compensated
  fractional delay.
- `Engine.render(start, end, outputs, state)` has no mutable render fields.
  It returns a new `RenderState` containing the frame cursor, sidechain
  envelope, resample phase, and non-finite sample counters.
- The sidechain envelope is sample-recursive with attack/release in seconds,
  so splitting the range at any block boundary does not change its result.
- Offline rendering still writes in bounded blocks. Sources are read on demand,
  not loaded as complete tracks.
- The streaming driver honors the requested pull sequence at its output
  boundary. Pulls smaller than 256 frames are coalesced into bounded engine
  windows to avoid Python/NumPy per-call overhead; the reported `block_count`
  remains the number of requested pulls.
- NaN and infinity are replaced by zero at the affected node and counted.

## Measurement details

`analysis.py` tracks exact peak, overall RMS, configurable fixed-window RMS,
per-channel clipping sample count, contiguous clipping runs, and a 4x
Catmull-Rom interpolated true peak. WAV files are read in blocks.

`analysis.json` contains:

- `tracks` and `buses`: per-node peak/RMS/clipping/true-peak metrics.
- `buses[].duck_max_db`: most negative sidechain gain in dB.
- `curves`: curve id, sampled frame count, minimum/maximum dB, first dB value.
- `alignment`: per-source rate ratio, start frame, fractional delay, maximum
  residual alignment error.
- `short_sources`: empty or zero-padded source tails.
- `nonfinite`: total and per-node zeroed values.
- `summary`: master peak, RMS, clipping, and true peak.

## Exit codes

- `0`: command completed and comparison passed.
- `2`: argument failure or invalid mix graph.
- `3`: offline/stream comparison exceeded tolerance.
- `4`: audio asset unreadable, missing, or has an invalid/declared-mismatched sample rate.

## Tests

```bash
python -m unittest discover -s tests -v
python -m compileall -q src/clipgraph clipgraph tests
```

Generated `fixtures/`, `out/`, WAV files, raw float files, and `__pycache__`
are ignored and are not part of the source delivery.
