# archivecheck

Restores early, batch-archived CMAF/fMP4 program assets. It takes an explicit
manifest of init and media segments, validates every box and sample range while
streaming, repairs the decode timeline, builds a random-access index, and proves
the result with an independent re-read.

Everything is local: fixtures are generated in-process, no external transcoder
or service is used, and all timestamps are handled with exact rational
arithmetic.

## Requirements

- macOS (Darwin) with Go 1.25.6
- No services or network access at runtime

`go.mod` pins `go 1.25.6`, `github.com/abema/go-mp4 v1.4.1` and
`github.com/pion/rtp v1.8.13`.

## Commands

Run from the project directory:

```sh
go mod tidy
go build ./...
go test ./...

# Generate input, commit 7 fragments, stop, then resume to completion.
go run ./cmd/archivecheck demo \
  --work ./tmp/archive --queue 4 \
  --slow-consumer 25ms --interrupt-after 7 --resume

# Independently re-read and verify the finished output.
go run ./cmd/archivecheck verify --work ./tmp/archive
```

The demo prints the verifiable fields, for example:

```
status=cancelled stage=interrupt last_committed=frag-008.m4s committed=7 fragments=12 peak_queue=4
fragments=12 repaired_tfdt=2 missing_mfhd=1 first_error_offset=4096 truncation_error_bytes=0
index_entries=48 peak_queue=4 resume_ms=368 output_sha256=<64 hex digits>
verified=true timestamp_regressions=0 invalid_index_offsets=0 output=./tmp/archive/output.acx
```

`verify` prints:

```
verified=true timestamp_regressions=0 invalid_index_offsets=0 index_entries=48 output_sha256=<64 hex digits>
```

Exit codes:

| Code | Meaning |
| ---- | ------- |
| 0 | Verified / completed |
| 2 | Input or verification failure |
| 3 | Incompatible resume state (checkpoint does not match input or output) |
| 4 | Processing cancelled (interrupt or signal) |

`archivecheck resume --work DIR` continues from a checkpoint left by a previous
process. `demo` without `--resume` treats `--interrupt-after` as terminal and
exits 4; with `--resume` it scripts the interruption and continuation in one
invocation.

## Layout

```
cmd/archivecheck/        CLI adapter (demo / verify / resume)
internal/clock/          exact rational time bases and cross-track mapping
internal/fault/          offset-bearing error model shared by all subsystems
internal/parse/          streaming box parser + moof/traf/mfhd/tfdt/trun validation
internal/repair/         deterministic ordering + tfdt timeline repair
internal/index/          random-access index + crash-safe checkpoint codec
internal/pipe/           manifest loading, bounded queues, stage orchestration
internal/verify/         normalized output writer and independent re-reader
internal/testkit/        self-generated normal and corrupted fixtures
```

## Design notes

**Streaming parse with exact offsets.** `internal/parse/boxes.go` reads one box
header at a time and checks size, header size, 64-bit extended sizes, and file
bounds before touching the payload. Structural payloads are capped at 8 MiB.
Every fault carries the segment name, box type, and raw byte offset.
`WalkPayload` applies the same checks to nested boxes.

**Fragments.** `ParseFragmentReader` requires exactly one `moof` and a following
`mdat`, then validates `mfhd`, per-`traf` `tfhd`/`tfdt`/`trun`, resolves sample
durations/sizes/flags from `trun` → `tfhd` defaults → `trex` defaults, and proves
each sample range lies inside the mdat payload. TRUN sample counts that exceed
decoded entries are rejected.

**Deterministic sequencing.** `ProbeSegment` reads only `moof` to obtain mfhd
metadata for ordering. `repair.Order` sorts by declared sequence, keeps
mfhd-less fragments at their manifest slot, reports every missing number once,
drops byte-identical duplicates, and rejects conflicting duplicates or
non-increasing declared sequences. `CanonicalSequences` maps the repaired order
onto stable 16-bit counters using the same fixed sequencer used for RTP.

**Timeline repair.** `repair.Builder` keeps a per-track next-decode-time and
rewrites a backward `tfdt` to the previous end, recording the original base,
the repaired base, and the reason on every affected fragment. Because the
builder is incremental it can be seeded from a checkpoint and continued.

**Random-access index.** `index.Normalize` sorts entries by track, decode time
and output offset, then cuts sync-delimited intervals. An interval that does not
begin with a sync sample is marked `seekable=false`, which makes that range
explicitly non-jumpable. `SeekPoints` returns the minimal random-access set.

**Bounded pipeline.** `pipe.Run` connects three stages over two bounded channels
(`QueueCapacity`): parse (reads and validates each fragment), transform (repairs
the timeline), and verify+write (re-reads each just-written span and only then
advances the committed boundary). A slow consumer blocks the producer instead of
buffering; peak occupancy is reported and can never exceed the configured
capacity. Cancellation stops every stage and reports the last committed
fragment.

**Crash-safe checkpoints.** `index.Save` writes a temp file in the same
directory, fsyncs, and atomically renames it. The checkpoint covers the input
manifest digest, committed-output prefix digest and byte length, committed
fragment list, per-track resume cursor, repair counters, and the normalized
index, with a SHA-256 over all of it.

**Resume safety.** `pipe.ResumeStart` refuses a checkpoint whose input digest,
fragment count, or boundary does not match the manifest. `verify.ResumeWriter`
re-hashes the committed prefix of the existing output and refuses to continue if
it does not match, so old artifacts are never overwritten with a bad tail.

**Independent verification.** `verify.Check` re-reads the entire container,
recomputes timestamps and sample offsets from the bytes on disk, checks
monotonicity and sample boundaries, compares the checkpoint index against the
re-read index, and compares the checkpoint's committed digest with the output
content digest. Repeated runs of the same input produce identical output and
therefore the same SHA-256.

## Output container

`output.acx` is a deterministic record stream:

```
"ACXARCH1"
repeat: 0x01 | name(u16 len + bytes) | track_id | sequence | timescale |
        original_base | repaired_base | reason | sample_count |
        repeat: dts | end_dts | duration | size | flags | cto | payload
0x00 | sha256(content)
```

The trailing digest covers everything before it, so a truncated or edited
output fails verification even if the checkpoint still checks out.
