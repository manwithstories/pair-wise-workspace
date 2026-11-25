# mediad — local media processing kernel

A reusable Go kernel that takes local HLS playlists and DASH MPDs, decodes
MPEG-TS and CMAF/fMP4 segments into unified samples on a single 90 kHz axis,
repairs RTP packet streams, aligns multiple camera tracks, and remuxes them
losslessly. Everything is local: no network, no re-encoding, no GUI.

## Build and test

```sh
go mod tidy && go mod download && go build ./... && go test ./...
```

Go 1.25.6, `github.com/abema/go-mp4 v1.7.4`, `github.com/pion/rtp v1.10.6`.

## Demo (all fixtures generated from a seed)

```sh
go run ./cmd/mediad demo --seed 41 --profile clean --cams 4 --segments 180
go run ./cmd/mediad demo --seed 41 --profile recover --cams 4 --segments 180 \
    --loss 0.03 --drift-ppm 120 --wrap-pts 8589900000 --vfr
go run ./cmd/mediad demo --seed 41 --profile corrupt --truncate-moof 17
```

Exit status: `0` completed and verified, `2` expected damaged input identified
and quarantined, `1` usage / I/O / internal error.

| profile | required output |
|---------|-----------------|
| clean   | `sync_p95_ms≤20`, `hash_match=true`, `output_samples=2880` |
| recover | `loss_recovery_pct≥95`, `recovered_packets>0`, `latency_p95_ms≤100` |
| corrupt | `rejected_segments=1`, `err_code=ERR_TRUNCATED_BOX`, no `output.mp4` |

## Architecture

```
cmd/mediad            terminal entry point (demo / generate / verify)
internal/media        unified 90 kHz timebase, rational math, errors, model
internal/manifest     HLS master/media + DASH MPD parsing, URI containment
internal/adapter      versioned container adapters (CMAF v1, TS v1, RTP v1),
                      track-key mapping and conflict/required-track checks
internal/recovery     RTP unwrap, reorder, dedup, redundant-copy repair
internal/timeline     per-camera drift correction, jump isolation, VFR-safe
internal/remux        lossless fMP4 writer with verified atomic commit
internal/mp4util      deterministic ISO BMFF box writing
internal/fixture      seeded fixture generation (4 cameras, 180 segments)
internal/pipeline     end-to-end orchestration + machine-checkable report
```

Data flow: manifest parsers discover segments → adapters emit unified samples
(and RTP recovery feeds the same adapter) → adapters and alignment share
`timebase.go`/`errors.go` → aligned samples go to the remuxer, which writes an
output file plus a source index with per-sample hashes.

## Guarantees

- **Unified timebase** — one 90 kHz axis; 33-bit PTS/DTS wraps are expanded
  exactly, preserving DTS/PTS and per-sample durations so VFR is never padded
  to a nominal frame rate.
- **Drift** — each camera is fit independently (Theil-Sen + robust refit);
  correction ppm, anchors and boundary error are reported, and a jump on one
  camera cannot perturb another.
- **Recovery** — 16-bit sequence wrap, reordering and duplicates are handled;
  only redundant copies restore lost packets, and unrecoverable losses are
  counted, never invented.
- **Lossless output** — encoded sample bytes are copied verbatim; the writer
  stages a temp file, verifies box structure and byte preservation against the
  source index, then atomically renames. Repeated runs produce identical hashes;
  truncated input never leaves a file that looks like success.
- **Bounded input** — manifest size, segment count, packet count/length and
  sample count are capped; URIs escaping the manifest root are rejected.
