# mediad — resilient RTP/LPCM media handoff pipeline

`mediad` reads locally generated or supplied RTP/RTCP capture streams in a
length-prefixed, arrival-timestamped, CRC-protected container; locates damaged
bytes; validates/rebuilds a sparse index; reorders and conceals lost packets;
decodes L16; converts mono/stereo/5.1 to 48 kHz stereo; performs BS.1770-style
gated loudness and true-peak control; and commits deterministic fragmented MP4
output through checksummed WAL/checkpoint boundaries.

## Acceptance run

```bash
go mod download
go build ./...
go test ./...
go run ./cmd/mediad demo --seed 29 --seconds 12 --out "$TMPDIR/mediad-case"
shasum -a 256 "$TMPDIR/mediad-case/final.mp4"
```

The demo creates:

- `normal/` — clean 5.1, 44.1 kHz capture and MP4 output;
- `corrupt/` — bad CRC, one-bit damage, truncated tail and out-of-range index;
- `interrupted/` — an MP4-stage injected exit-75 failure, WAL recovery and
  resumed output;
- `final.mp4` — byte-identical recovered output at the requested root path.

It prints `damage_offset`, `damage_offset_error_bytes` (zero for generated
one-bit fixtures; acceptance bound `<= 8`), `records`, `lost`, `reordered`,
`duplicates`, `samples=576000`, `rollback=1`, `resume_ms`, and
`sha256=<64 hex>`. Re-running the same demo path produces the same SHA-256.

`scripts/verify.sh` runs the acceptance commands and checks repeatability.
`scripts/check-exit-codes.sh` builds a binary and verifies exits `2`, `3`, `75`
and successful resume.

## Commands and exit codes

Process exit codes from the compiled binary are:

| Code | Meaning |
|---:|---|
| 0 | completed |
| 2 | arguments or input-size/usage error |
| 3 | unrecoverable corruption |
| 75 | injected interruption; WAL remains resumable |

`go run` itself wraps nonzero child exits as its own status 1 and prints
`exit status N`; build `cmd/mediad` when a harness must observe the raw 2/3/75
process status.

Useful commands:

```bash
go run ./cmd/mediad generate-fixture --seed 29 --seconds 12 --out ./case
go run ./cmd/mediad scan --input ./case/input.mdr
go run ./cmd/mediad index --input ./case/input.mdr --index ./case/index.json
go run ./cmd/mediad run \
  --input ./case/input.mdr \
  --index ./case/index-working-bad.json \
  --out ./case/final.mp4 \
  --work ./case/work \
  --inject-stage mp4   # first run: 75
# repeat without --inject-stage: resumes from the last committed stage
```

## Capture container

Each record is:

```text
MDR1                     4 bytes magic
length                   uint32 big-endian, includes kind/arrival/packet/CRC
kind                     1 byte: 1=RTP, 2=RTCP
arrival_ns               int64/uint64 big-endian nanoseconds
RTP or RTCP packet       length - 1 - 8 - 4 bytes
CRC32-IEEE               uint32 big-endian over header and record prefix
```

The scanner preserves all complete records before a truncated tail and reports
the truncation offset. A one-bit CRC fixture is located by bounded brute-force
bit syndrome search and normally gives the exact injected byte offset.

## Audio normalization

- L16 is signed big-endian PCM.
- Input rates 44.1 and 48 kHz are accepted; output is always 48 kHz.
- Channel order for 5.1 is `L, R, C, LFE, Ls, Rs`.
- Explicit unity-row-L1 downmix matrix:

```text
Lout = 0.500 L + 0.250 C + 0.125 LFE + 0.125 Ls
Rout = 0.500 R + 0.250 C + 0.125 LFE + 0.125 Rs
Mono -> L=M, R=M
Stereo -> identity
```

Loudness uses K-weighting biquads, 400 ms blocks and 100 ms hop, an absolute
-70 LUFS gate and the relative -10 LU gate. True peak is estimated with
deterministic 4x cubic oversampling. Gain is never positive, so exact silence
and low-level ambience are not upward-amplified; missing packets are exact
zeros before DSP.

## Transactions and determinism

- `mediad.wal` is JSON-lines with per-entry CRC32.
- `checkpoint.json` is atomically replaced and SHA-256 signed.
- Stage outputs are written to temporary files and renamed only after complete
  writes; an injected failure before commit leaves an existing committed
  destination untouched.
- A torn/corrupt WAL tail is truncated to the last valid entries and recovery
  falls back to the checkpoint/latest commits. Unclosed transactions are
  counted as rollbacks.
- MP4 timestamps are fixed at zero and all generated audio is deterministic for
  a fixed seed, so repeated runs emit byte-identical MP4 files.

## Limits and tests

- Input size limit: 1 GiB.
- Resident allocation envelope: 256 MiB, with PCM buffers explicitly bounded.
- Declared frame lengths are validated before allocation and integer overflow
  is rejected.
- Generative tests cover capture/CRC/index faults, sequence wrap, loss and
  duplicates, DSP invariants, WAL corruption, CLI status codes, and end-to-end
  interruption/recovery.
