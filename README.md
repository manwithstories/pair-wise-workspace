# mediad

`mediad` reconstructs multi-track program timelines after cross-node transfer,
generates offscreen verification frames with SHA-256 provenance, and writes a
manifest that explains every decision. It is a pure Go, terminal-only tool: no
GUI, browser, camera, microphone, network service, external media file or
encrypted content is involved.

## Requirements

- Go 1.23+
- macOS Terminal (any POSIX shell works)

## Quick start

```sh
go mod tidy
go build ./...
go test ./...

go run ./cmd/mediad demo --seed 42 --bandwidth-kbps 1800 \
  --loss-burst 4 --retry 2 --out ./tmp/demo
```

The success path prints one field per line:

```
target_kbps=1758.33
actual_kbps=1811.57
bitrate_error_pct=2.94
frame_count=30
psnr_mean_db=52.60
ssim_mean=0.9999
manifest_drift_ms=0
duplicate_packets=0
reordered_packets=0
lost_packets=0
blocked_node=none
```

Corrupted metadata must exit non-zero and name the failure:

```sh
go build -o ./tmp/bin/mediad ./cmd/mediad
./tmp/bin/mediad demo --seed 42 --corrupt-metadata --out ./tmp/bad
# stderr: category=metadata track=v0 blocked_node=metadata:v0
# exit:   2
```

`scripts/acceptance.sh` runs both and checks every threshold (bitrate error
<= 8 %, PSNR >= 30 dB, SSIM >= 0.90, drift <= 1 ms, exit codes 0/2).

## Verifying a run

```sh
mediad verify-manifest ./tmp/demo/manifest.json
# verified_artifacts=30
# manifest_sha256=<digest>
```

The command re-hashes every frame artifact and checks the manifest body against
its recorded self-hash. A manifest cannot contain the digest of its own final
bytes, so `manifest.json` appears in `artifacts` with the digest of the canonical
body (all other artifacts plus the manifest content); `VerifySelfHash`
reproduces it exactly and reports a missing or mismatched entry as an error.

## Exit codes

| code | meaning |
|------|---------|
| 0 | success |
| 1 | usage error (bad or missing flags) |
| 2 | data/processing error (metadata, transport, timeline, render, manifest) |

Note: `go run` collapses a child exit status of 2 into 1. The CLI also prints
`exit_code=2` so the contract is visible either way; use the built binary when
the shell exit code matters.

## Scenarios

`--scenario` selects a generated packet fixture:

| scenario | exercises |
|----------|-----------|
| `success` | clean stream |
| `wrap` | 16-bit RTP sequence wrap |
| `duplicate` | repeated packets |
| `reorder` | out-of-order arrival inside the reorder window |
| `burst-loss` | contiguous burst loss above `--loss-burst` |
| `interrupt` | clean stream; pair with `--resume-from` |
| `corrupt` | metadata corruption (used with `--corrupt-metadata`) |

`--resume-from <manifest.json>` resumes a run: completed DAG nodes replay their
pure product and must reproduce their recorded hash; everything else continues
in dependency order.

## Architecture

| package | responsibility |
|---------|----------------|
| `internal/clock` | one rational media timebase; integer-only timestamps |
| `internal/model` | shared error model (track, packet seq, interval, first blocker) |
| `internal/metadata` | MP4 (go-mp4) and a minimal MXF metadata sample; rotation, crop, color provenance |
| `internal/transport` | RTP wrap, dedupe, reorder, explicit gaps, sliding-window bandwidth |
| `internal/timeline` | deterministic frame grid, overlap order, holes, boundary rule, gap blocking |
| `internal/render` | offscreen procedural frames, crop/rotate/color, SHA-256, PSNR/SSIM |
| `internal/scheduler` | dependency DAG, idempotent retries, branch isolation, resume |
| `internal/manifest` | JSON manifest, statistics, gap->frame linkage, stability comparison |
| `internal/fixtures` | generated metadata and packet fixtures for every scenario |
| `internal/pipeline` | end-to-end wiring used by the CLI and its tests |
| `cmd/mediad` | flag parsing, terminal output, exit codes |

Data flow: packets -> transport -> scheduler; metadata + timebase -> timeline;
timeline frames -> render + manifest. All three share the timebase and error
model.

## Rules that are fixed by design

- **Rotation** is normalized to the nearest right angle in {0, 90, 180, 270};
  the raw angle is kept in the manifest.
- **Crop** is validated against the coded frame; an out-of-frame clean aperture
  is a track-scoped, interval-scoped error, and the affected track's frames are
  blocked rather than silently clipped.
- **Color** conflicts resolve by an explicit priority (explicit `colr`/MXF
  description > sample entry > recorded default), ties break on track key, and
  every non-chosen description appears in `color.conflicts`. A partial
  description is reported, never silently replaced.
- **Timeline frames** are half-open `[k*T,(k+1)*T)`. A source sample is chosen
  by the frame's start tick; a sample shorter than a frame is a boundary sample
  and is retained. Layers paint in track-key order. A hole at or above the
  configured threshold blocks the frame.
- **Late packets** are refused below the delivery watermark; they can never
  overwrite a committed interval.
- **Burst gaps** at or above the threshold become explicit gaps, are linked to
  the frames they block in `gaps[].frames`, and retain the first blocking
  dependency.
- **Quality metrics** compare the production render with an independent
  inverse-mapped reference (same source, exact color path). Blocked frames are
  excluded; if every frame is blocked, the score is zero rather than a vacuous
  pass.

## Outputs

```
<out>/
  input/sample.mp4      generated MP4 fixture
  input/sample.mxf      generated MXF metadata sample
  frames/frame_NNNN.ppm offscreen frames (binary PPM, no codecs)
  manifest.json         tracks, color decision, frames, gaps, bandwidth,
                        transport stats, DAG nodes, failures, metrics,
                        artifact hashes, stability comparison
```

The output directory is disposable and is rebuilt on every run. Keep it out of
version control (`tmp/` is ignored).

## Tests

```sh
go test ./...
```

Each subsystem has focused tests; `internal/pipeline` and `cmd/mediad` cover
the end-to-end acceptance contract, reproducibility, corrupt-input isolation,
all packet scenarios, gap/frame linkage, resume, and exit codes.
