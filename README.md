# isobmff-lab

Offline rebuild and validation of fragmented MP4 (fMP4) segment indexes.
Pure Python 3.11+ standard library at runtime; pytest for tests. No
playback, demuxing, or GUI dependencies.

## Setup

```sh
pip install -e .          # or: PYTHONPATH=src python -m isobmff.cli ...
```

## Usage

```sh
python -m isobmff.cli gen --out fixtures/case_ok.mp4 --samples 240 --fragments 6 --seed 7
python -m isobmff.cli gen --out fixtures/case_bad.mp4 --samples 240 --fragments 6 --corrupt duration --seed 7
python -m isobmff.cli verify fixtures/case_ok.mp4 --json out/ok.json
python -m isobmff.cli verify fixtures/case_bad.mp4 --json out/bad.json
```

`verify` accepts files, directories, or glob patterns. With multiple
inputs, `--json` is treated as a directory and one JSON file per input
is written, plus a summary table on the terminal.

## Exit codes

- `0` all checks passed
- `2` argument/usage error
- `3` structural damage (truncated boxes, stsc out of range, ...)
- `4` validation mismatch (rebuilt index differs from sidx)

In batch mode the worst exit code wins.

## Corruption kinds (`gen --corrupt`)

- `offset`: sidx `first_offset` lands inside the first mdat payload
- `duration`: last `subsegment_duration` one frame short of the stbl timeline
- `stsc`: trailing stsc `first_chunk` beyond the chunk count
- `truncate`: file cut mid-box

Fixtures are deterministic: the same `--seed` produces byte-identical
files. Sample payloads are synthesized; no external media is referenced.

## Layout

- `src/isobmff/reader.py` byte cursor, box header parsing (size/largesize/0/uuid)
- `src/isobmff/boxes.py` box tree and path index
- `src/isobmff/stbl.py` stts/ctts/stsc/stsz/stco/co64 expansion
- `src/isobmff/sidx.py` sidx v0/v1, hierarchical reference recursion
- `src/isobmff/rebuild.py` fragment boundaries from tfhd/tfdt/trun + stbl
- `src/isobmff/validate.py` offset cross-check and index comparison
- `src/isobmff/report.py` terminal tables and JSON summaries
- `src/isobmff/fixtures.py` synthetic fixtures and corruption injection
- `src/isobmff/cli.py` argparse subcommands `gen` / `verify`
