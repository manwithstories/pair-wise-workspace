# mediad — multicam audio delivery

Terminal tool that turns the DIT's per-camera long takes into one
editorial-ready audio delivery: 48 kHz stereo AAC at 192 kbps, normalised to
-16 LUFS with true peak at or below -1 dBTP, cut to the edit list to within one
48 kHz sample.

Every media byte is generated at run time — no external fixture files, no
external FFmpeg process. Demux, resample and encode all go through
[`go-astiav`](https://pkg.go.dev/github.com/asticode/go-astiav) and the linked
FFmpeg 9.0 libraries.

## Requirements

```
brew install go@1.25 ffmpeg pkgconf
```

Go must be **go1.25.6**. The module pins `go 1.25.6` / `toolchain go1.25.6` and
requires `github.com/asticode/go-astiav v0.43.0`.

## Build and test

```
go mod tidy
go build ./...
go test ./...

go build -o "$TMPDIR/mediad" ./cmd/mediad
```

> `go mod tidy` may drop the redundant `toolchain go1.25.6` line because it
> equals the `go` version; this is standard Go normalisation and does not
> change the toolchain actually used.

## Demo and acceptance

```
"$TMPDIR/mediad" demo --seed 41 --case clean  --out "$TMPDIR/a"
"$TMPDIR/mediad" demo --seed 41 --case clean  --out "$TMPDIR/b"
"$TMPDIR/mediad" verify "$TMPDIR/a" "$TMPDIR/b"
```

The two runs print the same `output_sha256` and `verify` reports `identical`.

Each demo prints one token line:

```
target_kbps=192 actual_kbps=... bitrate_error_pct=... integrated_lufs=-16.00
true_peak_dbtp<=-1 clipped_samples=0 drift_ppm=... late_events=...
replayed_events=... manifest_alignment_us<=21 output_sha256=...
```

Cases:

| case           | what it proves                                                    | exit |
|----------------|-------------------------------------------------------------------|------|
| `clean`        | non-zero start PTS, VFR video, mid-frame cuts, repeat hash         | 0    |
| `late-wrap`    | 33-bit wrap crossing, 180 ppm drift, one edit >2 s late            | 0    |
| `truncated`    | short final packet recorded with track/time and skipped            | 0    |
| `matrix`       | mono centred, 5.1 folded through an explicit matrix                | 0    |
| `invalid-base` | zero-denominator time base rejected before any media I/O          | 2    |
| `bad-layout`   | layout with no explicit matrix rejected as `UNSUPPORTED_LAYOUT`    | 2    |

`invalid-base` prints `error_code=INVALID_TIMEBASE`. A corrupt, unrecoverable
media error exits `3`.

## Layout

```
internal/media     rational time bases, MediaTick, classified errors
internal/manifest  JSONL manifest and edit list parsing, path containment
internal/clock     wrap unwrapping and per-camera drift fit
internal/timeline  watermark, snapshots and deterministic late replay
internal/av        libav demux, resample/mix, measurement and AAC mux
internal/fixtures  deterministic generation of every sample asset
internal/pipeline  plan, two-pass bounded render, checkpoints, atomic publish
cmd/mediad         demo / verify entry point
```

## Design notes

- **Exact time.** Raw ticks are rescaled through `math/big` into a shared
  `MediaTick` (1/48000 s), so no float rounding leaks between clock, timeline
  and audio. Cut lengths are computed exactly, making the manifest alignment
  error a pure sub-sample rounding well under 21 µs.
- **Wrap-safe clocks.** A 33-bit counter that steps forward across its period
  rolls the unwrap epoch once; small out-of-order jitter stays put. Edits
  expressed in wrapped ticks are lifted to the epoch nearest the sync anchors.
- **Drift is reported, not silently corrected.** Fits beyond ±200 ppm are
  flagged (`drift_anomaly=true`) and the applied slope is pinned to 1.
- **Late events.** Events older than 2 s behind the watermark trigger a replay
  from the nearest snapshot; the final order equals a clean sequential run, so
  repeated runs hash identically.
- **Bounded memory.** Rendering is two-pass: pass one streams each segment
  through a streaming loudness meter, pass two re-renders applying the gain
  into the encoder. Only one segment (≤5 minutes) is held at a time; a single
  listed asset may not exceed 12 hours.
- **Atomic delivery.** Output goes to a temp file, is verified and hashed, then
  renamed. An interrupted or failed run never overwrites a verified delivery,
  and a checkpoint lets the next run resume from the verified result.
- **Damage is recorded, not fatal.** A short final packet is reported with its
  camera, track and raw time (`damaged_<camera>_<track>=start-end@reason`) and
  replaced with silence.

## Constraints honoured

Pure local execution, no external services; only MOV/MP4 files listed in the
manifest; symlink escapes out of the media root are rejected; bounded queues;
build artefacts and credentials are never committed.
