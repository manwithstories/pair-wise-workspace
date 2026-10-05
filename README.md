# telem_replay

Offline, resumable replay of SCADA gateway telemetry logs. Pure Python 3.11+
standard library — no third-party packages, nothing to `pip install`.

## Quick start

```bash
python3 tools/gen_input.py --frames 400000 --sessions 24 --out input.jsonl

python3 -m telem_replay replay input.jsonl --state state.db --output out.jsonl --dlq dlq.jsonl
# Ctrl+C at any point -> checkpoint is saved

python3 -m telem_replay resume input.jsonl --state state.db --output out.jsonl --dlq dlq.jsonl
python3 -m telem_replay stats  state.db
```

## Layout

| file | role |
| --- | --- |
| `telem_replay/protocol.py` | frame header, TLV codec, CRC-32C |
| `telem_replay/reassembler.py` | bounded sliding-window fragment reassembly |
| `telem_replay/checkpoint.py` | SQLite (WAL) atomic checkpoint store |
| `telem_replay/state_machine.py` | ordered frames -> switch / measurement timeline |
| `telem_replay/pipeline.py` | streaming orchestrator, one pass, constant memory |
| `telem_replay/__main__.py` | `replay` / `resume` / `stats` CLI |

Data flow: line -> base64 -> TLV header -> CRC -> sliding-window reassembly ->
state machine -> `out.jsonl`, with three independent `try/except` layers routing
failures to `dlq.jsonl`.

## Wire format

26-byte header, little endian: `magic | version | flags | seq | frag_index |
frag_count | body_len | total_len | crc32c`, followed by that fragment's body.
Bodies are a flat TLV sequence (`u16 tag`, `u16 len`, value).

**The CRC covers `magic | version | seq | frag_count | total_len` plus the
reassembled body** — note what is *excluded*: `flags`, `frag_index` and
`body_len`, which differ between the fragments of one frame. A fragmented frame
can only be checksummed once every fragment has arrived, so the cover must be
identical for all of them; anchoring it to frame-invariant fields is what makes
the check well defined. Every fragment of a frame therefore carries the same
CRC, verified once against the joined body.

**CRC-32C is not `zlib.crc32`.** `zlib.crc32` is CRC-32/ISO-HDLC (polynomial
`0xEDB88320`), a different checksum. CRC-32C (Castagnoli, reversed polynomial
`0x82F63B78`) is implemented here with a slice-by-8 table loop. Measured on this
machine that runs ~17 MB/s against ~21 GB/s for `zlib.crc32` — a 1200x gap — so
the pipeline checksums **once per reassembled frame, never once per input
line**. That is what makes the throughput target reachable at all.

## Memory

Constant, by construction. Nothing accumulates across lines except in-flight
fragments (bounded by `--window` per session, `--max-sessions` overall) and
per-session state views. The input is consumed with a single `for line in file`;
there is no `read()`, no `readlines()`, no whole-file load.

Measured peak RSS, 24 sessions:

| input | lines/s | peak RSS |
| --- | --- | --- |
| 17 MB | 70k | 22.3 MB |
| 344 MB | 85k | 29.2 MB |

A 20x larger input costs 1.3x the memory.

## Resume: the part that is easy to get wrong

The checkpoint cannot store the live read cursor. Fragments that have been read
but not yet committed into a frame live only in the reassembly window; recording
the current position would skip them on resume and lose those frames. Instead:

1. **The safe offset is per session.** For each session it is the start of the
   oldest line whose data is still only in memory. A single global minimum over
   all sessions is *unsafe*: if session A holds an old fragment while session B
   is momentarily idle, the global minimum lands after B's unread lines and B's
   frames are lost outright. (This was a real bug; the test suite caught it.)
2. **The output length is part of the same transaction.** Frames committed after
   the last checkpoint but before a crash are already in `out.jsonl`. A resume
   truncates both sinks back to the recorded lengths before appending.
3. **`last_seq` de-duplicates the re-read.** Re-reading from the safe offset
   re-supplies in-flight fragments; anything at or below the committed sequence
   number is dropped.

Verified on a 344 MB / 3M-line log interrupted by a real `SIGINT`: the resumed
run produced a frame set **identical** to an uninterrupted run — 1,959,821
frames, 0 missing, 0 duplicated, 0 content mismatches, every session still in
ascending sequence order.

The `output_offset` regression in (2) was a genuine data-loss bug: an interrupted
run commits *after* closing its output handles, so `handle.tell()` reported `0`
and the next resume truncated a 13 MB output to nothing. The checkpoint now
records the on-disk size, which is correct whether or not handles are open.

## Error containment

Three layers, each independently guarded, none of which can end the run:

| layer | catches | example |
| --- | --- | --- |
| envelope | bad JSON, bad base64, missing/mistyped fields | `not json` |
| frame | short frame, bad magic, absurd `total_len`, CRC mismatch | truncated payload |
| reassembly | inconsistent fragment metadata, CRC over reassembled body | corrupted fragment |

Dirty lines are counted, written to `dlq.jsonl` with a **bounded 512-byte
preview** (a corrupt line can be megabytes long and the DLQ must not become a
second copy of the input), and skipped.

## Reassembly

Per session: fragments are keyed by `seq`, assembled in a fixed-size window
(`--window`, default 256). A frame that completes ahead of its turn is parked in
a bounded delay queue and released as soon as the preceding sequence number
arrives, so a single gap does not stall the stream. When the window overflows the
oldest incomplete frame is evicted and a warning is recorded.

A log may legitimately begin mid-stream, so the first sequence number seen
anchors the session. A frame that turns up *below* that anchor is kept and
released at end of input rather than discarded. After a resume the anchor comes
from the checkpoint and is authoritative, so re-seeing committed frames drops
them as duplicates.

## Deviations from the brief

Two places where the specification is self-contradictory or under-specified.
Both are implemented as described here rather than silently.

1. **"every 2000 frames or 30 seconds" vs "checkpoint overhead below 5%" of
   total runtime.** These do not hold together. A commit costs ~0.1 ms; at 85k
   lines/s, 2000 frames is ~24 ms of work, so the 5% budget is satisfiable — but
   only once commits are cheap. Issuing one `INSERT` per counter key cost ~10 ms
   per commit (25-34% of runtime). Collapsing counters and metadata into two
   batched `executemany` statements brought it to **0.6% at the specified
   2000-frame default**, and throughput from 49k to ~100k lines/s. The 2000/30s
   defaults are therefore kept as specified.
   Measured overhead is per-commit cost, and it grows with the number of
   tracked sessions, since every commit writes a snapshot per session. Very high
   session counts (>128) want a larger `--checkpoint-frames`.

2. **The verification command omits `--dlq` on `resume`.** Without it, resuming
   truncates the previous run's dead-letter log. The CLI accepts it as optional
   and defaults to `dlq.jsonl`, so the brief's command still works and keeps the
   corrupt-line audit trail; pass `--dlq` explicitly to redirect it.

## Measured results

24 sessions, 344 MB / 3,000,000 lines, 2% injected corruption, single core:

```
processed  2,999,700 lines (1,960,335 frames) in 33.21s [90,319 lines/s]
corrupt    39,950
checkpoints 981 writes, 0.340s (1.03% of run)
peak RSS   29.2 MB
```

Against the stated constraints: throughput **90k lines/s** (target 50k), peak RSS
**29.2 MB** (limit 32 MB), checkpoint overhead **1.0%** (limit 5%), resume point
located in **0.17 s** (limit 1 s).

## Tests

```bash
cd tests && python3 -m unittest discover -s .
```

46 tests, standard library `unittest`. Coverage includes the CRC-32C known
vector and the trap that it differs from `zlib.crc32`, frame/fragment round-trips,
bit-flip detection, TLV truncation, every dirty-line class, window eviction,
delay-queue ordering, and the resume guarantees: batched crash/resume
convergence, no duplicate `(session, seq)`, per-session ordering, offsets landing
on line boundaries, refusal to resume onto a divergent output, and the
`output_offset` regression above.
