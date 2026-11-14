# mkvmp4

Streaming Matroska (`.mkv`) to faststart MP4 remuxer for recorded live events.
The parser, timeline rebuild, ISO BMFF writer and verifier are implemented in
Python without ffmpeg or another media library.

## Install

```bash
python3.11 -m pip install -e .
```

## Demo

```bash
python3.11 scripts/make_fixtures.py --out samples --cases 4
python3.11 -m mkvmp4.cli probe samples/case_b_restart.mkv --max-depth 4 --json out/probe.json
python3.11 -m mkvmp4.cli remux samples/case_b_restart.mkv -o out/case_b.mp4 \
  --manifest out/remux.json --repair clamp
python3.11 -m mkvmp4.cli verify out/case_b.mp4 --against out/remux.json
pytest
```

The generated `case_b_restart.mkv` exercises unknown-size Segment and Cluster
elements, restart timestamp discontinuities, all three Matroska lace modes,
chapters and untrusted Cues. Its expected probe summary is 613 elements, seven
unknown-size elements and five clusters. Remux writes four tracks, rebuilds five
chunk groups, and records two duplicate, one backward and three gap repairs.

## Commands and exits

- `probe FILE [--max-depth N] [--json PATH]` prints the element count, unknown
  size count, cluster count and a stable SHA-256 parse digest.
- `remux FILE -o OUT [--manifest PATH] [--repair clamp|resample|fail]
  [--max-repair-ratio R]` writes `ftyp`, `moov`, then `mdat`.
- `verify FILE --against MANIFEST` reads sample tables back and checks layout,
  sample count, first/last PTS, keyframe count and chapter count.

Exit codes: `0` success, `2` corrupt input or invalid EBML, `3` unsupported
codec, `4` repair limit, `5` verification mismatch.

## Layout

`src/mkvmp4/ebml/vint.py` handles EBML variable integers and unknown-size
sentinels. `ebml/reader.py` is the buffered streaming scanner. `mkv/` contains
the semantic model, parser and timing logic. `mp4/` contains box and sample-table
writers. `probe.py`, `remux.py` and `verify.py` expose the three workflows, and
`scripts/make_fixtures.py` is the deterministic fixture generator.

