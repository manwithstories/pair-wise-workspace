# Mixer Delivery Auditor

Deterministic offline/streaming mix rendering and delivery audits for Python
3.11+. Audio is read and written only through `soundfile`; fixtures are
generated locally and no external media is required.

## Install

```bash
python3 -m pip install -r requirements.txt
```

## Demo

```bash
python -m mixer.cli gen-fixtures --out-dir fixtures --duration 6
python -m mixer.cli render --graph fixtures/session.json --out out/mix.wav \
  --master-sr 48000 --block-size 1024 --report out/report.json
python -m mixer.cli stream --graph fixtures/session.json --out out/stream.wav \
  --block-size 257 --report out/stream.json
python -m mixer.cli compare --a out/mix.wav --b out/stream.wav
```

`stream` also accepts `--seek SECONDS`; automation is still evaluated on the
absolute timeline. Both render paths use the same node `process(nframes,
start_frame)` interface and never load a complete source or mix into memory.

## Graph format

```json
{
  "master_sr": 48000,
  "block_size": 1024,
  "output": "master",
  "nodes": [
    {"id": "dialogue", "type": "source", "path": "fixtures/dialogue.wav"},
    {"id": "dialogue_gain", "type": "gain", "input": "dialogue",
     "automation": "fixtures/dialogue_gain.csv"},
    {"id": "dialogue_pan", "type": "pan", "input": "dialogue_gain", "pan": 0.1},
    {"id": "mix", "type": "sum", "inputs": ["dialogue_pan"]},
    {"id": "master", "type": "limiter", "input": "mix", "ceiling_db": -1.0}
  ]
}
```

Node aliases may use `id`, `alias`, or `name`. Supported types are `source`,
`gain`, `pan`, `sum`, and `limiter`; there must be exactly one sink, and the
optional `output` field must name it. Paths are resolved against the current
workspace and escaping it is rejected.

Automation CSV files use `time_sec,gain_db`, require strictly increasing
non-negative times, and hold the first/last gain outside the curve. A missing
or empty curve is 0 dB. Gain is interpolated linearly in dB at absolute time.

Sources at any rate are converted with a reduced rational `master_sr/source_sr`
ratio (for example `160/147` from 44.1 kHz to 48 kHz). Output length is
round-half-up and the filter tail is zero padded. Mono is up-mixed only when a
stereo branch is summed; sources with more than two channels use the first two
and emit one warning.

## Reports and exit codes

Reports use `schema: "mix-report/1"` and include input SHA-256 hashes,
`master_sr`, `block_size`, per-track peak/RMS/clipping statistics, bus
statistics, the first five clipping times, and automation arrays sampled at
`--automation-step` (601 points for a 6-second render at 0.01 seconds).

`compare` prints JSON with `identical`, `max_abs_diff`, and
`first_divergence` (a zero-based frame index or `null`). The process exits `5`
when the files differ.

| Code | Meaning |
|---:|---|
| 0 | success |
| 2 | command-line parameter error |
| 3 | missing/invalid input, channel mismatch, or invalid sample rate |
| 4 | graph validation failure |
| 5 | offline and stream outputs differ |
| 6 | output/report write failure |
| 7 | clipping found with `--fail-on-clip` |

Tests:

```bash
python -m unittest -v tests.test_core
```

