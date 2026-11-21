# MediaMeld

MediaMeld ingests studio, camera and proxy MXF/MP4 material and hands an
editing system a single, verified timeline. It is a pure terminal tool: all
behaviour is reachable through flags and stdout, and it has no external service
or GUI dependency.

Everything it operates on is generated locally by Go code — no external media
files are referenced, downloaded or played.

## Quick start

```sh
go mod tidy
go build ./...
go test ./...

go run ./cmd/mediameld demo \
  --seed 17 --late-ms 240 --drift-ppm 85 \
  --wrap-at 4294960000 --vfr 24,25,23 \
  --out ./tmp/demo
```

Expected output (values are deterministic for a fixed seed):

```
tracks=3
samples=120
late_replays=1
timeline_sha256=<64 hex chars>
sample_hash_mismatches=0
target_bitrate_bps=...
actual_bitrate_bps=...
bitrate_deviation_pct=0.0884
quality_samples=120
quality_match_rate_pct=100.00
manifest_alignment_max_ms=0
```

Damaged input is isolated rather than fatal:

```sh
go run ./cmd/mediameld demo --seed 17 --case corrupt-klv --out ./tmp/corrupt
```

```
error_code=ERR_KLV_LENGTH
recovered_tracks=2
tracks=2
exit_code=2
```

A policy crop that exceeds the frame fails with the offending track named:

```sh
go run ./cmd/mediameld demo --seed 17 --case crop-conflict --out ./tmp/crop
```

```
error_code=ERR_CROP_RANGE
conflict_track=3
exit_code=2
```

### Exit codes

| Code | Meaning |
|------|---------|
| `0`  | Success; delivery and manifest written. |
| `2`  | Input defect (isolated corruption, bad length, crop out of range). |
| `1`  | Bad arguments or internal failure. |

A run that ends with exit code `2` never leaves a `manifest.json` behind, so a
damaged delivery cannot be mistaken for a complete one.

## Architecture

The data flow is fixed: demuxers emit a unified event model with a shared
integer time base and error vocabulary; the mapper and timeline share that base;
the remuxer consumes the stable timeline; the manifest reads the remux result and
independently re-verifies it.

| Package | Responsibility |
|---------|----------------|
| `internal/model` | Integer nanosecond instants, exact rational frame rates, drift-compensating clock, 32-bit unwrap, track/event types and stable error codes. |
| `internal/ingest/mp4.go` | Hardened MP4 box scan and sample-table reconstruction with byte hashes, DTS and PTS. |
| `internal/ingest/mxf.go` | KLV triplet parser with BER length bounds and isolated tail recovery. |
| `internal/ingest/rtp.go` | Length-framed RTP demux plus sequence-ordered unwrap and drift calibration. |
| `internal/mapping` | Fingerprint-bound role mapping and container → track → policy metadata adjudication. |
| `internal/timeline` | Max-end-minus-late watermark, closed-interval segments, explicit gaps and deterministic replay. |
| `internal/remux` | Verbatim sample copy with independent re-hash and fingerprint-gated resume. |
| `internal/manifest` | Delivery record plus an independent alignment/hash/segment verifier. |
| `internal/fixture` | Byte-exact MP4, MXF, KLV and RTP writers used to generate every test asset. |
| `internal/app`, `cmd/mediameld` | Acceptance pipeline and terminal entry point. |

## Invariants

- Time is always integer ticks with rational frame rates; no float accumulation
  ever reaches an ordering decision.
- The 32-bit RTP wrap is unwrapped in sequence order, so a reordered packet still
  lands on the correct side of the modulus.
- Late events are detected by arrival order, replayed from the nearest
  checkpoint, and hashed deterministically; repeat runs over the same input and
  seed produce the same `timeline_sha256`.
- Segment boundaries are closed intervals: an instant belongs to exactly one
  segment, never two.
- Display metadata is adjudicated container → track → policy. Every original
  value and the reason it was replaced is retained; unrecognized colour is
  recorded as `unknown` and never causes sample bytes to be rewritten.
- Remux copies declared byte ranges verbatim. Input and output sample hashes are
  compared after writing, and a resume checkpoint whose fingerprint does not
  match the current inputs is refused.
- All generated paths stay inside the directory passed to `--out`.

## Tests

Each subsystem has its own tests: `internal/model`, `internal/ingest`
(MP4, MXF/KLV, RTP), `internal/mapping`, `internal/timeline`, `internal/remux`,
`internal/manifest`, `internal/fixture` and the end-to-end `internal/app`.
Generated fixtures stay under 8 MiB each.
