# FieldPost

FieldPost is a pure-local Go media handoff kernel for post-show DIT, media
engineering and delivery operations. It parses HLS/DASH locations, recovers
reordered or missing RTP-sharded files, streams them through a byte-bounded
pipeline, demuxes MPEG-TS and CMAF/fMP4, maps AVC/AAC tracks by camera rules,
remuxes losslessly to keyframe-aligned CMAF, builds keyframe indexes and
verifies stable essence hashes.

## Run acceptance

```bash
go version
go mod tidy
go build ./...
go test ./...
go run ./cmd/fieldpost demo --seed 42 --profile all --memory-cap 8388608
```

Or:

```bash
./scripts/acceptance.sh
```

The demo generates all normal and adverse data in code. It writes healthy
fragments to a temporary delivery directory and never writes the isolated
unrecoverable segment.

Expected key metrics include:

```text
healthy.roundtrip=1.000
recovery.keyframe_hit=1.000
essence_sha256=12/12
peak_mem_bytes<=8388608
backpressure_events>0
boundary.rejected>=4
```

## Exit codes

| Code | Meaning |
|---:|---|
| 0 | Demo and all expected rejections/invariants passed |
| 2 | Command-line or manifest error |
| 3 | Input is unrecoverable and must be isolated |
| 4 | Hash, memory or timeline invariant failed |

## Packages

- `internal/core`: integer time bases, structured errors and essence hashes.
- `manifest`: HLS master/media and static DASH MPD parsing/validation.
- `transport`: bounded RTP sequence dedup, wrap-aware reorder and two retries.
- `pipeline`: byte-weighted backpressure, cancellation and worker cleanup.
- `demux`: streaming MPEG-TS and CMAF/fMP4 parsers plus TS fixture muxer.
- `mapping`: deterministic camera AVC/AAC track mapping.
- `remux`: lossless CMAF/fMP4 writer and atomic fragment commit.
- `index`: deterministic keyframe index and hit verification.
- `fixture`: seed-stable media, manifests, RTP impairments and boundary cases.
- `internal/demo`: acceptance orchestration and terminal report.

All media is synthetic and local. FieldPost does not decode/transcode essence,
access cameras/microphones, open a browser, or contact external services.
