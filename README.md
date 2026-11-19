# pairflow/media

Restores the time relationship of multi-track programmes between an
origin and an edge node, and exports a reviewable, deterministic frame
bundle. Everything runs locally on generated fixtures: no network, no
external media, no GUI.

## Quick start

```sh
go run ./cmd/mediatimeline demo --seed 41 --drift-ppm 85 \
    --wrap-at 4294967000 --out /tmp/pairflow-frames
go test ./...
```

`demo` prints a JSON summary and writes the bundle. Exit codes:
`0` validation passed, `1` input or validation failure, `2` bad flags.

`check` runs every stage without writing frames:

```sh
go run ./cmd/mediatimeline check --frame-rate 25 --json=false
```

## Summary fields

| field | meaning | acceptance |
| --- | --- | --- |
| `roundtrip_rate` | MP4 decode deltas re-derived exactly from the parsed sample table | ≥ 0.99 |
| `keyframe_hit_rate` | queried times resolved to an in-bounds sync sample | ≥ 0.98 |
| `segment_sha256_count` | distinct segment digests in the exported bundle | ≥ 8 |
| `recovered_records` | subtitle cues parsed across damaged corpora | ≥ 2 |
| `errors_with_offset` | distinct error records carrying a byte offset | ≥ 2 |
| `vfr_samples` | samples whose decode delta differs from the nominal frame | ≥ 10 |

Additional fields report RTP wraps, duplicates, backsteps, jumps,
drift anomalies, the fitted drift and offset, and the RTP→MP4 sync
residual before and after correction.

## Architecture

```
cmd/mediatimeline       terminal CLI (JSON or human output, exit codes)
internal/clock          exact rational seconds, tick bases, drift fit,
                        counter unwrapping, the shared SourceError type
internal/subtitle       SRT and WebVTT parsers, byte offsets preserved
internal/mp4            deterministic fixture builder and sample-table
                        index with keyframe lookup
internal/fixture        SMPTE timecode (24/25/30/30000/1001/24000/1001,
                        drop and non-drop, day wrap) and the media corpus
internal/timeline       multi-track composition, priority, windows and
                        subtitle conflict policies
internal/render         offscreen PPM frames, per-segment SHA-256, manifest
internal/pipeline       stage orchestration, validation and the summary
internal/safepath       output path validation and atomic writes
```

Every timestamp is an exact fraction of seconds (`clock.Rational`,
backed by `math/big`). Floating point appears only when converting for
presentation. Accumulating 30 000 frames of 1001/30000 s yields exactly
1001 s.

## Behaviours worth knowing

- **Drift** is fitted with a Theil–Sen robust seed, then refined by
  least squares on the retained samples, so jumped timestamps cannot
  rotate the line. Anomalies keep their raw node value and are reported
  with a residual.
- **RTP** timestamps are extended to 64 bits without ever rewriting the
  wire value; duplicates, backsteps and forward jumps are flagged, and
  underflow before the first wrap is an error.
- **Subtitles** tolerate a BOM, CRLF, legal overlaps and a truncated
  tail. A malformed record keeps its byte offset in the error and the
  parser continues with the next record.
- **MP4** duplicate PTS, missing samples, truncated size tables and
  out-of-bounds chunk offsets are reported at their byte or table
  position; none are silently repaired.
- **Subtitle conflicts**: within a track the configured policy
  (`last-wins`, `keep-both`, `reject`) applies; across tracks the higher
  priority track wins, with the same policy deciding the loser's fate.
- **Export** is deterministic: identical inputs produce identical frame
  and segment digests. Frames are written to a temporary file and
  atomically renamed. Paths containing `..` are refused.
- **Validation gating**: a failing input-stage check prevents any frame
  from being written; `--continue-on-error` writes anyway while still
  reporting the failures.
