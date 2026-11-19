# mediq

`mediq` validates a supplied broadcast frame sequence (image frames with capture
metadata, sequence numbers and SHA-256 checksums) and re-wraps the valid frames
into a random-access MP4 index. It is a terminal-only tool: no graphics, no
background services, no network.

The workload it is built for:

* **delivery floors** need every problem frame located precisely;
* **on-duty engineers** need to resume an interrupted run without redoing work;
* **archivists** need proof that the indexed output reproduces the frame bytes
  unchanged.

## Build and run

```sh
brew install go
go mod download
go test ./...
go run ./cmd/mediq fixture --dir ./tmp/fixtures --frames 10000 --corrupt 7311 --truncate-tail 1
go run ./cmd/mediq process --input ./tmp/fixtures/damaged.seq --output ./tmp/out.mp4 \
    --checkpoint ./tmp/job.ckpt --chunk-frames 256 --queue 8
go run ./cmd/mediq verify --input ./tmp/out.mp4 --source ./tmp/fixtures/damaged.seq
```

Interrupt/resume demonstration (build once, then run the script):

```sh
go build -o ./tmp/mediq ./cmd/mediq
sh scripts/demo.sh ./tmp/fixtures 0.4
```

## Commands

### `mediq fixture`

Writes deterministic sample sequences into `--dir`:

| file | defect exercised |
| --- | --- |
| `normal.seq` | none |
| `damaged.seq` | one frame whose payload was changed after hashing (`--corrupt N`) |
| `wrap.seq` | a 32-bit capture-clock rollover after frame `--wrap-at N` |
| `duplicate.seq` | a repeated record and an out-of-order pair |
| `truncated.seq` | the last record cut in half (`--truncate-tail N`) |
| `slow.seq` | identical to `normal.seq`, used with `process --slow-ms` |

Every fixture is produced by the same encoder the production reader parses.

### `mediq process`

Reads records from `--input`, validates them, repairs the timeline and writes an
MP4 index to `--output`, checkpointing every `--chunk-frames` samples. The queue
depth is `--queue`.

Terminal fields:

```
frames_read=10000
frames_indexed=9999
frames_bad=1
frames_quarantined=0
first_corrupt_frame=7311
first_corrupt_offset=4695645
committed_chunks=40
recovered_chunks=0
resumed=false
resume_ms=953
peak_queue_depth=8
output_sha256=de859b4a267ab993345257cc2b02bccfc588960a71c7a5422090dad2ab1743ba
index_valid=true
```

* `frames_bad` — records rejected by checksum/metadata/truncation checks.
* `frames_quarantined` — structurally valid frames the timeline stage could not
  place unambiguously (duplicate sequence, impossible jump, …). Per-code counts
  are printed as `quarantined_<code>=N`.
* `recovered_chunks` — chunks restored from a previous checkpoint.
* `resume_ms` — wall time of this invocation (including verification).
* `peak_queue_depth` — deepest the bounded queue ever got; never exceeds
  `--queue`.

### `mediq verify`

Re-reads the published MP4. With `--source`, every sample is hashed and matched
against a valid source frame in order, proving the frame bytes were not altered
and that no valid frame was silently dropped.

```
samples=9999
chunks=40
first_media_90k=0
last_media_90k=29997000
timestamps_monotonic=true
source_faults=1
unindexed_source_frames=0
output_sha256=de859b4a267ab993345257cc2b02bccfc588960a71c7a5422090dad2ab1743ba
index_valid=true
```

## Exit codes

| code | meaning |
| --- | --- |
| 0 | completed; verification passed (isolated bad frames are still 0) |
| 1 | unexpected local failure, including an unreadable `verify` target |
| 2 | invalid arguments |
| 3 | input damaged beyond recovery (framing lost, nothing publishable) |
| 4 | checkpoint does not belong to this job |
| 5 | published output failed verification |
| 130 | interrupted by SIGINT |

A run stopped with SIGINT keeps its last committed chunk and checkpoint. Rerun
the identical command to resume; the resumed output is byte-identical to an
uninterrupted run.

## Input format

A sequence is a concatenation of records, all big-endian:

| offset | size | field |
| --- | --- | --- |
| 0 | 4 | magic `MFRM` |
| 4 | 1 | version (1) |
| 5 | 1 | flags |
| 6 | 2 | header length (72) |
| 8 | 8 | frame sequence number |
| 16 | 8 | capture timestamp (ticks) |
| 24 | 4 | timebase numerator (ticks/second) |
| 28 | 4 | timebase denominator |
| 32 | 4 | metadata length |
| 36 | 4 | frame length |
| 40 | 32 | SHA-256 of the frame bytes |
| 72 | `metadata length` | RTP metadata carrier |
| … | `frame length` | frame bytes |

The metadata carrier is a normal RTP packet (`pion/rtp`): the header carries the
sequence number and the 32-bit timestamp, the payload carries a human-readable
summary, and the RFC 8285 one-byte extension `0x0E` carries the full 64-bit
timestamp so values past 2³² survive.

Only this record format is supported. Truncation is reported at the first
missing byte; a short header, short body or unknown magic is a diagnosable fault
rather than a silent stop.

## Media clock and repair

Every timestamp is converted to one integer 90 kHz clock by exact rational
arithmetic (`ticks × 90000 × den ÷ num`, half-up). The repairer then:

1. buffers a bounded reorder window and emits frames in sequence order;
2. telescopes a 32-bit capture-clock rollover into a single forward tick step;
3. clamps small backwards jitter (≤ 5 ms by default) forward to keep the media
   timeline strictly increasing;
4. quarantines — rather than guesses at — a duplicate sequence number, a frame
   that arrives after its slot closed, or a jump that cannot be a wrap, a drop
   or jitter. Each quarantine names the frame, its byte offset and the reason.

## MP4 index

The output is plain ISO-BMFF: `ftyp`, a 64-bit `mdat` with one sample per
indexed frame, then a `moov` with `stts`/`stsc`/`stsz`/`stco` (`co64` past 4 GiB).
Samples use the declared entry type `mfrm`; the media timescale is 90 kHz, so
`stts` decode times are directly seekable by timestamp. The file is assembled at
`OUTPUT.part` and atomically renamed only when the index is complete and
verified; versions of the format are never partially published.

## Checkpoints

`--checkpoint` (default `OUTPUT.ckpt`) is published atomically through
`ckpt.part` + rename. It records:

* the input's committed byte offset and the CRC-32 of that prefix;
* the index state (samples, chunks, payload length and CRC, last media time);
* the timeline repairer state, so resume continues the same clock;
* a chained digest of the chunk log.

The chunk log (`ckpt.chunks`) is append-only, so checkpoint cost does not grow
with job length. On resume the tool re-checks the input size and prefix digest,
truncates the index part file and tables to exactly the committed sizes,
re-verifies the committed payload CRC and the chunk-log chain digest, and
refuses (exit code 4) on any mismatch. A checkpoint claiming the job already
completed is likewise refused, so a stale one can never mask a rerun. A
successful publish removes the checkpoint and chunk log: a finished delivery
leaves no resumable state behind.

## Resources

* Memory is constant in the number of frames: the reader holds one record, the
  reorder window is bounded, the index tables are spooled to disk, verification
  streams both the sample tables and the source. Measured maximum RSS is about
  13–16 MB for 10 thousand, 100 thousand and 1 million frames.
* Committing a chunk performs durability barriers (`fsync`). They cost a few
  milliseconds each on the development machine, so raise `--chunk-frames` to
  trade resume granularity for throughput: 1,000,000 frames take about 8 s with
  4096-frame chunks and about 90 s with 256-frame chunks.

## Layout

```
cmd/mediq/main.go          flags, terminal output, exit codes
internal/model             frame, record header, 90 kHz clock, faults
internal/source            bounded record reader, prefix checksum
internal/validate          RTP metadata carrier and SHA-256 checks
internal/timeline          wrap/reorder/duplicate/drift repair
internal/index             MP4 writer, streaming reader, verifier
internal/checkpoint        atomic checkpoint and chained chunk log
internal/pipeline          bounded queue orchestration, batching, recovery
internal/fixture           deterministic sample generation
```
