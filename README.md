# mediacore

Local-only, deterministic toolkit for recovering presentation timelines from
raw H.264/H.265/AV1 bitstreams, validating keyframe indexes, measuring PCM
audio, and checkpointing a bounded-memory A/V pipeline so an interrupted run can
be resumed and traced.

Everything is generated locally: fixtures are procedural, encoders run through
PyAV, and no network access, device permission or GUI dependency is involved.

## Quick start (macOS)

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
.venv/bin/python -m mediacore.cli demo --codec h264 --duration 12 \
    --bitrate 900000 --seed 41 --out .tmp/demo
.venv/bin/python -m mediacore.cli verify \
    --manifest .tmp/demo/manifest.json --index .tmp/demo/index.json
.venv/bin/python -m pytest -q
```

The demo prints `target_bps`/`actual_bps`/`deviation_pct`,
`video_psnr_db`/`audio_rms` and
`manifest_alignment_max_ms`/`random_access_max_ms`, plus segment count, issue
count and the manifest digest. `--json` emits the same data as JSON.

Exit codes: `0` verified, `2` bad arguments/input, incompatible checkpoint or a
corrupt recording, `3` recoverable interruption (re-run the same command to
resume), `4` a verification metric is outside its documented limit.

Limits used by `verify` and by `demo` before it exits `0`: overall bitrate
deviation ≤ 15 %, A/V alignment ≤ 40 ms, random-access error ≤ 40 ms, and
finite, positive quality statistics.

## Commands

| command | purpose |
| --- | --- |
| `demo` | generate fixtures, run every stage, write `manifest.json` + `index.json` |
| `resume` | same as `demo` (kept for scripts that resume explicitly) |
| `scan` | scan one raw stream and print every defect with its byte offset |
| `verify` | re-read a manifest, re-derive its digest, check limits and the supplied index |

Useful demo flags: `--corrupt-tail` (cut the stream at a structurally detectable
point), `--interrupt-after N` (exit `3` after N committed stages), `--index FILE`
(validate an untrusted keyframe index), `--quiet`, `--json`.

## Architecture

```
src/mediacore/
  clock.py       rational time base, wrap unwrap, reorder/duplicate/gap repair,
                 A/V origin alignment; every stamp keeps raw + fixed + reason
  bitstream.py   chunk-independent streaming scanners for Annex-B (H.264/H.265)
                 and AV1 OBU; reports truncation and garbage offsets
  index.py       validates untrusted keyframe indexes against scanned units,
                 rebuilds random-access points, verifies seek targets
  audio.py       block-streamed WAV measurement (peak/RMS/silence) + alignment
  decode.py      single-AU keyframe decode, strict tail probe, PSNR scoring
  queues.py      bounded queues, byte budget, LRU frame/audio caches
  checkpoint.py  atomic checkpoints: input/config fingerprints, stage commits
  manifest.py    segment records, digests, verification
  pipeline.py    wires the stages, keeps memory bounded, commits checkpoints
  cli.py         argument parsing, exit codes, terminal output
```

### Memory model

* The reader fills a bounded queue of raw chunks; a slow consumer blocks it.
* Access-unit payloads are dropped as soon as they are not needed for the next
  stage (only the quality window and the tail probe window stay resident) and
  are re-read from the file by exact byte range when required.
* Audio blocks travel through a bounded queue of at most
  `QUEUE_MAX_AUDIO_BLOCKS` blocks.
* Quality scoring decodes through a byte-limited LRU frame cache.
* The manifest reports these high-water marks, so a run proves it stayed inside
  its budget instead of merely happening to fit.

### Recovery and provenance

A checkpoint stores the input fingerprint (size + BLAKE2b), the configuration
digest and each committed stage boundary with a digest of its output. A stage is
reused only when the fingerprint and digest still match; a changed input or
parameter raises a checkpoint conflict naming the field, and half-written
checkpoint files are ignored rather than treated as commits. Re-running the same
inputs produces a byte-identical manifest digest, and a resumed run matches a
fresh one even though it records `resumed: true` and its run counter differs
(those bookkeeping fields are excluded from the digest; media data is not).

### Corrupt input

`--corrupt-tail` cuts the file at a point that is structurally detectable (mid
ANNEX-B start code, or inside an AV1 OBU payload whose length field proves the
shortfall). The scanner reports the offset, the tail probe confirms the final
access unit cannot be decoded strictly, the manifest records both, and the CLI
exits `2` instead of silently accepting the recording.
