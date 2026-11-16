# audio-tool — offline WAV sample-rate and programme-loudness normalizer

A pure-local command-line tool for consolidating game-audio delivery
material: it resamples WAV/FLAC to one target rate, normalizes programme
loudness to a target LUFS under a true-peak ceiling, and writes a reproducible
JSON report of everything it did.

It exists because hand-processing a delivery set is not reproducible or
auditable: rates differ (44.1/48/96 kHz), layouts run from mono to 7.1, tails
do not line up, and source material is sometimes already clipped. After a run
you should be able to answer "what exactly happened to which file, and can I
prove it later" from the report alone.

## Requirements

- Python 3.11+
- `soundfile` 0.12.1 (WAV/FLAC I/O)
- No numpy, no ffmpeg, no network, no third-party resampler.

## Install / run

The package lives in `src/audio_tool`; a thin top-level package points
`__path__` at it, so both of these work from a clean checkout:

```bash
python -m audio_tool.cli --help
pip install -e .        # optional, gives you the `audio-tool` script
```

## Acceptance demo

```bash
python -m tests.generate_fixtures --out fixtures
python -m audio_tool.cli batch --input fixtures --output out \
    --sample-rate 48000 --target-lufs -16 --true-peak -1 \
    --report reports/run.json
python -m audio_tool.cli inspect --report reports/run.json
```

The demo produces 7 valid WAVs and 7 matching report entries; every file lands
within 0.001 dB of −16 LUFS. Deliberately invalid fixtures are written to
`fixtures_broken/` so they cannot make the demo report a failure. Feed them in
explicitly to watch per-file error isolation:

```bash
python -m audio_tool.cli batch --input fixtures_broken --output out_broken \
    --report reports/broken.json --quiet
echo $?    # 1 — every file failed, each for its own documented reason
```

## Commands

### `batch`

| Flag | Meaning |
|---|---|
| `inputs...` | Files or directories; directories are scanned recursively for `.wav`/`.flac` |
| `--manifest PATH` | Newline-separated input list; paths resolve relative to the manifest |
| `--input DIR` | Extra input directory (repeatable) |
| `--output DIR` | Output root (required); relative sub-paths are mirrored |
| `--sample-rate N` | Target rate, 8000‥192000 Hz (default 48000) |
| `--target-lufs F` | Programme loudness target (default −16) |
| `--true-peak F` | True-peak ceiling in dBTP, −20‥0 (default −1) |
| `--block-frames N` | Frames per processing block (default 16384) |
| `--resample-passband F` | Low-pass cutoff in Hz (default: 92% of the lower Nyquist) |
| `--resample-taps N` | One-sided window length (default 48; higher = sharper, slower) |
| `--resample-phases N` | Polyphase resolution (default 256) |
| `--subtype X` | `PCM_16`, `PCM_24` (default), `PCM_32`, `FLOAT` |
| `--jobs N` | Worker threads sharing one kernel per input rate |
| `--report PATH` | Write the JSON report (atomic) |
| `--workspace DIR` | Confinement root; every path must resolve inside it |
| `--no-fingerprint` | Skip SHA-256 input hashes (faster) |
| `--dry-run` | Print the plan without writing anything |

### `inspect` / `verify`

`inspect --report R` prints the summary table. `inspect --report R --verify`
(or `verify --report R`) re-derives each file's arithmetic from the stored
statistics and exits 1 on any inconsistency: gain above request, before+gain
disagreeing with measured after, negative counters, true peak over ceiling, a
missing output. `--json` emits the machine-readable form.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Every input processed |
| 1 | At least one input failed (bad format, unsupported layout/rate, NaN, truncation) |
| 2 | Invalid parameters, or the input set is unavailable |
| 3 | An output or the report could not be written |

## How it works

Pipeline: `io` → `resampler` → `loudness` → `normalize`, scheduled by `batch`,
reported by `report`, driven by `cli`. Nothing below `cli` imports it.

**`model.py`** — typed vocabulary: exit-code-mapped errors, the 1–8 channel
layout table with BS.1770 weights (LFE at 0, surround pairs at 1.41),
validated config, plane-major `AudioBlock`, `LoudnessStats`.

**`container.py` / `io.py`** — validation before any DSP. The RIFF chunk walk
is there because libsndfile silently clamps a truncated data chunk to whatever
bytes exist, so a half-copied file would otherwise read as healthy. Blocks are
plane-major and block-sized; the final tail is zero-filled and the padded frame
count is reported. Writes stage to a sibling temp file and land via
`os.replace`.

**`resampler.py`** — rational polyphase windowed-sinc kernel. Two details a
naive version gets wrong: the window length is measured in periods of the
*slower* rate (so heavy decimation gets a narrow transition band rather than a
filter nearly an octave wide), and outputs are only emitted once their whole
tap window holds real samples (so block boundaries cannot corrupt the result
and block size does not change the output at all). Identity rate is a bit-exact
passthrough.

**`kweight.py` / `truepeak.py` / `loudness.py`** — BS.1770-4. K-weighting is
derived analytically from the recommendation's analog parameters rather than
from a 48 kHz table, so it is correct at any rate; at 48 kHz it reproduces the
ITU coefficients exactly. Gating is 400 ms blocks on a 100 ms grid, −70 LUFS
absolute then 10 LU relative. True peak is 4× oversampled with unit-gain
polyphase banks and deferred interpolation.

**`normalize.py`** — two passes over a resampled intermediate, because gain
depends on whole-file gated loudness. True-peak protection *reduces gain*
rather than clamping samples, so a protected file stays undistorted. Clipping
is reported as three separate counters: `original_clipped`, `output_clipped`,
`added_clipped`.

**Silent material** passes through at unity and reports `status: "silent"`
with `lufs: null`, `digital_silence: true`, and finite dB fields (the floor is
−240 dB, not −inf). It is a normal delivery case, not an error.

## Report shape

```jsonc
{
  "schema": "audio-tool/report/1",
  "algorithm_version": "bs1770-4/kweight-biquad+polyphase-sinc/v1",
  "config": { "target_rate": 48000, "target_lufs": -16.0, "true_peak_db": -1.0,
              "resampler": { "passband_hz": null, "half_taps": 48, "phases": 256 } },
  "summary": { "succeeded": 7, "failed": 0, "output_sample_rate": 48000,
               "before_rms": …, "after_rms": …, "before_lufs": …, "after_lufs": …,
               "original_clipped": …, "output_clipped": …, "added_clipped": …,
               "zero_padded_frames": … },
  "files": [ { "source": "…", "output": "…", "input_sha256": "…", "ok": true,
               "stats": { "status": "ok", "gain_db": …, "before": {…}, "after": {…} } } ]
}
```

## Tests

```bash
python -m pytest -q
```

112 tests covering kernel accuracy and alias rejection, ITU coefficient match,
reference-tone loudness, gating, true-peak semantics, format validation,
truncation detection, atomic-write behaviour, failure isolation, threading,
exit codes and offline verification.

## Constraints honoured

- Pure local; no network, no numpy, no ffmpeg, no third-party resampler.
- 16/24/32-bit PCM plus float/double ingest; 32-bit float intermediate.
- Block processing; a 49 MB 96 kHz 5.1 file peaks around 40 MB RSS.
- Sources are never overwritten; paths are confined to `--workspace`.
- No secrets, connection strings or build artifacts are written.

## Known limits

- Throughput is roughly 0.5–1× realtime for 96 kHz multichannel input in pure
  Python. `--resample-taps` trades sharpness for speed; `--jobs` parallelizes
  across files.
- Unlisted channel counts above 8 are rejected rather than guessed.
- Loudness for material shorter than one 400 ms block uses the whole-file
  K-weighted mean square, which is close but not identical to a gated reading.
