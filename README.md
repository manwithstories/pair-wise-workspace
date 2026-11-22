# Archive Processing Kernel

A reusable Go kernel for receiving recovered programme audio packets from an
old capture chain, ordering them into a trustworthy timeline, and delivering a
segmented, multi-bitrate archive that can be resumed and audited.

Everything is generated locally from a seed. The kernel reads no external
files, opens no sockets and never writes outside the project directory.

## Quick start

Requires Go 1.23.x (the module pins `go 1.23.0`). Works on Apple Silicon and
Intel macOS; no system media frameworks are used.

```sh
go mod tidy && go build ./... && go test ./...

go run ./cmd/archive demo --seed 42 --duration 120s --late-window 2s \
  --bitrates 64k,128k --interrupt-after 40s --resume
```

Both acceptance scenarios can be run at once with `scripts/acceptance.sh`.

### Output fields

The demo prints `KEY=value` lines that are stable and machine-parseable:

| Field | Meaning |
| --- | --- |
| `AUDIO_VIDEO_SYNC_P95_MS` | P95 residual offset between the underlying packet edges of the two renditions at shared boundaries (target ≤ 40) |
| `PACKET_LOSS_RECOVERY_RATE` | Recoverable loss holes ÷ (loss holes + unrecoverable holes); unrecoverable gaps stay in the denominator (target ≥ 0.85) |
| `PIPELINE_P95_MS` | P95 wall-clock time to build and commit one segment (target ≤ 300) |
| `REPRESENTATION_BOUNDARY_MAX_DELTA_MS` | Largest difference between the delivered cut instants of the representations; all cuts are on one grid, so this is 0 |
| `RESUME_OUTPUT_SHA256_MATCH` | Resumed digest equals a clean uninterrupted reference run |
| `REPLAY_VERSION_COUNT` | Highest segment version reached by deterministic late-event recalculation (target ≥ 2 when in-window late traffic exists) |
| `ACCEPTANCE` | `pass` or a per-target breakdown |

Additional lines (`OUTPUT_SHA256`, `QUARANTINED_PACKETS`, `LOSS_HOLES`,
`UNRECOVERABLE_GAPS`, `ESTIMATED_SILENCE_US`, `BOUNDARY_MAX_SKEW_MS`, …) give
the underlying evidence. No value is hard-coded: each is computed from the
generated sample.

Exit codes: `0` success and acceptance targets met, `2` invalid input or
parameters, `1` processing failure or a missed target.

## Architecture

Three subsystems share one microsecond time base, one packet identity
(`stream + extended sequence`) and one error taxonomy (`internal/model`).

**Ingest** (`internal/ingest`, `internal/codec`, `internal/clock`)
Parses pion RTP records, unwraps 32-bit timestamps and 16-bit sequence numbers,
rejects >1 MiB payloads, illegal clock jumps and non-positive durations, and
derives each packet's duration from the Opus TOC table or the RFC 3640 AU
header. A payload whose duration cannot be proven is never assigned one.

**Timeline** (`internal/timeline`, `internal/recovery`)
Orders events by media time under an arrival watermark. Events beyond the
window are quarantined; events inside it force the touched committed segments
to be recalculated deterministically. Bounded gaps become labelled silence
fills with an estimated duration; gaps with no evidence (including a truncated
tail) are declared unrecoverable. Duplicate packets are de-duplicated by
stream, sequence and payload hash, so audio time is never counted twice.

**Delivery** (`internal/segment`, `internal/ladder`, `internal/checkpoint`,
`internal/provenance`, `internal/pipeline`, `internal/mp4io`)
Cuts every representation on one common grid, records the real packet-edge
skew and any final-edge trim, and writes JSON-lines manifests and a provenance
ledger whose SHA-256 chains are incremental and checkpointable. Each segment
version is materialised as a fragmented MP4 `moof+mdat` pair and re-parsed with
go-mp4 before its digest is accepted. A checkpoint stores the input offset,
configuration digest, committed segments, timeline versions and both chain
digests, so a restart resumes without changing the output hash.

## Worked rules

* **Packets are not time.** Sequence numbers order packets; media time comes
  only from unwrapped RTP timestamps converted once to microseconds.
* **Variable packet durations.** The 64k rung uses 20 ms Opus frames; the 128k
  rung uses 21333 µs AAC frames that do not divide the 10 s grid, so boundary
  deviation is a recorded value (`BOUNDARY_MAX_SKEW_MS`), not an assumption.
* **Wrap.** Both streams start near sequence and timestamp wrap so every run
  exercises reassembly across the 16- and 32-bit boundaries.
* **Idempotence.** Re-submitting an identical segment version is ignored;
  submitting a different payload for the same version is an error.
* **Replay provenance.** A recalculated segment keeps its previous version and
  hash in `replaces_version` / `replaces_sha256` plus the packet that caused it.
* **Bounded memory.** Events, duplicate indexes, quarantine evidence and
  version books are pruned to the active replay horizon, so memory tracks the
  late window and current segment, not archive length.

## Limits and validation

* Payloads are limited to 1 MiB.
* Negative/zero durations, illegal clock jumps, unknown codecs, mismatched
  clock rates, `--late-window` larger than a segment, `--interrupt-after`
  beyond the duration, duplicate bitrates and directories outside the working
  tree are all rejected with exit code 2.
* Paths are resolved through symlinks before the write-safety check.

## Layout

```
cmd/archive/            terminal adapter and report formatting
internal/clock/         unified microsecond base, wrap and jump validation
internal/model/         packet identity, event, error taxonomy
internal/ingest/        RTP parsing and counter accounting
internal/codec/         Opus and AAC duration derivation
internal/timeline/      watermark, gap policy glue, replay version book
internal/recovery/      deterministic silence-fill / unrecoverable policy
internal/segment/       chained manifest writer
internal/ladder/        common-grid representation alignment
internal/checkpoint/    atomic resumable state store
internal/provenance/    chained provenance ledger
internal/mp4io/         fMP4 init/media writer with strict read-back
internal/pipeline/      orchestration, config, report and materialisation
internal/sample/        deterministic fixtures with injectable faults
internal/metrics/       millisecond histograms for the demo metrics
scripts/acceptance.sh   runs both acceptance scenarios
```
