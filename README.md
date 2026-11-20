# region-media

Deterministic repair, normalization and chunked delivery of regional live-media
captures. Pure local CLI (macOS, Python 3.12); no network, no devices, no
external services. Packages: PyAV 12, NumPy 2, soundfile 0.12 (pytest 8 for
tests).

## What it does

Edge nodes write a small self-describing capture (`stream.json` +
`capture.edg`): RTP packets with float PCM, RTCP sender reports and video
metadata records. The pipeline:

1. **ingests** the capture — checks RTP framing, unwraps 32/16-bit sequence and
   timestamp wrap, detects duplicates / reordering / burst loss / truncated
   records, and applies the RTCP wall-clock mapping (reporting drift in ppm).
2. **assembles** packets by media position into bounded blocks, filling
   timeline gaps with silence and trimming overlaps.
3. **normalizes** sample rate (44.1/48/96 kHz → target), channel layout (mono,
   stereo, 5.1, 7.1 via an explicit matrix), loudness (BS.1770-4 → target LUFS)
   and peak (4× true peak → ceiling).
4. **chunks** the result into fixed-duration files, commits each atomically,
   and records a checkpoint + SQLite index so an interrupted run resumes only
   the missing chunks (re-hashing and quarantining anything that disagrees).
5. **verifies** by re-decoding the delivered files and recomputing bitrate
   deviation, loudness/peak and manifest alignment.

## Install

```sh
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

## Demo

```sh
.venv/bin/python scripts/make_fixtures.py --out .tmp/fixtures

.venv/bin/python -m region_media.cli process \
  --input .tmp/fixtures/good --out .tmp/out \
  --rate 48000 --layout stereo --lufs -16 --chunk-ms 1000 --target-kbps 256

.venv/bin/python -m region_media.cli verify \
  --out .tmp/out --manifest .tmp/out/manifest.json

.venv/bin/python -m pytest -q
```

Damaged input and interruption/resume:

```sh
.venv/bin/python -m region_media.cli process \
  --input .tmp/fixtures/damaged --out .tmp/out-damaged --target-kbps 256

.venv/bin/python -m region_media.cli process \
  --input .tmp/fixtures/good --out .tmp/out-int --simulate-interrupt-after 2
.venv/bin/python -m region_media.cli resume \
  --input .tmp/fixtures/good --out .tmp/out-int
```

Each command prints one JSON object. Acceptance fields include `target_kbps`,
`actual_kbps`, `bitrate_deviation_pct`, `chunks`, `samples`, `integrated_lufs`,
`true_peak_dbfs`, `clipped_samples`, `lost_packets`, `reordered_packets`,
`metadata_conflicts` and `manifest_alignment_ms`.

## Exit codes

| code | meaning |
|------|---------|
| 0 | success, acceptance metrics met |
| 1 | input or processing error |
| 2 | invalid arguments |
| 3 | recoverable interruption, or acceptance metrics not met |

## Layout

```
src/region_media/
  cli.py         ingest / process / resume / verify (JSON output)
  errors.py      shared error codes, severities, diagnostics
  clock.py       RTP unwrap, RTCP mapping and drift
  packet.py      capture framing, RTP headers, sequence tracking
  assemble.py    streaming scan, position reorder, block assembly
  normalize.py   rate conversion, channel matrix, BS.1770 loudness, true peak
  metadata.py    colour-metadata validation and video provenance
  codec.py       Opus/Ogg and AAC/MP4 chunk encode/decode
  checkpoint.py  atomic commits, SQLite index, quarantine
  manifest.py    per-chunk manifest and metrics
  pipeline.py    orchestration + verify
scripts/make_fixtures.py   deterministic good/damaged/boundary fixtures
tests/test_pipeline.py     acceptance and regression tests
```

## Notes

- Opus in Ogg is used at 48 kHz because its decoder returns exactly the encoded
  sample count; AAC/MP4 covers 44.1/96 kHz targets and records its frame padding
  so decoded length is trimmed to media time.
- The loudness meter is streaming (continuous K-filter state), so results are
  identical whether audio is pushed in one block or many.
- Memory grows with the block/chunk size, never the stream length; the manifest
  only ever lists chunks that were atomically committed and hash-checked.
