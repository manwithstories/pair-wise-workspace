# mediapack

A local, terminal-only media packaging pipeline.  It ingests metadata-bearing
image-frame sequences with SHA-256 checksums and multi-track audio
descriptions, validates them, parses and round-trips MP4 containers, aligns
segment boundaries across bitrate representations, and emits a reproducible
per-segment hash manifest.  Work is scheduled as a dependency graph whose
nodes are resumable by idempotency key.

No network access, no device permissions, no external services: every input
is generated locally from a seed.

## Requirements

- Go 1.23 or newer (the toolchain on the build machine may be newer)
- macOS Terminal (or any POSIX shell)

## Build, test, demo

```sh
go mod tidy
go build ./...
go test ./...
```

Normal path:

```sh
go run ./cmd/mediapack demo --seed 41 --rates 800000,1600000 --segment-ticks 90000
```

Failure and resume path:

```sh
go run ./cmd/mediapack demo --seed 41 --rates 800000,1600000 \
  --segment-ticks 90000 --inject corrupt-frame --resume
```

Expected normal output includes:

```
status=ok
roundtrip_match_pct=100.00
keyframe_hit_pct=100.00
segments=3
segment_sha256=<64 lowercase hex characters>
```

The corrupted run prints `status=blocked` and `first_blocked=frame-2`, and
exits non-zero.  The resume run prints `resumed_nodes=N` and produces the same
`segment_sha256` values as an uninterrupted run.

> `go run` itself reports exit status 1 whenever the program exits non-zero.
> To observe the documented codes directly, build once and invoke the binary:
>
> ```sh
> go build -o /tmp/mediapack ./cmd/mediapack
> /tmp/mediapack demo --seed 41 ...     # 0 on success
> /tmp/mediapack demo --inject corrupt-frame ...  # 2 on detected media fault
> /tmp/mediapack demo --bogus           # 1 on command-line error
> ```

## Exit codes

| Code | Meaning |
| ---- | ------- |
| 0 | Success |
| 1 | Command-line or configuration error |
| 2 | Detected input, limit, or dependency failure |

## Commands and flags

```
mediapack demo [flags]
mediapack help
```

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--seed N` | 41 | Deterministic fixture seed |
| `--rates a,b` | 800000,1600000 | Bitrate representations (1–8, unique) |
| `--segment-ticks N` | 90000 | Segment length in programme ticks (90 kHz) |
| `--frames N` | 90 | Frames per representation |
| `--inject NAME` | none | Deterministic fault injection |
| `--resume` | false | Reuse durable state; only unfinished nodes run |
| `--workdir DIR` | .mediapack/demo | Fixtures, artifacts and state |
| `--audio-policy P` | block | `block`, `prefer:<source>`, `downmix:<layout>` |
| `--color-policy P` | block | `block`, `prefer:<source>` |

Injections: `corrupt-frame`, `duplicate-frame`, `missing-frame`,
`truncated-mp4`, `illegal-box`, `audio-conflict`, `color-conflict`,
`misaligned-rate`, `unknown-box`.

## Architecture

```
cmd/mediapack/          terminal entry point, DAG wiring, report
internal/clock          exact rational ticks; rejects overflow and inexact conversion
internal/fault          attributable errors (track, frame, segment, first blocked dependency)
internal/frames         frame manifests, SHA-256, RTP timestamp agreement
internal/audio          channel layouts, sample rates, explicit mapping policies
internal/color          CICP colour normalisation, sourced conflicts
internal/mp4            validated box tree, reserialisation, round-trip comparison
internal/index          keyframe table on the unified tick base
internal/segments       boundary alignment across representations
internal/dag            idempotent keys, durable state, retry, blocked chains
internal/hash           per-segment digests and reviewable manifest
internal/fixtures       seed-deterministic sample corpus and fault injection
```

### Data flow

Frames and track descriptions enter `frames`, `audio` and `color`, all on the
unified `clock` base and reporting through `fault`.  Validated MP4s are parsed
by `mp4`, indexed by `index`, aligned by `segments`, and hashed by `hash`.
`dag` schedules these stages with keys derived from input hashes and
configuration, persists state to `--workdir/state.json` after every
transition, and names the first blocking dependency on failure.  Each node
also persists its output as an artifact under `--workdir/artifacts/`, so a
resumed run can rebuild the final report without re-executing skipped nodes.

### Determinism and resume

- Fixtures are byte-identical for a given seed, rate list and injection.
- Segment boundaries are exact rationals; a conversion that would round is
  rejected rather than approximated.
- Node keys cover ordered input hashes and the configuration hash.  Re-running
  with identical inputs skips every node and reproduces the manifest hashes.
- A bad segment never enters the success manifest; the manifest is marked
  `blocked` and the failing state is persisted.
- `--resume` with a fresh work directory first completes the stages through
  `segments` (an interrupted run), then resumes; the final hashes are compared
  against a clean run before the process reports success.

## Limits

Frame count, track count, box count, nesting depth, segment count and DAG node
count are all bounded; inputs above a limit are rejected with exit code 2.
