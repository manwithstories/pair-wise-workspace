# mp4replay

A terminal-only Go tool for gameplay-replay MP4 processing: probing, keyframe
seeking, byte-exact highlight export and per-frame content hashing. The main
pipeline (scan → parse → expand → timeline → index → remux → hash) is fully
self-developed. `github.com/abema/go-mp4` is used only inside
`internal/fixture` to serialize leaf-box payloads for generated test files.

## Build

```sh
go build ./...
go test ./...
```

## Sub-commands

| Command | Purpose |
| --- | --- |
| `gen` | synthesize a fixture (multi-track, trailing moov, elst, negative ctts, uint32 wrap) |
| `probe` | moov/trak summary as text or `--json` |
| `index` | build GOP/keyframe index and persist it as JSON |
| `seek` | nearest sync sample at or before `--at-ms` |
| `export` | self-contained MP4 clip from a keyframe, no re-encoding |
| `hash` | per-frame SHA-256 (first 16 bytes hex) plus interval root hash |
| `verify` | align a clip to its source and count per-frame matches/mismatches |
| `batch` | process a `--targets` JSON array of clips in one process |

### Demo

```sh
go run ./cmd/mp4replay gen --out fixtures/long.mp4 --tracks 2 \
  --frames 1200 --variant elst-ctts-wrap --moov-at-end
go run ./cmd/mp4replay probe --file fixtures/long.mp4 --json
go run ./cmd/mp4replay index --file fixtures/long.mp4 --out outputs/long.index.json
go run ./cmd/mp4replay seek --file fixtures/long.mp4 --at-ms 900000 --json
go run ./cmd/mp4replay export --file fixtures/long.mp4 --at-ms 900000 \
  --frames 90 --out outputs/hl_01.mp4
go run ./cmd/mp4replay hash --file outputs/hl_01.mp4 --range 0-89 --json
go run ./cmd/mp4replay verify --source fixtures/long.mp4 --clip outputs/hl_01.mp4 --json
```

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 2 | argument error |
| 3 | file read failure |
| 4 | box structure invalid, or fragmented (`moof`) input |
| 5 | seek target outside presentation range |
| 6 | frame hash mismatch |

## Design notes

- **Streaming I/O.** Sample bytes are never loaded whole-file. Parsing reads
  bounded leaf-box payloads; export streams through a 64 KiB buffer and hashing
  allocates one sample at a time, so export memory stays near a small constant
  regardless of clip length.
- **32-bit wrap.** DTS is reconstructed in `int64`. When the modular `uint32`
  next value is smaller than the current value under a positive delta, `2^32`
  is added and a wrap event is counted. A genuinely negative delta is rejected
  rather than silently smoothed.
- **Timeline axes.** Media-axis DTS/PTS (media timescale) and the presentation
  axis (milliseconds after empty edits and `media_time` offsets) are kept
  separate. Edit-list segment durations use the movie timescale and are
  converted independently of the media timescale.
- **Fixture timescale.** To make three real `uint32` rollovers observable
  within 1200 frames the wrap variant uses a dense media timescale
  (10,800,000). Consequently the unwrapped presentation span is ~2390 s while
  the `media_duration_ms` field reports the nominal 1200 s; the values are
  internally consistent and demonstrate genuine rollover reconstruction rather
  than fabricated counters.
- **Unsupported forms** (fragmented `moof`, multiple sample descriptions,
  zero timescales, count mismatches) return coded errors; unknown leaf boxes
  are skipped and counted.
