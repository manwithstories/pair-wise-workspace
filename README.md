# framefix

`framefix` inspects and repairs packet timestamps in H.264/H.265 MP4 files
without invoking `ffmpeg`, `ffprobe`, preview windows, or network services.
It decodes once for frame classification and pixel summaries, keeps only a
bounded reorder window, and remuxes the original compressed packets.

## Install

```bash
python3.11 -m venv .venv
. .venv/bin/activate
pip install -e '.[test]'
```

## Demo

```bash
python -m framefix synth --out fixtures --profiles clean,bframes,broken
python -m framefix scan fixtures/bframes.mp4 --json
python -m framefix repair fixtures/broken.mp4 \
  --out out/broken.fixed.mp4 \
  --report out/broken.report.json \
  --max-jump-ms 250
python -m framefix scan out/broken.fixed.mp4 --json
python -m framefix selfcheck
```

Exit codes: `0` success, `2` input/codec failure, `3` partial result with
remaining damage, `4` post-repair pixel-summary mismatch.

