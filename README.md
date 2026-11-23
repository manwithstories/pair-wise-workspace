# medpipe

Local, dependency-light media pipeline for aligning multi-track capture frame
sequences, slicing them on a shared time base, detecting silence, and
fingerprinting segments so duplicate content is found rather than re-encoded.

Everything runs on the local machine with no network, device or external
service access. Sample data is generated from a seed; no fixture files are
read from disk.

## Requirements

- Go 1.25.6 (the `go` directive in `go.mod` is pinned to `1.25.6`)
- macOS or any platform Go supports

## Build and test

```sh
go mod tidy
go build ./...
go test ./...
```

## Demo

```sh
go run ./cmd/medpipe demo --seed 42 --tracks 3 --frames 240 \
  --drift-ppm 180 --wrap-at 65536 --vfr
```

The command generates a capture, compares a re-serialized sequence against the
bytes on disk, checks the keyframe index against segment-boundary queries,
injects checksum corruption, damages one frame's media framing to force a
mid-graph failure, repairs it, and prints a stable key=value report:

```
roundtrip_rate=1.000
keyframe_hit_rate=1.000
segments=5
segment_sha256_count=5
corrupt_rejected=1
recovery_reused_nodes=6
blocked_dependency=fingerprint
blocked_by=segment
...
```

Exit codes: `0` success, `2` input or dependency error, `1` internal
invariant failure.

Useful flags: `--work DIR` (keep artifacts in a known place), `--keep` (keep
the auto-created temp directory). `medpipe verify --input FILE` re-serializes
a sequence and reports whether its byte digest round-trips.

## Architecture

| Package | Responsibility |
| --- | --- |
| `internal/model` | Frames, tracks, integrity/content digests, typed errors and exit-code mapping, codec payload framing |
| `internal/clock` | Counter unwrapping, drift estimation, VFR-aware mapping to a shared nanosecond timeline |
| `internal/sequence` | The `.medpseq` reader/writer: per-frame checksums, offsets, path containment |
| `internal/index` | Sparse keyframe index and at-or-before queries |
| `internal/audio` | Energy sample collection, windowed silence detection |
| `internal/segment` | Half-open segment planning with frame-edge snapping |
| `internal/fingerprint` | Per-segment content hashes and dedupe keys |
| `internal/mp4sidecar` | ISO-BMFF manifest emission and re-read via go-mp4 |
| `internal/rtpio` | RTP packetization/reassembly round trip via pion/rtp |
| `internal/dag` | Idempotent node keys, single-flight execution, durable store, blocked-dependency reporting |
| `internal/pipeline` | Wires the graph, rehydrates cached runs, builds the report |
| `internal/synth` | Deterministic seeded capture generation |
| `cmd/medpipe` | Terminal entry point |

### Time model

Each track declares a counter width and nominal tick delta. The reader keeps
raw counters; `clock` converts them with modular forward distance, rejecting
steps at or beyond half the counter range as undecidable. Drift is estimated
from the mean of near-nominal steps so VFR gaps are not mistaken for clock
error, then removed before mapping to nanoseconds. All downstream stages use
the shared nanosecond timeline, never file order.

### Integrity vs content

Every frame carries an integrity checksum over track, sequence number, raw
timestamp and payload. Metadata, dimensions and energy are excluded so
consumer annotations cannot fake corruption. Content digests cover only track,
sequence and payload; segment fingerprints additionally bind the frame's
timeline position. Media payloads are separately framed with a codec tag so a
recorder that corrupts content without corrupting bookkeeping is still caught
at the slicing stage.

### Idempotency and recovery

DAG node keys combine the node name, sorted upstream digests and a canonical
parameter digest. Results persist in a JSON store keyed by the node key, so a
re-run with identical inputs reuses recorded results and never appends
duplicates. Concurrent submissions of the same key share one execution. When
a dependency fails, successors are blocked and the first blocked node in
topological order is reported; repairing the input and re-running resumes from
the failure without redoing completed work.
