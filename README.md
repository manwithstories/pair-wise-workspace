# mediarc

Local, verifiable archiving for multi-track production masters. `mediarc`
parses a MOV/MP4/MKV master, binds its video and audio streams explicitly,
normalises audio to a chosen rate and channel layout under a loudness ceiling,
carries colour metadata forward with an audit trail, isolates damaged segments
instead of failing the whole master, and writes a manifest that can be verified
years later — including from a byte offset in the source file.

Everything runs locally. There are no network calls, no external media and no
graphical dependencies.

---

## Install

```sh
brew install ffmpeg
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
```

All third-party packages are installed into the project-local `.venv`; the
system Python is never touched.

## Generate the sample fixtures

```sh
.venv/bin/python scripts/make_fixtures.py --out .tmp/fixtures
```

The fixtures are encoded locally with PyAV's bundled libx264 and are safe to
delete; regeneration is deterministic in content.

| Fixture | What it exercises |
| --- | --- |
| `good.mkv` | baseline: 3 video GOPs + stereo 44.1 kHz PCM |
| `good.mp4` / `good.mov` | MP4/MOV container, `colr` box, brand whitelist |
| `multi.mkv` | two video tracks + audio, explicit stream mapping |
| `empty_track.mkv` | a mapped audio stream that carries no packets |
| `truncated.mkv` | container cut off before its declared duration |
| `damaged.mp4` | one interior GOP damaged — **locatable and repairable** |
| `damaged.mkv` | every keyframe damaged — locatable, **not** repairable |
| `boundary.mkv` | video/audio start offsets that are not tick-aligned |
| `color_conflict.mkv` | codec colour says BT.709, tag says BT.2020/HDR |
| `keyframe_index.mp4` | `stss` index missing a real keyframe |

## Acceptance run

```sh
.venv/bin/python -m mediarc.cli archive \
    --input .tmp/fixtures/good.mkv --manifest .tmp/archive.json \
    --rate 48000 --layout stereo --lufs -16

.venv/bin/python -m mediarc.cli verify \
    --manifest .tmp/archive.json --fixtures .tmp/fixtures --self-test
```

Expected terminal output includes:

```
roundtrip_rate=1.000
keyframe_hit_rate=1.000
segment_hashes=3/3
audio_segment_hashes=1/1
recovered_segments=1
stage_chain=ok
error_offset=844
```

`error_offset` is a byte position in the **source file**, never in a rewritten
segment. When libavformat exposes no position the line reads
`error_offset=unknown`; the value is never invented.

## Commands

```
mediarc archive --input FILE --manifest FILE [--rate R] [--layout L] [--lufs X]
                [--map ROLE:INDEX ...] [--transition FROM:TO:START_MS:END_MS ...]
                [--work-dir DIR] [--resume] [--jobs N]
                [--max-file-mb N] [--max-segment-seconds S] [--cut-every S]
mediarc verify  --manifest FILE [--source PATH] [--fixtures DIR] [--self-test] [--json]
mediarc probe   --input FILE [--map ROLE:INDEX ...] [--json]
```

Exit codes: `0` every required check passed · `1` an input or processing
failure · `2` the command line itself was wrong.

## How the guarantees are kept

**Integer timeline.** All timestamps are integer ticks on a single rational
rate chosen as the least common multiple of the video time bases and audio
sample rates. No float seconds reach the manifest, so a restored edit point
cannot drift by a frame. Negative PTS, non-monotonic DTS and two frames
rounding onto one tick are diagnosed with the offending packet's source offset.

**Sample-exact audio.** Audio positions are counted from the first decoded
frame, never from container timestamps. Matroska audio commonly uses a 1/1000 s
time base, which cannot express an arbitrary sample index; seeking on it would
shift a cut by up to half a millisecond. All audio segments for one stream are
cut in a single decode pass, so a long master is read once, not once per
segment. Loudness follows ITU-R BS.1770-4 and was calibrated against FFmpeg's
`ebur128` to within 0.1 LU at 44.1/48/96 kHz.

**Colour is audited, not tidied.** Bitstream VUI, the MP4 `colr` box and
container tags are read as separate pieces of evidence. PyAV 13 does not expose
the `colr` box at all, so `mediarc` walks `moov/trak/.../stsd` itself. When the
sources disagree, the value a decoder would act on wins and both values are
recorded with the reason. Absent fields stay absent.

**Failure isolation.** A segment that cannot be archived is recorded with a
locatable error while the rest continue. Verification then fails, honestly,
because a segment is missing; a partial archive never reads as success.

**Staged resume.** Probe, timeline and transform each persist their input
digest, output digest and the upstream digest they were built on. `--resume`
reuses a stage only when its recorded input still matches, and refuses with
`stage-hash-mismatch` otherwise. `verify` checks that the recorded chain is
consistent (`stage_chain=ok`).

**Repair with an honest boundary.** `integrity_scan` decodes each GOP in
isolation to find the first packet whose loss breaks the GOP, reporting its
source byte offset. `recover` restarts at the last intact keyframe, skips only
the failing range and records the damaged offset, the resume offset and the
salvaged packet count. When no safe boundary exists it declines rather than
emitting a file that only looks repaired.

## Tests

```sh
.venv/bin/python -m unittest discover -s tests -v
```

The suite covers the success path, damaged and truncated material, empty
tracks, boundary timestamps, colour conflicts, resume refusal, manifest
round-trips, sample-exact cuts and the CLI exit-code contract.

## Layout

```
src/mediarc/
  cli.py        terminal entry point and exit-code contract
  errors.py     locatable error model (file, stream, stage, byte offset)
  timeline.py   integer tick base, tracks, segments, transitions
  probe.py      PyAV container/stream/keyframe parsing
  audio.py      resampling, layout mapping, BS.1770 loudness
  color.py      colour metadata resolution with an audit trail
  mp4color.py   MOV/MP4 colr and stss boxes (not exposed by PyAV 13)
  pipeline.py   staged orchestration, isolation and resume
  manifest.py   deterministic, round-trippable archive manifest
  verify.py     hashes, index checks, damage localisation and repair
scripts/make_fixtures.py
tests/test_pipeline.py
```
