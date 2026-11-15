# audiosplit

Local, streaming dialogue segmentation for 48 kHz WAV masters. The project has
no service, browser, GUI, or audio-device dependency. Source WAV files are only
read; segment WAVs, JSON manifests, and SQLite indexes are written locally.

## Install

```bash
python3.11 -m venv .venv
. .venv/bin/activate
pip install -e .
```

## Workflow

```bash
python -m audiosplit fixtures --out data/fixtures --seed 7 --files 6

python -m audiosplit scan data/fixtures/take_a.wav \
  --frame-ms 20 --hop-ms 10 --enter-db 6 --exit-db 3 \
  --min-seg-ms 120 --min-sil-ms 250 --pad-ms 40 \
  --db out/index.sqlite --json

python -m audiosplit export --dir data/fixtures --db out/index.sqlite \
  --out out/segments --manifest out/manifest.json

python -m audiosplit report --db out/index.sqlite --json

python -m audiosplit selftest --fixtures data/fixtures --tol-samples 48
```

All commands write one JSON object to stdout. `scan` returns sample-level
boundaries, per-segment peaks, noise floor, entry threshold, silence totals, and
silence ratio. `export` recursively reads WAV files, reuses the same detector,
atomically writes 16-bit mono segment WAVs, and writes a manifest atomically.

## Architecture

- `wavio.py`: strict RIFF/PCM parsing, 8/16/24/32-bit decoding, multichannel
  downmix, block iteration, sample-range reads, atomic WAV writes.
- `frames.py`: streaming RMS, peak, zero-crossing rate, and dBFS metrics.
- `noise.py`: low-quantile/mode floor estimate and adaptive hysteresis levels.
- `detector.py`: the only detector implementation, frame state machine, sampled
  boundary refinement, and zero-crossing snapping.
- `segmenter.py`: minimum segment/silence regularization, padding, clipping,
  and silence/peak accounting.
- `store.py`: versioned SQLite schema, content/per-parameter idempotent upserts,
  batches, export registration, and aggregate queries.
- `exporter.py`, `fixtures.py`, `selftest.py`, `cli.py`: batch orchestration,
  deterministic truth fixtures, accuracy checks, and command handling.

The scanner keeps only frame statistics and bounded read windows in memory; even
segment peak calculation and export are chunked. A 30-minute 32-bit 48 kHz
all-active benchmark scans in about 1.5 seconds at roughly 75 MB peak RSS on the
development machine.

## Exit Codes

| Code | Meaning |
| ---: | --- |
| 0 | Success |
| 2 | Invalid arguments or detector parameters |
| 3 | Audio, WAV, filesystem, or unexpected runtime error |
| 4 | Selftest count/boundary tolerance failure |
| 5 | SQLite open, query, or migration failure |

Errors are returned as JSON with a stable `error.code`, `error.message`, optional
`error.path`, and `exit_code`; tracebacks are not printed.

## Tests

```bash
python -m unittest discover -s tests -v
```

The suite covers all supported PCM depths, downmix, malformed/truncated WAVs,
state-machine edge cases, noise adaptation, idempotent scans, rerunnable exports,
manifest/file consistency, reports, selftest accuracy, and CLI exit codes.

