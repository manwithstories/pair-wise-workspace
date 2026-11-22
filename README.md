# mediacheck

A local, terminal-only toolkit for triaging recorded MP4/MOV media before it is
used as the source of an interactive replay session. It answers three
operational questions without decoding or re-encoding anything:

- **Which representation layers are still usable?** — structure and sample
  index coverage per track, keyframe spacing, and per-segment byte accounting.
- **Where does the bitstream break?** — the first unreadable offset, the
  damaged sample range, whether recovery resumed, and what is unrecoverable.
- **Did boundary recovery drift the manifest clock?** — a shared-timebase
  cross-layer check plus per-segment manifest alignment and bitrate deviation.

All inputs are produced by the tool itself; nothing external is downloaded.

## Requirements

- Go 1.23 or newer on macOS (or any Linux/Windows terminal).
- Dependencies are pinned in `go.mod`: `github.com/abema/go-mp4 v1.3.0`,
  `github.com/pion/rtp v1.8.11`.

## Quick start

Run these from the project directory:

```sh
go mod tidy
go build ./...
go test ./...

# Synthesize valid, truncated, corrupt, missing-index and boundary-mismatch
# fixtures under ./tmp-fixtures.
go run ./cmd/mediacheck generate --dir ./tmp-fixtures

# Healthy input: alignment must be at most 1 ms, exit code 0.
go run ./cmd/mediacheck analyze \
  --input ./tmp-fixtures/valid.mp4 \
  --ladder 300000,600000 --segment 2s

# Tail-truncated input: recover the last complete sample, report the damage
# offset and recovered count, exit code 2.
go run ./cmd/mediacheck analyze \
  --input ./tmp-fixtures/truncated.mp4 \
  --ladder 300000,600000 --segment 2s --recover
```

Add `--json` for the machine-readable summary, which additionally contains
per-segment bitrate statistics and classified issues.

## Exit codes

| Code | Meaning                                                          |
| ---- | ---------------------------------------------------------------- |
| 0    | Complete success; no damage affected the analysis.               |
| 2    | Damage was detected, recovery completed, summary is available.   |
| 1    | Invalid arguments, unreadable input, or nothing could be used.   |

## What each stage does

`internal/clock` owns the single timebase. Track ticks convert to the shared
millisecond timeline with round-half-up and saturation, so damaged durations
cannot wrap into negative time. RTP timestamps use the same rule modulo 2^32.

`internal/probe` walks top-level boxes without loading `mdat`, parses the
`moov` sample tables in memory (capped by `--max-moov`), rebuilds chunk
offsets from `mdat` bounds when `stco`/`co64` is missing, and iterates samples
through a bounded cursor. Every offset and length is validated before a read.

`internal/repair` scans each track independently with a fixed read buffer.
A sample is accepted only if it lies inside the file and inside an `mdat`
payload and is not a uniform fill (0x00/0xff). On damage the scan stops,
records the range, and resumes at the next verifiable sync boundary; recovered
timestamps never regress. Cross-layer boundary disagreement beyond the
tolerance rejects the switch and lists the difference. A track that recovers
nothing is reported `unrecoverable` while the other layers are preserved.

`internal/fanout` fans the recovered timeline out to each ladder rung and fans
the rungs back into one manifest. Bitrate is measured from recovered sample
bytes per shared segment, so the delta reflects what survived recovery.

`internal/report` renders a stable text or JSON document. Output is
deterministic: no maps or timestamps, and identical input yields identical
bytes.

## Performance and safety

- The whole file is never read into memory. `mdat` is only ever touched for
  sample-sized reads through a reusable buffer; a >1 GiB input is covered by a
  test that asserts bounded heap growth.
- Box sizes, offsets, sample counts and ladder lengths are capped; oversized
  or negative values are rejected before allocation.
- At least four fault classes are exercised end to end: tail truncation,
  corrupt samples, missing index, and cross-layer boundary mismatch.

## Layout

```
cmd/mediacheck        CLI (generate, analyze)
internal/clock        shared timebase, rounding, RTP timestamps
internal/fault        shared fault codes and severities
internal/probe        box walker, sample tables, timeline cursor
internal/repair       independent recovery, boundary cross-check
internal/fanout       bitrate ladder, aligned manifest, RTP markers
internal/fixture      deterministic MP4/MOV generator
internal/report       text and JSON summaries
internal/integration  independent re-verification of the pipeline
```
