# relay — deterministic cross-node media relay

A pure-Go pipeline that takes cross-node RTP media segments plus MXF/MP4
multi-track material, puts every sample on one rational timeline, and exports
an off-screen PNG sequence with reproducible SHA-256 evidence. It is built for
delivery verification: when node clock drift, 32-bit timestamp wrap, variable
frame rate and conflicting metadata pile up, it must be possible to point at the
exact frame that went wrong and to prove that a re-run is byte-identical.

Everything is local. There are no external services, and all fixtures are
generated from a seed; nothing is read from outside the repository.

## Requirements

- Go 1.25.6 (`go.mod` pins the directive to `1.25.6`)
- macOS (Darwin) on Apple Silicon or Intel; any OS with a working Go toolchain
  also works
- Dependencies: `github.com/abema/go-mp4 v1.4.1`, `github.com/pion/rtp v1.10.6`,
  and the standard library (`image`, `image/jpeg`, `image/png`, `testing`)

```sh
go mod tidy
go build ./...
go test -race ./...
```

## Usage

```sh
# Full self-check: normal, corrupt and cancelled/resumed paths.
go run ./cmd/relay selfcheck --seed 41 --workers 4 --inflight 12 --out "$TMPDIR/relay-check"

# Two independent demo runs, compared with cmp.
go run ./cmd/relay demo --seed 41 --workers 4 --inflight 12 --out "$TMPDIR/relay-a"
go run ./cmd/relay demo --seed 41 --workers 4 --inflight 12 --out "$TMPDIR/relay-b"
cmp "$TMPDIR/relay-a/SHA256SUMS" "$TMPDIR/relay-b/SHA256SUMS"
```

Flags: `--seed`, `--workers`, `--inflight`, `--out`, `--width`, `--height`,
`--segments`, `--policy`, `--cancel-after`, `--resume`.

### Exit codes

| Code | Meaning |
| ---- | ------- |
| 0 | checks passed |
| 2 | unrecoverable input (bad container, invalid limits, foreign output dir) |
| 3 | cancelled and resumable (a `checkpoint.json` was written) |
| 4 | digest mismatch between runs |

Note that `go run` reports any non-zero exit as `1`; use a built binary
(`go build -o relay ./cmd/relay`) to observe 2/3/4 directly.

### Self-check output

```
roundtrip_rate=1.000
keyframe_hits=48/48
frames=120
segments=6
sha256_match=6/6
wraps=2
vfr_intervals=3
recovered=2/2
cancelled=1
```

`png_shard=<index>.<sha256>` lines follow, each a 64-character lowercase hex
digest of a contiguous segment of rendered frames.

## Architecture

Data flows in one direction, and every stage obeys one rule: **no stage compares
raw source timestamps.** All timing is converted into the public rational
timebase in `internal/model` first.

```
fixture ─▶ container ─▶ metadata ─▶ timeline ─▶ transport ─▶ index
                                                              │
                                             pipeline (pool + merge)
                                                              │
                                                    render ─▶ audit
```

| Package | Responsibility |
| ------- | -------------- |
| `internal/model` | Rational `Timebase`, 32-bit wrap expansion, container-neutral `Track`/`Sample`/`Description` |
| `internal/fault` | One error type carrying track, offset, kind and recoverability |
| `internal/container` | MP4 box and MXF KLV parsing with byte-exact offsets; re-mux writers for round trips |
| `internal/metadata` | Rotation/crop/colour conflict resolution by precedence, rotate-then-crop |
| `internal/timeline` | Drift fitting from sync anchors, multi-track composition on 90 kHz |
| `internal/transport` | RTP duplicate/reorder/loss/wrap recovery with a bounded window |
| `internal/index` | Keyframe index validation and scan-based rebuild |
| `internal/pipeline` | Bounded worker pool and deterministic merge with cancellation |
| `internal/render` | MJPEG decode, rotate, crop, off-screen PNG write |
| `internal/audit` | Round-trip field comparison, shard digests, SHA256SUMS |
| `internal/fixture` | Deterministic MP4/MXF/JPEG/RTP generation from a seed |
| `internal/scene` | Assembles the acceptance scenario and its corrupt variants |
| `internal/relay` | Command orchestration and reporting |
| `cmd/relay` | CLI entry point |

### Behaviour guarantees

- **Rational timebase.** Rates such as 90 kHz RTP, millisecond container time
  and 30000/1001 are exact rationals; conversion rounds half to even and checks
  for 64-bit overflow. Wrap expansion lives here so timestamps are unwrapped
  once, centrally.
- **Metadata precedence.** The caller policy wins over the bitstream sample
  description, which wins over the container description. Losers are recorded.
  Rotation is applied before cropping; a clean aperture is converted into the
  rotated orientation before centring.
- **Drift.** Node clock drift is fitted by least squares over sync anchors. A
  node above 500 ppm is marked unalignable and excluded rather than silently
  misplaced.
- **No fixed-rate padding.** Samples keep their own durations; the composition
  counts distinct intervals and reports them (`vfr_intervals`). Three distinct
  durations must survive.
- **Bounded and deterministic.** Pools, reorder windows, KLV lengths, track
  counts and image dimensions are all capped. Reordering is a pure function of
  the input, so identical seeds and parameters produce identical SHA-256.
- **Atomic output.** Frames are written by a single goroutine after workers
  decode in memory; a cancelled run leaves only complete PNGs and a checkpoint.
  A non-relay directory is never overwritten.
- **Only MJPEG decodes.** Any other codec keeps its sample hash and returns an
  explicit unsupported error.

## Determinism

Two `demo` runs with the same seed, worker count and in-flight limit produce
byte-identical `SHA256SUMS`, which is what the `cmp` acceptance step checks. The
`selfcheck` command additionally re-renders into scratch directories and compares
segment digests (exit 4 on mismatch), cancels and resumes a run to prove the
checkpoint reproduces the uninterrupted output, and confirms that transport
recovery matches the generator's ground truth.
