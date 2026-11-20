# mediacore

Local-only, deterministic toolkit for recovering presentation timelines from raw
H.264/H.265/AV1 bitstreams, validating keyframe indexes, analysing PCM audio,
and checkpointing/resuming bounded-memory A/V pipelines.

All fixtures are generated locally. No network access, device permissions or GUI
dependencies are used.

## Quick start (macOS)

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
.venv/bin/python -m mediacore.cli demo --codec h264 --duration 12 \
    --bitrate 900000 --seed 41 --out .tmp/demo
.venv/bin/python -m mediacore.cli verify \
    --manifest .tmp/demo/manifest.json --index .tmp/demo/index.json
.venv/bin/python -m pytest -q
```

The demo prints `target_bps`/`actual_bps`/`deviation_pct`,
`video_psnr_db`/`audio_rms` and
`manifest_alignment_max_ms`/`random_access_max_ms`.

Exit codes: `0` verified, `2` bad arguments/input or checkpoint conflict,
`3` recoverable interruption (resume by re-running the same command),
`4` verification metrics outside limits.

`--corrupt-tail` writes a deliberately truncated stream which is reported with
byte offsets. `--interrupt-after N` commits N stages, then exits `3`; re-running
the same demo resumes from validated blocks and produces the same manifest hash.
