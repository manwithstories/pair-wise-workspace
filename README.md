# mediasync

Deterministic multi-track A/V ingestion, recovery, synchronization and
manifest reporting for raw elementary streams, built for the macOS terminal.

A remote production team glues together studio interviews, exterior cameras
and a remote commentator. The media engineer checks sync and quality, the
transport engineer traces loss, and the pipeline maintainer needs to know
which stretches are damaged and to regenerate the same answer twice.

## Quick start

```sh
brew install ffmpeg libsndfile
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
.venv/bin/python -m mediasync.cli demo --seed 17 --loss 0.08 --jitter-ms 45 \
    --target-bps 4000000 --out /tmp/mediasync.json
.venv/bin/python -m mediasync.cli verify /tmp/mediasync.json
.venv/bin/python -m pytest -q
```

`demo` prints the contract summary and writes the JSON manifest:

```
target_bps=4000000 actual_bps=4001208 deviation_pct=+0.030
video_frames=300
audio_peak=0.259125 audio_rms=0.126885
loss=31 recovered=31 unrecoverable=0
loss/recovered/unrecoverable=31/31/0
manifest_max_error_ms=0.089
status=PASS
```

`verify` re-checks the file from scratch instead of trusting its `status`.

Exit codes: `0` pass, `2` invalid input, parameters or manifest, `3` an
unrecoverable interval is present, `4` a manifest or synchronization gate
failed.

## Pipeline

```
fixtures ─▶ ingest ─▶ jitter ─▶ clock ─▶ timeline ─▶ recover ─▶ sync ─▶ report
```

| module | responsibility |
| --- | --- |
| `clock.py` | 90 kHz ticks, 33-bit wrap expansion, drift fit, restarts, display track |
| `errors.py` | shared error model and the exit-code contract |
| `ingest.py` | bounded raw H.264/HEVC/AV1 loading, codec sniffing, index validation |
| `jitter.py` | extend-16-bit sequence, de-dup, reorder, wait budget, late packets |
| `timeline.py` | explicit half-open intervals, dependency graph, cycle/overlap checks |
| `recover.py` | seeded retransmission, boundary repair, mandatory gap marking |
| `sync.py` | anchor alignment on the 90 kHz grid, drift and VFR diagnostics |
| `report.py` | bitrate, delivery, audio levels, manifest and verification gates |
| `fixtures.py` | code-generated streams and audio; no external media |
| `pipeline.py` | end-to-end orchestration, closed-loop bitrate calibration |
| `cli.py` | `demo`, `verify`, `inspect` and exit codes |

### Design decisions worth knowing

* **Bitrate is measured, not requested.** Encoders miss CBR by different
  margins, so the pipeline encodes, measures bytes, and re-encodes in a
  bounded closed loop. The manifest records the measured rate. A run whose
  encoded program misses the target by more than 5 % fails the gate, so a
  bad calibration can never look like a pass.
* **Transport damage is reported separately from encoding.** Lost bytes are
  counted in `transport.lost_bytes`; loss does not silently distort the
  bitrate gate.
* **Nothing is silently spliced.** A hole either has its exact bytes restored
  by a seeded retransmission (recorded one attempt at a time) or it stays an
  explicit gap in the manifest. Dependents of an incomplete interval are
  downgraded to incomplete too.
* **The seed decides, order does not.** Every random draw is
  `blake2b(seed, track, purpose, index)`. Reordering tracks or packets cannot
  change the outcome, which is what makes reruns byte-identical.
* **Time keeps its provenance.** Raw timestamps are preserved; wraps expand to
  a monotonic epoch and restarts shift the display track forward while the
  original ticks remain available for inspection.

### Bounds

`Limits` caps stream size, index size, index entries, track count, audio
duration and channel count. Indexes are parsed from a bounded file and every
entry is checked against the on-disk stream size; no stage reads a whole media
file into memory.

## Tests

`pytest -q` generates its own fixtures for every scenario, including the three
raw codecs, packet loss with and without retransmission, clock wrap, encoder
restart, variable frame rate, truncated streams and a dependency cycle.
