# mediacore

Lossless multi-track delivery for MP4 and a declared MXF subset. The tool
parses container structure, maps tracks by stable identity, resolves
conflicting rotation/crop/colour descriptions, streams coded samples through a
bounded queue and rewrites the container without decoding anything.

## Requirements

- Go 1.23 (the toolchain is pinned in `go.mod`)
- No external services, no files outside the repository

Dependencies are pinned:

| Module | Version | Use |
| --- | --- | --- |
| `github.com/abema/go-mp4` | v1.4.1 | MP4 box definitions |
| `github.com/pion/rtp` | v1.8.9 | RTP datagram boundaries, header extensions, sequence numbers |

## Build and test

```sh
go mod tidy
go build ./...
go test ./...
```

Tests generate all fixtures in memory or as sparse files; nothing is read from
outside the repository.

## Demo

```sh
go run ./cmd/mediacore demo --generate --format mixed \
  --queue-bytes 1048576 --consumer-delay 20ms \
  --checkpoint ./tmp/checkpoint.json
# tracks=3 samples=720 max_queue_bytes=<n> remux_sha256_match=true

go run ./cmd/mediacore demo --generate --corrupt truncate --offset 4096
# error_class=truncated input_offset=4096 tolerance_bytes=0   (exit 2)

go run ./cmd/mediacore demo --generate --interrupt-after 120 \
  --resume ./tmp/checkpoint.json
# resume_ms=<n> replay_samples=0 output_sha256=<hex>           (exit 0)
```

`go run` itself maps any non-zero process status to 1. Build the binary to
observe the documented codes: 0 success and successful resume, 1 usage error,
2 corrupt or unsafe input.

```sh
go build -o mediacore ./cmd/mediacore
./mediacore demo --generate --corrupt truncate --offset 4096; echo $?   # 2
```

### Other demos

| Command | Demonstrates |
| --- | --- |
| `--format mxf` | the declared MXF subset |
| `--corrupt sample_length --offset N` | an illegal sample length caught from the table |
| `--format mxf --corrupt wrap` | 32-bit timestamps expanded across wrap |
| `--format mxf --corrupt ambiguous_wrap` | an unresolvable wrap, reported with its offset |
| `--corrupt cancel` | cancellation propagating upstream and releasing the queue |
| `--big` | a ~1 GiB logical sample stream read in bounded chunks |
| `--rotation N` | an explicit metadata override |
| `--strict` | reject a rotation that is not a whole quarter turn |

## Design

```
cmd/mediacore          terminal entry point and acceptance flow
internal/media         exact rational time, error classes, unified model
internal/adapter       versioned registry, MP4 box walker, MXF KLV walker
internal/metadata      container < track < override resolution
internal/pipeline      bounded queue with backpressure, checkpoints
internal/mux           structure-only remux with per-track SHA-256
internal/manifest      delivery record, including every fault location
```

**Time.** Timestamps are `media.Rat` values (`int64/int64`); floating point is
used only for display. A per-track `Unwrapper` expands 32-bit wrapping clocks
and returns the exact input offset when a transition is exactly half the range
and therefore cannot be resolved. `media.RTPClock` maps those exact times onto
the 32-bit delivery clock and uses `pion/rtp` for datagram boundaries, header
extensions and sequence numbers; the manifest records each track's RTP range.

**Errors.** Every failure is a `media.Error` carrying a class, container,
track and byte offset. `errors.Is` matches on class, and the class determines
the exit code. Truncation reports the first byte the input could not supply.

**Mapping.** Tracks are ordered by source path, media type and stable
identifier; output IDs are assigned in that order. Duplicate or missing
identifiers abort the run rather than merging.

**Metadata.** Overrides win over track descriptions, which win over container
descriptions. Rotation is normalized to quarter turns, a crop outside the coded
frame is an error, and every disagreeing colour source is kept in the manifest.

**Streaming.** The queue bounds both in-flight samples and bytes. The producer
blocks when either budget is full, so a slow consumer creates backpressure
instead of growth. Cancellation releases the byte budget and the buffers.

**Recovery.** Checkpoints pin the input fingerprint, mapping signature, output
offset and per-track SHA-256 plus hash state. Loading verifies committed
segments newest-first and truncates the output to the newest intact one. A
second resume of a completed delivery re-verifies the bytes on disk and writes
nothing.

**Losslessness.** The output `stsd` entry is copied verbatim from the input, and
every coded sample is copied byte-for-byte. The muxer hashes the copied input
stream and the bytes reread from the output, so `remux_sha256_match=true` means
the delivered sample stream equals the source.

## Limits

- MXF support is the declared subset only: a single-partition OP1a file with
  SMPTE 377M KLV framing, GC/AA essence and an optional index segment.
- MP4 support covers standard ISO base media boxes and the codecs listed in
  `supportedCodec`. Anything else fails with `unsupported_codec`.
- Sub-pixel clean apertures are rejected instead of rounded.
