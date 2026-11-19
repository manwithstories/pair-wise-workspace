# mediaqc

Batch quality control and repair for timestamped MP4/MOV delivery material.
mediaqc scans each asset, repairs recoverable timestamps and structural damage,
rebuilds a random-access index, verifies its own output independently, and only
then atomically installs it. Damaged bytes are reported as **input byte
offsets**, never as vague "file is corrupt" messages.

Everything runs locally. The only dependencies are FFmpeg (libavformat,
libavcodec, libavutil) and Catch2, both available from Homebrew.

## Build and run

```sh
brew install cmake ffmpeg catch2
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build --parallel
ctest --test-dir build --output-on-failure
```

Generate the boundary fixtures, process them, and re-verify the outputs:

```sh
./build/mediaqc selftest --generate-fixtures --work ./build/demo
./build/mediaqc batch  --input ./build/demo/input --output ./build/demo/output --resume --json
./build/mediaqc verify --input ./build/demo/output --json
```

`scripts/make_fixtures.sh [WORK_DIR] [BINARY]` generates the fixture set on its
own (it drives `ffmpeg` through the binary and prints the follow-up commands).

## Commands

| Command | Purpose |
| --- | --- |
| `selftest --generate-fixtures --work DIR [--json]` | Generate 12 boundary fixtures and run the batch over them. Exit 0 means every fixture behaved as designed — including the ones that must fail. |
| `batch --input DIR --output DIR [--resume] [--json] [--stop-after-packets N] [--fail-after-bytes N]` | Repair every media file in `DIR`. `--resume` reuses validated checkpoints. The two `--stop`/`--fail` options are failure-injection hooks used by the tests. |
| `verify --input DIR [--json]` | Reopen every output, re-check its index, seek to 0/25/50/75/100 %, and decode everything. |

Exit codes: `0` all verified, `1` at least one asset failed, `2` argument or
configuration error, `3` output verification failed.

### JSON summary keys

`files_total`, `files_ok`, `files_failed`, `files_resumed`, `files_interrupted`,
`checkpoint_refusals`, `first_error_offset_bytes`, `offset_error_bytes`,
`resume_seconds`, `output_sha256`, `index_entries`, `decode_errors`, `repairs`,
`packets`, `elapsed_seconds`, `exit_code`, and a per-file `files` array with the
full error list. `output_sha256` is a digest over the sorted per-file output
hashes, so it is identical for identical runs.

## Pipeline

```
input ──► scanner ──► timeline ──► journal ──► mux ──► verifier ──► rename
           │            │           │          │         │
      structure,     integer     payload +   faststart  independent
      offsets,       rollover/   fixed-size  moov in    re-read,
      SHA-256        missing     records     front      index, seeks,
                     repair                            decode, offsets
```

* **scanner** (`src/scanner.cpp`) opens the input once with libavformat, but
  trusts its own ISO-BMFF walk (`src/mp4_structure.cpp`) for byte accounting.
  FFmpeg happily returns a short final packet for a truncated `mdat`; mediaqc
  compares each declared sample span with the real file size and reports the
  first missing byte at EOF.
* **timeline** (`src/timeline.cpp`) repairs missing DTS/PTS, 32-bit rollover and
  bounded regressions, all in integer ticks of the track's own time base. A
  backwards jump that is neither a rollover nor jitter fails the track rather
  than guessing; the failure carries the offending packet's byte range.
* **journal** (`src/index_writer.cpp`) appends payloads and fixed-size records
  (48 bytes) so the committed length is a valid resume point.
* **mux** replays the journal through libavformat with `+faststart`, which
  reconstructs `stco/stsz/stts/stss` from packet boundaries — including for
  inputs whose `moov` was at the end, missing entries, or pointed past EOF.
  `AVFMT_FLAG_BITEXACT` plus a stable journal order make repeated runs
  byte-identical.
* **verifier** (`src/verifier.cpp`) reopens the partial output on its own
  context, checks every index entry against the file size, seeks to fixed
  timeline points, decodes every packet, and maps decode errors back through
  the journal to the exact input byte range.

Timestamps are always integer ticks in an explicit rational base; all
cross-track comparisons use the canonical `1/90000` base. Floating-point
seconds are never accumulated.

### Error model

Every `Error` (`src/media_error.hpp`) carries:

* the input path, container track id and demuxer stream index;
* the pipeline stage (`open`, `structure`, `scan`, `timeline`, `write`,
  `index`, `verify`);
* the first affected byte offset and, when the exact byte cannot be isolated,
  the exclusive end of the uncertainty span (`offset_uncertainty()`);
* the last offset whose bytes were confirmed;
* the timestamp (stream ticks and time base) when relevant;
* the recovery action taken, so nothing is dropped silently.

Localisation guarantees:

| Damage | Reported offset | Uncertainty |
| --- | --- | --- |
| Truncated tail | first missing byte (file size) | 0 bytes |
| Corrupt sample | start of the sample | ≤ 1 packet |
| Bad/incomplete `stco` | the offset field inside `moov` | 4 or 8 bytes |
| Missing `moov` | end of the parsed box chain | 0 bytes |
| Truncated `moov` | first byte past EOF | 0 bytes |
| Zero-length input | 0 | 0 bytes |
| Ambiguous backwards DTS | start of the offending packet | 1 packet |

### Resume

Checkpoints (`src/resume_store.cpp`) are written atomically (temp file, `fsync`,
`rename`) and record the stage, input SHA-256, config digest, committed input
offset, packet counts and per-stream timeline state. On `--resume`:

* all three of input checksum, config digest and output path must match, and
  the output hash must still match if the stage is `verified`;
* any mismatch refuses the resume and explains why (the asset is rebuilt);
* a mid-stream interruption resumes by skipping the committed global packet
  count, so the resulting output hash equals an uninterrupted run's.

## Fixtures

`src/fixtures.cpp` encodes a small clip from `lavfi` sources, then patches bytes
to produce the boundary set. Nothing is downloaded and no external media is
referenced.

| Fixture | Case | Expected |
| --- | --- | --- |
| `01_clean.mp4` | baseline | ok |
| `02_truncated_tail.mp4` | `mdat` cut mid-sample | ok, tail dropped |
| `03_corrupt_packet.mp4` | sample body overwritten | ok, decode error localised |
| `04_index_out_of_range.mp4` | chunk offset past EOF | ok, index rebuilt |
| `05_incomplete_index.mp4` | `stco` entry count too large | ok, index rebuilt |
| `06_moov_at_end.mov` | `moov` after `mdat` | ok, index moved in front |
| `07_no_moov.mp4` | `moov` removed | fail at last confirmed offset |
| `08_truncated_moov.mp4` | `moov` cut mid-table | fail at first missing byte |
| `09_timestamp_wrap.mp4` | 32-bit decode-time rollover | ok, unwrapped |
| `10_backwards.mp4` | backwards jump, no rollover | fail at the packet |
| `11_zero_length.mp4` | 0 bytes | fail at 0 |
| `12_stale_checkpoint.mp4` | preseeded bad checkpoint | resume refused, rebuilt |

## Layout

```
CMakeLists.txt            build, FFmpeg via pkg-config, Catch2 tests
src/media_clock.hpp       rational time base and integer rescaling
src/media_error.hpp       stage/track/byte-offset error model
src/mp4_structure.{hpp,cpp}  independent ISO-BMFF walker
src/scanner.{hpp,cpp}     libavformat front end with cross-checks
src/timeline.{hpp,cpp}    timestamp repair
src/resume_store.{hpp,cpp}  atomic checkpoints
src/index_writer.{hpp,cpp}  journal and index-rebuilding muxer
src/verifier.{hpp,cpp}    independent output verification
src/pipeline.{hpp,cpp}    batch orchestration, JSON, verify/selftest
src/fixtures.{hpp,cpp}    boundary fixture generator
src/main.cpp              CLI and exit codes
tests/*.cpp               Catch2 suites (clock, timeline, structure, resume, pipeline)
scripts/make_fixtures.sh  fixture generation helper
```

## Limits

`Limits` in `src/config.hpp` caps input size (16 GiB), track count (64), packet
size (64 MiB), decoded index entries (4 M samples) and `moov` size (64 MiB).
Inputs are rejected, not truncated, when they exceed a limit, and paths that
resolve outside the batch input directory are skipped with a `path_escape`
error. Memory use is bounded: packets are streamed through the journal one at a
time, and per-asset bookkeeping is size-capped.
