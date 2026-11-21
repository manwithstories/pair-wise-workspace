# archivecheck

A reusable Matroska/WebM archive-recovery library and pipeline for local
film-archive work. It parses masters that lack indexes or have incomplete
structure, remuxes them losslessly into MP4, and produces reviewable evidence:
per-sample SHA-256 hashes, a keyframe index, a sample manifest tying every
output byte range back to a source offset, and stage/offset-aware error
reports.

Everything the test suite and the demo use is generated in code
(`internal/fixture`); no external media or services are involved.

## Quick start

```sh
go mod tidy && go build ./... && go test ./...

go run ./cmd/archivecheck demo --seed 17 --queue 4 --slow-consumer-ms 2
go run ./cmd/archivecheck verify  --fixture truncated --recover=false
go run ./cmd/archivecheck recover --fixture truncated --recover=true
go run ./cmd/archivecheck inspect --input path/to/master.mkv
go run ./cmd/archivecheck demo --seed 17 --queue 4 --slow-consumer-ms 2 --rtp
```

`--rtp` additionally frames every spooled sample as RTP packets with
`pion/rtp` and checks the payload round trip. `--keep DIR` retains the
generated fixtures and outputs for manual review.

`demo` prints, among other things:

```
roundtrip_match=100.00% keyframe_hit=100.00% segment_hash_match=100.00% peak_queue=4
```

`verify` on damaged input prints a machine-readable stage line and exits 2:

```
status=error stage=parse offset=1947 exit=2
```

`recover` prints a recovery manifest and exits 0:

```
status=recovered skipped_segments=1 source_offset=1865 hash_verified_segments=21 exit=0
```

## Exit codes

| code | meaning |
| ---- | ------- |
| 0 | verified, or recovered by the documented rules |
| 2 | corrupt input that cannot be recovered (including ambiguous boundaries) |
| 3 | runtime or output failure |

## Architecture

| package | responsibility |
| ------- | -------------- |
| `internal/clock` | integer timebase, rounding, deterministic sample mapping and digest |
| `internal/fault` | error model carrying stage, source offset and track number |
| `internal/fsguard` | input-root confinement and atomic output creation |
| `internal/ebml` | streaming EBML reader; unknown-size bounds from parent limits |
| `internal/mkv` | block de-lacing with exact source byte ranges |
| `internal/track` | track validation and documented MP4 codec mapping |
| `internal/demux` | Matroska document walk, block delivery, Cues capture |
| `internal/stream` | fixed-capacity queue with backpressure and cancellation |
| `internal/remux` | lossless MP4 writer (ftyp, mdat, moov, stbl, ctts v1) |
| `internal/verify` | independent re-read, per-sample SHA-256, gap and stss checks |
| `internal/index` | keyframe index from blocks, corroborated by Cues |
| `internal/rtpprobe` | optional RTP packet-structure validation (pion/rtp) |
| `internal/pipeline` | stage wiring, spooling, recovery classification |
| `internal/fixture` | self-generated clean and damaged Matroska documents |
| `cmd/archivecheck` | terminal entry point |

Data flow: EBML parser → block de-interleaver → track mapper, all sharing one
timebase and error model; mapped samples → bounded queue → remuxer. A slow
consumer blocks the producer instead of growing memory, and cancellation
propagates through the queue to every stage. The output is re-read by an
independent parser and checked with hashes and the rebuilt index.

Memory is bounded by `queue capacity × max block size`, never by file length:
frame payloads are spooled to a temporary file and only metadata is retained.

Each successful run also writes `<output>.manifest.json` next to the copy. The
manifest names every sample's source offset and size, the output file's
SHA-256, the mapping digest, the queue high-water mark, the proof-skipped
ranges and the rebuilt keyframe entries. It is the artifact a reviewer checks;
nothing in it is inferred from a summary line.

## Fixtures

| name | damage |
| ---- | ------ |
| `clean` | well-formed interleaved Matroska |
| `unknown-size` | unknown-size Segment and Clusters |
| `no-cues` | no Cues element; index must be rebuilt |
| `out-of-order` | interleaved blocks with regressing decode timestamps |
| `truncated` | tail cut inside the last Cluster |
| `oversized` | a size that crosses its parent while bytes remain |
| `badblock` | a SimpleBlock with an illegal track number |
| `slow-consumer` | well-formed; exercises backpressure |

## Dependency note

The brief names `go-mp4 v1.20.0`. No such version exists upstream (the latest
tag is `v1.7.4`), so `go.mod` pins `v1.7.4`, the newest published release.
`pion/rtp v1.8.13` is pinned as specified.

## Supported codecs

Only codecs with a documented, lossless MP4 sample-entry mapping are remuxed:
H.264, HEVC, VP9, AV1, AAC, Opus, FLAC and UTF-8 text subtitles. Anything else
is counted as unknown and refused by `verify`/`recover`; it is never
transcoded.
