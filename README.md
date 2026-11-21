# mediad

Deterministic media-timeline processing for remote productions.

A remote team merges studio, location and edit-suite sources (MXF and MP4)
into one randomly seekable timeline. `mediad` repairs timestamps, reconciles
conflicting metadata, recovers RTP streams over an unstable link, rebuilds a
random-access index and reconciles the delivery manifest against the media it
actually produced. Every stage is recorded in a checksummed write-ahead log,
so an interrupted run resumes only from stages that still verify.

The main deliverable is a reusable library under `internal/`. `cmd/mediad` is
a thin terminal entry point used for demonstration and acceptance.

## Install and run

```sh
brew install go@1.23
go mod tidy
go build -o /tmp/mediad ./cmd/mediad
go test ./...
go run ./cmd/mediad demo --seed 17 --faults duplicate,reorder,burst-loss \
  --bandwidth-mbps 8,2,12 --interrupt-after index --resume
```

The demo generates its own MP4 and MXF samples from `--seed`; nothing is
downloaded and no external media is read. Repeated runs with the same
arguments produce the same manifest digest.

Exit codes:

| code | meaning |
| ---- | ------- |
| 0 | verified: all stages committed and every threshold met |
| 2 | invalid input or invalid media metadata |
| 3 | recovery or commit failed |
| 4 | a quantitative acceptance gate failed |

## Output fields

| field | meaning |
| ----- | ------- |
| `target_bitrate_kbps` | bitrate the delivery spec requires (2% safety margin) |
| `actual_bitrate_kbps` | delivered bitrate computed from sample bytes |
| `bitrate_delta_pct` | `(actual - target) / target * 100` |
| `quality_psnr_db` | PSNR against the deterministic seeded payload (no frame decode) |
| `recovered_packets` | lost packets whose timing was bridged from intact neighbours |
| `unrecoverable_packets` | lost packets inside a burst too long to bridge |
| `duplicate_packets` / `reordered_packets` | receiver counters |
| `manifest_alignment_ms` | worst start/end deviation in milliseconds |
| `index_seek_mismatches` | seeks that disagreed with decode order |
| `manifest_sha256` | digest of the canonical manifest |
| `rollback_stage` / `resume_stage` | recovery boundary, `-` when not applicable |
| `injected_faults` | total faults the run injected |

A successful sample requires `|bitrate_delta_pct| <= 5`, `manifest_alignment_ms
<= 1` and `index_seek_mismatches == 0`. `--json` prints the summary alone.

## Flags

```
--seed int               fixture and payload seed (default 17)
--faults list            duplicate,reorder,burst-loss,corrupt-log
--bandwidth-mbps list    e.g. 8,2,12; drives the adaptive reorder window
--interrupt-after stage  probe|rtp|repair|index|manifest
--resume                 reuse checksum-verified stages after a crash
--dir path               working directory (default: under os.TempDir)
--seeks int              index probes per track (default 11)
--max-burst int          longest bridged packet gap (default 8)
```

## Architecture

```
cmd/mediad            terminal entry point and exit-code mapping
internal/clock        exact rational timebase (no float drift)
internal/model        shared track/sample model and unified error type
internal/probe        MP4 (go-mp4) and MXF (KLV/local-set) readers
internal/normalize    rotation snapping, crop validation, colour arbitration
internal/rtp          sequence unwrap, dedup, bounded adaptive reorder, loss gaps
internal/repair       drift, wraparound and duplicate timestamp repair
internal/index        random-access index rebuild and seek verification
internal/txn          checksummed WAL, atomic commit, rollback and resume
internal/manifest     manifest alignment and canonical digest
internal/report       acceptance summary and quality metric
internal/fixture      deterministic MP4/MXF writer, RTP packetiser, fault injection
internal/pipeline     orchestration of the stages above
```

All times are exact fractions of a second (`clock.Rational`). Comparisons and
reported errors are computed in integer microseconds, which is why alignment
is reproducible to the tick.

### Behaviour guarantees

- **Metadata conflicts.** Colour is arbitrated container < track < explicit
  and the full trace is kept. Rotation is snapped to 0/90/180/270; anything
  else is rejected with the track ID. A crop that would empty the frame is an
  error naming the track and its time range, never a silent truncation.
- **Timestamps.** Repair unwraps the 32-bit field, measures drift against the
  nominal frame rate, resolves duplicates, and forces strictly increasing DTS
  by moving only affected samples one tick. Before/after offsets are reported.
- **RTP.** Sequence numbers are unwrapped exactly; duplicates and late
  arrivals are counted separately from loss. The reorder window adapts to
  bandwidth within hard bounds and the buffer refuses overflow with an
  `over_limit` error. Gaps are explicit; bridged frames are marked
  `Synthesised`, never presented as original.
- **Recovery.** Every stage writes begin/data/commit records with CRC-framed
  payloads before renaming its artifact. A torn or corrupted tail truncates to
  the last valid record. A committed stage whose artifact fails its SHA-256 is
  rolled back to the previous atomic boundary.

## Development

Each subsystem has its own tests (`go test ./internal/<pkg>`). The pipeline
and CLI tests pin the acceptance thresholds and the exact output contract.
