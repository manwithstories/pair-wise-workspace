# archive-media

Cold-storage archive recovery for historical broadcast programmes. Pure local
CLI (macOS, Python 3.12); no network, no devices, no external services.
Packages: PyAV 12, NumPy 2, soundfile 0.12 (pytest 8 for tests).

## What it does

A damaged HLS media playlist or DASH MPD (SegmentTemplate / SegmentTimeline)
references audio segments that may be truncated, corrupt, duplicated,
out of order or missing. The pipeline:

1. **parses the manifest** into an ordered segment plan, keeping representation
   id, sequence, exact rational timing and the raw reference. References are
   normalized lexically and anything escaping the archive root or using a
   network scheme is refused. Missing duration / index entries are synthesized
   from the declared timescale and reported, never silently invented.
2. **lays every segment on one rational timeline**, detecting reordering,
   timestamp-rounding collisions and non-contiguous joins without compressing
   or reordering the plan.
3. **demuxes each segment by streaming PyAV packets** with a bounded retry
   count; flush packets are ignored, corrupt packets are located, and whatever
   audio is recoverable is kept.
4. **normalizes audio** — explicit channel matrix (mono / stereo / 5.1 / 7.1),
   stateful resampling to the target rate, and streaming BS.1770-4 loudness
   control toward a target LUFS with a true-peak ceiling. Filter state carries
   across chunks; the final block reports its actual sample count and boundary
   error instead of padding silently.
5. **runs two passes over fixed-capacity queues** (read → demux → process →
   write). Slow consumers block upstream producers; peak occupancy and
   backpressure duration are reported, and memory stays bounded by the queue
   capacity rather than programme duration.
6. **applies the recovery policy** on damage: `skip` (zero-length placeholder),
   `gap` (silent time hole preserving the planned duration) or `stop`
   (`--strict`, fatal with a full locator).
7. **builds a keyframe / decodable-point index** via neighbour scanning when
   the source index is incomplete, and verifies its hit rate against a
   millisecond tolerance.
8. **verifies** manifest round-trip consistency, timeline coverage and a
   per-output SHA-256. Unchanged repair runs are byte-identical; changed
   inputs change the recorded fingerprint and any hash difference is reported.

## Install

```sh
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

## Generate fixtures

All media is synthesized locally from seeded tones:

```sh
.venv/bin/python scripts/make_fixtures.py --out ./.tmp-fixtures
```

The fixture set covers clean, damaged (truncated + corrupt packet + missing
index entry), truncated-only, reordered, timestamp-collision and
missing-segment playlists, in both HLS and DASH form.

## Acceptance run

```sh
.venv/bin/python -m archive_media.cli verify \
  --input ./.tmp-fixtures/damaged.m3u8 \
  --output ./.tmp-output \
  --rate 48000 --layout stereo --target-lufs -18 \
  --queue-size 4 --index-tolerance-ms 40
```

Printed metrics include `roundtrip_consistency`, `keyframe_hit_rate`,
`segment_sha256_count`, `segment_sha256_mismatches`, `queue_peak_items`,
`recovered_segments` and `damaged_segments`. A full JSON report plus the
keyframe index are written under the output directory.

The fatal path:

```sh
.venv/bin/python -m archive_media.cli verify \
  --input ./.tmp-fixtures/truncated.m3u8 --output ./.tmp-strict --strict
```

prints the error classification and locator (manifest, representation,
sequence, time range, reference) and exits `2`.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success, including recovered-but-damaged segments |
| 2 | fatal damage under the selected policy (e.g. `--strict`) |
| 3 | invalid parameter or manifest syntax error |

## Tests

```sh
.venv/bin/python -m pytest
```

## Layout

```
src/archive_media/
  timeline.py        rational time basis, planner, gap/collision detection
  errors.py          locator error model, codes, severity and exit policy
  manifest_core.py   segment plan, safe reference normalization, dispatch
  manifest_hls.py    HLS media-playlist parser and round-trip renderer
  manifest_dash.py   DASH SegmentTemplate/SegmentTimeline parser/renderer
  manifest.py        facade and round-trip entry point
  demux.py           streaming PyAV demux with bounded retry and recovery
  audio_layout.py    channel matrix and stateful resampler
  audio_loudness.py  streaming BS.1770 meter and per-chunk gain control
  audio.py           AudioConfig / AudioProcessor facade
  pipeline.py        bounded queues, backpressure metrics, policies
  pipeline_writer.py segment rendering, silence holes, deterministic WAV
  pipeline_runner.py two-pass threaded recovery orchestration
  index.py           decodable-point scan and tolerance hit verification
  verify.py          round-trip, hashes, fingerprint, timeline coverage
  cli.py             terminal verify command and exit-code handling
scripts/             deterministic fixture encoders and generator
tests/               component and end-to-end recovery tests
```
