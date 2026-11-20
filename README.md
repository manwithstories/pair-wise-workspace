# relaymedia

Local, resumable segment-relay tooling for regional live-distribution teams
that shuttle programme clips between edge nodes. `relaymedia` builds one
unified, rational timeline from companion subtitles and editorial timecodes,
cuts forwardable segments on sample-exact boundaries, locates silence,
removes cross-node duplicate audio, and can resume after a link outage while
proving that pre- and post-recovery artifacts are byte-identical.

Everything runs locally on macOS (arm64 and x86_64). There are no network
calls, no system services, no graphical dependencies and no external media;
every fixture is synthesised deterministically by a script.

## Install and acceptance run

```sh
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
.venv/bin/python -m relaymedia.cli demo --out /tmp/relaymedia-demo --seed 42 --queue-bytes 1048576
.venv/bin/python -m relaymedia.cli verify --dir /tmp/relaymedia-demo
```

All third-party packages live in the project-local `.venv`. Dependencies are
PyAV 12.x, NumPy 2.x and soundfile 0.12.x.

The demo prints a quantitative summary containing every required field:

```
bad_record_count          isolated bad records across all stages
first_error_offset_bytes  earliest located byte offset (-1 if none)
offset_error_bytes        worst damage-localisation error vs. recorded truth
resume_seconds            wall time of the checkpoint-driven second run
segment_count             sample-exact segments cut from the primary node
duplicate_count           cross-node duplicate audio matches
output_sha256             stable aggregate hash of every archived artifact
peak_buffer_bytes         highest live bytes held across all bounded queues
```

Acceptance guarantees:

* damage localisation within one record line or 32 bytes;
* resume completes in under two seconds;
* repeated runs produce an identical `output_sha256`;
* `peak_buffer_bytes` never exceeds the configured `--queue-bytes`.

Exit codes: `0` every assertion passed · `2` input/usage error ·
`3` verification failed · `4` checkpoint no longer matches inputs/parameters.

## Layout

```
src/relaymedia/
  clock.py        fractional frame rates, negative offsets, day wrap,
                  repeated stamps, sample-position mapping with rounding error
  subtitles.py    SRT/WebVTT parser with byte offsets and error isolation
  audio.py        one-pass PyAV decode, streaming resample, channel maps,
                  windowed silence runs, soundfile WAV output, damage scan
  segments.py     cue -> unified clock -> integer sample boundaries
  fingerprint.py  rate-invariant time-window hashes with timeline overlap guard
  color.py        MP4 colr box walk vs. codec context, conflict audit
  pipeline.py     bounded item+byte queues, back-pressure, streaming stages
  archive.py      streaming sink: incremental WAV, silence, fingerprints
  checkpoint.py   atomic stage cursor bound to input/parameter/payload digests
  errors.py       locatable error model and the 0/2/3/4 exit contract
  cli.py          terminal demo/verify adapter
tests/
  generate_fixtures.py  deterministic success/damage/boundary material
  test_pipeline.py      unittest suite (16 cases)
```

## How the guarantees are kept

**One rational clock.** All times are `Fraction` seconds on a single
timeline. Fractional frame rates (e.g. `30000/1001`), negative source
offsets, day-boundary wrap and repeated stamps are first-class; a record that
cannot be converted is reported with its byte offset and line, and no segment
boundary is silently moved. Subtitle boundaries that fall inside a sample
block are mapped through the clock with the exact fractional position and
retained rounding error (always under half a sample).

**Streaming, back-pressured memory.** Stages communicate through bounded
queues with both an item cap and a byte budget on one condition variable.
Decoded blocks are split below the budget before queueing, so no single
message can exceed the limit and the pipeline always makes progress. A slow
sink stalls every upstream stage; a shared accountant records the live and
peak queued bytes.

**Channels and colour are audited, not tidied.** A source whose channel count
differs from the run layout gets an explicit mapping record (mono
duplication, surplus-channel drop, silence padding). Colour evidence is read
from both the MP4 `colr` box (walked manually, including FullBox and visual
sample-entry headers) and the codec context; conflicts retain both raw
values and missing fields stay missing instead of being fabricated.

**Rate-invariant de-duplication.** Fingerprints hash fixed *time* windows
with absolute amplitude and zero-crossing bands, so the same audio at 44.1 kHz
and 48 kHz produces the same sequence. A hash run only counts as duplicate
when its ranges also overlap on the unified timeline; merely adjacent,
non-overlapping content is never de-duplicated, and every match points to the
first source that carried the content.

**Recoverable, locatable failures.** Decode errors carry stage, input byte
offset and media time. For raw ADTS AAC a container scan locates an injected
corruption marker exactly (the decoder's packet position can be hundreds of
bytes off), while the decodable prefix is salvaged and later records keep
processing.

**Content-bound checkpoints.** Each checkpoint write is atomic
(temp file + `os.replace`). It records the input digest, the parameter digest
and, per completed stage, a payload digest plus a cursor. Resume reuses a
stage only when its payload still hashes correctly and the input/parameter
digests match; any drift raises a locatable mismatch mapped to exit code 4.

## Tests

```sh
.venv/bin/python -m unittest discover -s tests -v
```

The suite covers the clock, subtitle isolation, sample-exact segments,
cross-rate fingerprints and adjacency exclusion, queue back-pressure,
checkpoint refusal, colour conflicts and the full CLI exit-code contract.
