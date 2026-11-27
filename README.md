# fieldcut

Three-camera field-frame synthesis for a single 25 fps delivery timeline.
The tool validates frame manifests, unwraps 32-bit device ticks, fits clock
drift, plans a track dependency DAG, probes 48 kHz calibration audio, fills
bounded frame holes, composites and exports deterministically, then proves
the result by independent recomputation.

Terminal only. Every behaviour is reachable through flags.

## Build

```sh
go version                                   # go1.25.6 required by go.mod
go mod tidy && go mod vendor
go build ./...
go test ./...
```

Dependencies are pinned by `go.mod`, `go.sum` and `vendor/`
(`github.com/disintegration/imaging v1.6.2` and its transitive
`golang.org/x/image`). There is no network or external service dependency.

## Demo

```sh
go run ./cmd/fieldcut demo \
  --seed 41 --frames 240 --tracks 3 --wrap-at 4294967200 \
  --drift-ppm 120 --drop-every 37 --corrupt-every 53 \
  --out /tmp/fieldcut-demo

go run ./cmd/fieldcut verify --dir /tmp/fieldcut-demo
```

The demo generates its own dataset under `OUT/input` (frames, `audio.pcm`,
`manifest.json`). Frame content is deterministic from the seed, embeds a
per-camera marker, and includes:

- a real 32-bit tick wrap mid-sequence,
- 120 ppm device drift between tick and wall time,
- variable frame intervals that map onto the 25 fps grid,
- injected recovery holes, one adjacent corrupt frame and one long
  unrecoverable gap,
- a 48 kHz calibration pulse train whose first pulse is 1 ms late.

It then runs the pipeline three ways: an interrupted run (limit = 25% of
frames), a resume from that directory, and a clean full run into
`OUT/export-full`. `resume_equal` compares the two root hashes. Finally it
runs the rejection self-check, printing one `reject_check=` line per exit
code contract.

Reported line:

```
sync_max_ms=…  recovery_rate=…  eligible=…  recovered=…  p95_frame_ms=…
frames=…  root_sha256=<64 hex>  resume_equal=true  verify=PASS
```

`verify` recomposes every frame from the generated input, hashes the PNG
bytes on disk and compares against `export.json`; it never trusts the
recorded root alone.

## Exit codes

| code | meaning |
| ---- | ------- |
| 0 | verification passed |
| 2 | input rejected (manifest, path, size, hash, corrupt frame, conflict, limit) |
| 3 | dependency graph or time base invalid |
| 4 | metrics outside `sync_max_ms <= 2.0`, `recovery_rate >= 0.95`, `p95_frame_ms <= 100`, or resume/verify mismatch |
| 1 | uncategorised |

## Layout

| package | role |
| ------- | ---- |
| `internal/media` | shared rational time base, limits, single `MediaError` model |
| `internal/ingest` | manifest validation, hashing, decode classification, path safety |
| `internal/clock` | 32-bit half-range unwrap, exact rational drift fit |
| `internal/timeline` | DAG with masks, priority, offsets; cycles fail before output |
| `internal/audio` | 48 kHz PCM parsing and calibration pulse alignment |
| `internal/recovery` | bounded interpolation vs explicit missing records |
| `internal/render` | deterministic compositing, atomic export, resume checkpoints |
| `internal/runner` | bounded pipeline, verification, resume orchestration |
| `internal/metrics` | metric bounds and terminal summary |
| `internal/gen` | deterministic demo dataset and mutation hooks |
| `cmd/fieldcut` | `demo`, `verify`, `version` |

## Guarantees enforced

- Sizes outside `4096×2160` or `3` tracks are rejected before work starts.
- Manifest paths may not be absolute, contain `..`, or use a drive/scheme.
- A raw tick step at or beyond half the counter range is ambiguous and is
  reported with track and sequence.
- Frames are mapped by exact rational arithmetic with a left-closed,
  right-open 25 fps rule; no float accumulation.
- Only holes with valid flanks at most two frames apart are interpolated.
  Everything else is an explicit missing record; corrupt repairs are
  reported separately and never inflate `recovery_rate`.
- Output directories are only written if empty or owned by this program;
  frames are committed via temp file + rename, and a chunked checkpoint
  (`progress.json`) makes an interrupted run resumable to the same root
  hash as a full run.
- A configuration or source hash change invalidates the cache by clearing
  only program-owned artifacts.

## Sample output

```
$ go run ./cmd/fieldcut demo --seed 41 --frames 240 --tracks 3 \
    --wrap-at 4294967200 --drift-ppm 120 --drop-every 37 --corrupt-every 53 \
    --out /tmp/fieldcut-demo
generated frames=240 tracks=3 wrap_at=4294967200 drift_ppm=120 corrupt=13 dropped=21
interrupted after 60 frames; resuming
partial frames=240 p95_frame_ms=24
sync_max_ms=1.0 recovery_rate=1.00 eligible=51 recovered=51 p95_frame_ms=24 frames=240 root_sha256=53d80a579c… resume_equal=true verify=PASS
```

`p95_frame_ms` is wall-clock composition latency per frame and varies with
the machine; the root hash does not.
