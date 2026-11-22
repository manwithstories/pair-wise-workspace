# archivecheck

`archivecheck` recovers Matroska/WebM preservation masters into a trustworthy
random-access archive. It parses the container structure, repairs media
timestamps, resequences a locally simulated RTP transfer, rebuilds a
random-access index, and verifies an archival manifest — every stage committed
through a write-ahead log so an interrupted run can resume without recounting
work.

Everything runs locally. No network calls, no external media, no credentials.
Sample data is generated in-process from a seed.

## Quick start

```sh
brew install go          # Go 1.23 or newer
go mod tidy
go build ./...
go test ./...
go run ./cmd/archivecheck demo   --seed 41 --loss-burst 3 --reorder 5 \
                                 --duplicate 2 --bandwidth-mbps 2,0.4,4 \
                                 --interrupt-after index
go run ./cmd/archivecheck verify --seed 41
```

Or run the bundled acceptance check: `sh scripts/acceptance.sh`.

## Commands

| Command | Purpose |
| --- | --- |
| `demo` | Runs four paths — **normal**, **corrupt**, **interrupted**, **resumed** — and prints the metric bundle for each. |
| `verify` | Re-runs the clean path and enforces the acceptance thresholds (bitrate within 5 %, manifest within 1 ms, non-zero quality samples, non-empty index digest). |

Common flags: `--seed`, `--frames`, `--loss-burst`, `--reorder`,
`--duplicate`, `--bandwidth-mbps`, `--memory-bytes`, `--interrupt-after`,
`--resume`, `--manifest-skew-ms`, `--workdir`, `--json`.

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Verified. |
| `2` | Input damaged but fully diagnosed (the normal result of the corrupt path). |
| `3` | Transaction recovery or integrity check failed. |

The `demo` command exits `0` when every path behaves as designed: normal and
resumed clean, corrupt diagnosed (`2`), interrupted stopped with a resumable
journal.

## What each path demonstrates

- **normal** — a clean source carried over a jittery link. All defects zero,
  `manifest_alignment_ms` at 0, bitrate on target, index digest stable.
- **corrupt** — a truncated element, a timestamp regression and a duplicate
  timestamp in the source, plus burst loss, duplication and reordering on the
  wire. Every defect is reported with its module, kind and source offset. The
  same defects propagate across EBML, timeline and transport in one report.
- **interrupted** — the run stops after committing the index stage and leaves
  a prepared-but-uncommitted manifest plus a torn journal tail.
- **resumed** — recovery truncates the torn tail, rolls back the uncommitted
  stage (`rollback_count` increments), recomputes only that stage, and
  produces the **same index digest** as the clean run.

## Metrics printed

`target_bps`, `actual_bps`, `bitrate_delta_pct`, `quality_psnr_db`,
`quality_samples`, `manifest_alignment_ms`, `lost_packets`,
`duplicate_packets`, `reordered_packets`, `timestamp_repairs`,
`index_entries`, `rollback_count`, `index_sha256`, plus `blocks`,
`spilled_chunks`, `peak_memory_bytes`, `source_truncated` and the committed /
resumed stage lists.

## Architecture

```
cmd/archivecheck        CLI: demo / verify, terminal output, exit codes
internal/clock          exact rational time base (big.Rat), no float drift
internal/fault          cross-module fault vocabulary, offsets, exit mapping
internal/ebml           VINT parsing, unknown-size masters, block reassembly
internal/transport      RTP dedupe/reorder with explicit gaps; budget+spill
internal/timeline       timestamp repair (regress/duplicate) and drift
internal/index          random-access index over verified block boundaries
internal/manifest       archival checklist with 1 ms alignment verification
internal/journal        checksummed WAL with torn-tail recovery
internal/txn            atomic staged commits, rollback and resume
internal/verify         PSNR quality and bitrate statistics
internal/fixture        seeded Matroska and RTP generators
internal/pipeline       wires the subsystems into one deterministic run
```

Data flows: the EBML subsystem hands blocks with source offsets and exact
timestamps to the timeline subsystem; the transport subsystem hands packet
sequence, gaps and bandwidth samples to the reorderer and repairer; the
transaction subsystem wraps parse, repair, index and manifest, committing only
stages that pass validation.

### Memory

Input size is unbounded, but the transport buffer is bounded by
`--memory-bytes`. Chunks beyond the budget spill to a per-run `spill/`
directory and are removed on completion. `peak_memory_bytes` never exceeds
the budget, and a spilled run produces the **same** metrics signature and index
digest as an in-memory run (covered by `TestSpillMatchesInMemory`).

### Safety

- Source files are never modified; the index, sidecar and manifest are written
  to a separate `out/` directory.
- No read ever crosses the source end: the reader is bounds-checked and a
  truncated element is reported with its offset and a recoverable boundary.
- No fabricated frames: packet gaps are explicit, and missing frames keep
  their slot marked `Missing` rather than being silently re-timed.

## Dependencies

- `github.com/pion/rtp v1.8.11` — RTP packet marshaling for the simulated link.
- `github.com/abema/go-mp4` — ISOBMFF box marshaling for the index sidecar.
  The brief pins this at `v1.20.0`, but no such release is published; the
  latest available is `v1.7.4`, which is what `go.mod` fixes. The sidecar is a
  valid `ftyp` + `free` pair that go-mp4 writes and re-reads
  (`TestSidecarRoundTripWithGoMP4`).

`go.mod` targets Go 1.23 and both `darwin/arm64` and `darwin/amd64` build
cleanly.
