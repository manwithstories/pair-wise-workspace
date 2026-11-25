# bcast — local batch media recovery and evidence

`bcast` is a pure-local terminal system for overnight program/advert/highlight
batches. It scans MP4 sample tables and RTP companion records, executes bounded
segments, resumes after a simulated interruption, and verifies per-segment
provenance hashes against a clean rerun.

## Setup

```bash
curl -fL https://go.dev/dl/go1.25.6.darwin-arm64.pkg -o /tmp/go1.25.6.pkg \
  && sudo installer -pkg /tmp/go1.25.6.pkg -target / \
  && rm /tmp/go1.25.6.pkg
export PATH=/usr/local/go/bin:$PATH
brew install ffmpeg
go mod tidy
```

Or run `scripts/setup-macos.sh`.

## Acceptance

```bash
go version
go build -o /tmp/bcast ./cmd/bcast
go test ./...

/tmp/bcast demo --root /tmp/bcast-run --seed 41 --selfcheck --interrupt-after 2 \
  || test $? -eq 75
/tmp/bcast resume --root /tmp/bcast-run
/tmp/bcast verify --root /tmp/bcast-run --full-scan
```

`scripts/acceptance.sh` runs the same flow.

Exit codes:

- `0`: verification passed
- `1`: evidence/runtime inconsistency
- `2`: command line or path argument error
- `75`: simulated resumable interruption

Successful verification prints:

```text
roundtrip=24/24 keyframe_hit=12/12 segment_sha256=8 mismatch=0 rejected=1 rtp_duplicate=1 resumed=2 invalidated=1
segment=0 sha256=...
...
segment=7 sha256=...
```

## Design notes

- `internal/model`: multi-track timeline, clips, dissolves and rational gain.
- `internal/timebase`: integer numerator/denominator arithmetic; no float time accumulation.
- `internal/fault`: layered stable error codes and CLI exit mapping.
- `internal/index`: SHA-256 input identity, go-mp4 sample/PTS/DTS/keyframe scan, composite cache key.
- `internal/rtptrace`: pion/rtp parsing; duplicate sequence numbers and timestamp wraps are counted, never silently reordered.
- `internal/engine`: bounded segment channel, checkpoints, atomic segment reuse and resume/clean hash comparison.
- `internal/render`: FFmpeg offscreen RGB frame export and exact integer dissolve.
- `internal/rewrap`: keyframe-aligned `-c copy` rewrap and decoded raw-RGB composite encoding, validated with go-mp4.
- `internal/provenance`: ordered frame hashes, source intervals and per-segment SHA-256 manifests.
- `internal/safe`: root confinement, traversal/symlink rejection and hash-verified atomic commit.

Generated run contents include `assets/`, `records/`, `timeline/`, `frames/`,
`mp4/`, `manifests/`, and `state/`. Sample media and RTP records are generated
by Go code; no external media files or network services are used.

Memory is bounded by the segment channel capacity and the fixed
`160×120×3` RGB frame size. Copy segments do not accumulate sample bytes;
composites keep only the small fixed number of frames required by the dissolve.
