# cinecheck

Terminal-only H.264 frame inspection and PTS repair toolkit for drone
grid-inspection aerial mapping. It turns weakly-connected, reconnect-prone
H.264 captures into a strictly equal-interval frame sequence and reports
exactly what was decoded, reordered and repaired — no GUI, no browser, no
camera/microphone, no external media or services.

## Data flow

```
demux -> decode -> reorder -> timeline -> stats -> export
```

`analyze` and `repair` run the identical pipeline
(`cinecheck/pipeline.py`), so reordering and repair results can never
diverge between the terminal summary and the CSV/JSON outputs.

## Modules

- `demux.py` — stream time base and decode-order packet pts/dts; video only.
- `decode.py` — per-packet decode, `pict_type` capture, bad-packet tolerance.
- `reorder.py` — bounded (<=32 frame) stable pts sort, dts fallback, violations.
- `timeline.py` — six anomaly classes, equal-interval rebuild, idempotency.
- `stats.py` — frame/I/P/B/GOP counts, nominal vs measured fps, time span.
- `export.py` — shared schema-versioned payload; terminal, CSV and JSON.
- `fixtures.py` — NumPy-synthetic, libx264-encoded deterministic clips.
- `cli.py` / `pipeline.py` — argparse subcommands, shared pipeline, exit codes.

## Install

```bash
python -m pip install -e .
```

## Commands

```bash
# Generate the four deterministic sample clips (320x180, 24 fps, 1/12288)
python -m cinecheck fixtures --out .fixtures --seed 7

# Terminal summary
python -m cinecheck analyze .fixtures/bpyramid_gap.mp4

# Equal-interval repair with per-frame CSV and schema-versioned JSON
python -m cinecheck repair .fixtures/bpyramid_gap.mp4 \
    --csv .fixtures/pts.csv --json .fixtures/repair.json
```

Options: `--json`, `--csv`, `--fps-override`, `--tolerance-ms`,
`--reorder-depth` (clamped to 1..32), `--seed`.

## Exit codes

| Code | Meaning                                              |
|------|------------------------------------------------------|
| 0    | no anomalies detected                                |
| 2    | anomalies detected and repaired                      |
| 3    | unrecoverable (truncated tail / no decodable frames) |
| 4    | invalid arguments                                    |

## Anomaly classes

`duplicate_pts`, `negative_delta`, `gap` (>1.5x frame length), `jitter`
(beyond tolerance), `non_zero_start`, `tail_residual`. Each event keeps its
original observed pts. Repair rebuilds an `i * step` axis and re-runs the
repair once; `idempotent_pass` reports the second-pass repair count, which
must be `0`.

## Fixtures

`clean_bframes`, `bpyramid_gap`, `vfr_jitter`, `truncated_tail` — 360
frames each (the truncated clip decodes fewer). With `--seed 7`,
`bpyramid_gap` yields 30 I / 90 P / 240 B, 13 repairs, and a maximum jump
of 1024 ticks (83.333 ms at 1/12288).

## Tests

```bash
pytest -q
```

Inputs are opened read-only and never rewritten. The reorder window is
bounded to 32 frames, so memory does not grow with film length. Time base
and frame rate are always read from the stream (with an explicit override
available), never hard-coded; audio streams are ignored.
