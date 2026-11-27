# tsqc — MPEG-TS/RTP ingest quality checker

`tsqc` is a local, terminal-only quality checker for broadcast ingest. It
takes an RTP-carried MPEG-TS Program Stream, repairs packet loss where
possible, demultiplexes H.264 and AAC, cuts playable segments on IDR
boundaries, regenerates PAT/PMT, archives products by content digest and
resumes an interrupted batch from a checkpoint.

Everything runs locally. There is no network service, no browser and no
device permission request.

## Try it

```bash
mise install go@1.25.6
mise exec go@1.25.6 -- sh -c '
  go mod tidy &&
  go build -o /tmp/tsqc ./cmd/tsqc &&
  go test ./... &&
  go run ./cmd/tsqc demo --seed 41 --faults all \
      --state /tmp/tsqc-state --out /tmp/tsqc-out &&
  go run ./cmd/tsqc verify --dir /tmp/tsqc-out'
```

The demo prints the checks that matter in the control room:

```
status=PASS
segments=8
gaps_recovered=1 gaps_unrecoverable=1
roundtrip_consistency=100.00%
keyframe_hit_rate=100.00%
segment=00 sha256=...
```

Exit codes: `0` pass, `1` stream unplayable, `2` bad arguments or a digest /
product integrity failure.

## Layout

| Package | Responsibility |
| --- | --- |
| `internal/clock` | shared 27 MHz time base, PSI/PES unwrapping, error model |
| `internal/rtp` | bounded reordering, XOR single-loss repair, gap tracking, Pion wire format |
| `internal/ts` | 188-byte packets, PSI sections and tables, demux and PES assembly |
| `internal/pipeline` | keyframe index, IDR segmentation, re-mux, CAS, checkpoint, archive verify |
| `internal/fixture` | seeded TS/RTP sample generator and fault injection |
| `cmd/tsqc` | terminal adapter only: flags, output, exit codes |

Data flows RTP reorder/repair → TS demux and clock → keyframe index and
segmentation → content-addressed cache and checkpoint. Each subsystem has a
reusable core plus its own tests.

## Commands

```
tsqc demo   --seed N [--faults all|comma,list] --state DIR --out DIR
tsqc verify --dir DIR
```

Faults available: `duplicate`, `reorder`, `burst`, `pcrwrap`, `ccjump`,
`badpsi`, `truncate`, or `all`. The same seed always produces the same
stream, the same segments and the same per-segment SHA-256.

`demo` resumes automatically: a compatible checkpoint skips committed
fragments, a corrupt or version-mismatched checkpoint is discarded and the
work is recomputed. A cache hit is only accepted when the source identity,
input digest and the stored object's own SHA-256 all still match.
