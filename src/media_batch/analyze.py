"""Audio analysis: chunked-RMS silence detection and normalized fingerprints.

Silence is measured per fixed-size block with NumPy RMS, merged across block
boundaries; intervals shorter than 300 ms are not reported. Fingerprints are
computed on gain-normalized, silence-trimmed, resampled audio so duplicates
with different gain or edge padding still match.
"""

from __future__ import annotations

import numpy as np

SILENCE_THRESHOLD_DB = -50.0
MIN_SILENCE_MS = 300
BLOCK_MS = 20
FP_MAX_HZ = 8000        # fingerprint analyzes only 0..8 kHz (alias-free)
FP_BANDS = 512          # linear spectral bands below FP_MAX_HZ
FP_FRAME_MS = 64        # analysis window length in milliseconds


def _rms_db(block: np.ndarray) -> float:
    if block.size == 0:
        return -np.inf
    rms = float(np.sqrt(np.mean(block.astype(np.float64) ** 2)))
    return 20.0 * np.log10(rms) if rms > 0 else -np.inf


def detect_silence(samples: np.ndarray, sample_rate: int,
                   threshold_db: float = SILENCE_THRESHOLD_DB,
                   min_ms: int = MIN_SILENCE_MS,
                   block_ms: int = BLOCK_MS) -> list[tuple[int, int]]:
    """Return [(start_ms, end_ms)] silent intervals, merged across blocks.

    Intervals shorter than min_ms are dropped. Mono or (n, ch) input.
    The final partial block is clipped to the true duration, so a short tail
    is never inflated to a whole block.
    """
    if samples.ndim > 1:
        samples = samples.mean(axis=1)
    if len(samples) == 0:
        return []
    block = max(1, sample_rate * block_ms // 1000)
    silent_blocks: list[bool] = []
    for start in range(0, len(samples), block):
        silent_blocks.append(_rms_db(samples[start:start + block]) < threshold_db)
    total_ms = (len(samples) * 1000 + sample_rate // 2) // sample_rate
    intervals: list[tuple[int, int]] = []
    run_start: int | None = None
    for i, is_silent in enumerate(silent_blocks):
        if is_silent and run_start is None:
            run_start = i
        elif not is_silent and run_start is not None:
            intervals.append((run_start * block_ms, i * block_ms))
            run_start = None
    if run_start is not None:
        end = min(len(silent_blocks) * block_ms, total_ms)
        intervals.append((run_start * block_ms, end))
    return [(s, e) for s, e in intervals if e - s >= min_ms]


def merge_intervals(intervals: list[tuple[int, int]],
                    min_ms: int = MIN_SILENCE_MS) -> list[tuple[int, int]]:
    """Merge touching/overlapping intervals (e.g. split at stream block
    boundaries), then drop those shorter than min_ms."""
    merged: list[list[int]] = []
    for s, e in sorted(intervals):
        if merged and s <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], e)
        else:
            merged.append([s, e])
    return [(s, e) for s, e in merged if e - s >= min_ms]


def _trim_silence(x: np.ndarray, sample_rate: int,
                  threshold_db: float = SILENCE_THRESHOLD_DB) -> np.ndarray:
    block = max(1, sample_rate * BLOCK_MS // 1000)
    n_blocks = len(x) // block
    if n_blocks == 0:
        return x
    loud = [i for i in range(n_blocks)
            if _rms_db(x[i * block:(i + 1) * block]) >= threshold_db]
    if not loud:
        return x[:0]
    return x[loud[0] * block:(loud[-1] + 1) * block]


def _resample_linear(x: np.ndarray, src_rate: int, dst_rate: int) -> np.ndarray:
    if src_rate == dst_rate or len(x) == 0:
        return x
    n_out = max(1, int(round(len(x) * dst_rate / src_rate)))
    src_idx = np.arange(n_out) * (len(x) - 1) / max(1, n_out - 1)
    lo = src_idx.astype(np.int64)
    hi = np.minimum(lo + 1, len(x) - 1)
    frac = src_idx - lo
    return x[lo] * (1 - frac) + x[hi] * frac


def _log_band_spectrum(x: np.ndarray, sample_rate: int,
                       frame: int | None = None) -> np.ndarray:
    """Mean log-magnitude spectrum over linear bands below FP_MAX_HZ.

    Averaging short windowed frames before the FFT makes the shape depend on
    the programme's steady spectrum rather than on where a single window
    happened to fall. Subtracting the mean removes overall gain. Content above
    FP_MAX_HZ is ignored instead of resampled, which avoids aliasing adjacent
    carriers onto each other at a low analysis rate.
    """
    frame = frame or max(256, sample_rate * FP_FRAME_MS // 1000)
    if x.size < frame:
        x = np.pad(x, (0, frame - x.size))
    window = np.hanning(frame)
    frames = np.lib.stride_tricks.sliding_window_view(x, frame)[::frame]
    mags = np.abs(np.fft.rfft(frames * window, axis=1)).mean(axis=0)
    top = max(2, min(len(mags), int(round(FP_MAX_HZ * frame / sample_rate)) + 1))
    mags = mags[:top]
    edges = np.linspace(0, len(mags), FP_BANDS + 1).astype(np.int64)
    energy = np.array([
        mags[edges[i]:max(edges[i + 1], edges[i] + 1)].sum()
        for i in range(FP_BANDS)])
    # Floor at a fixed fraction of the strongest band: below that, differences
    # are FFT leakage and frame alignment noise, not programme content.
    floor = max(float(energy.max()), 1e-12) * 1e-3
    log_energy = np.log(np.maximum(energy, floor))
    return log_energy - log_energy.mean()


def fingerprint_vector(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    """Gain- and edge-silence-invariant fingerprint as a unit-norm vector.

    Mono -> RMS normalize -> trim edge silence -> mean log-band spectrum.
    Two vectors are compared by cosine similarity.
    """
    if samples.ndim > 1:
        samples = samples.mean(axis=1)
    x = samples.astype(np.float64)
    rms = float(np.sqrt(np.mean(x ** 2))) if x.size else 0.0
    if rms > 0:
        x = x / rms
    x = _trim_silence(x, sample_rate)
    if x.size == 0:
        return np.zeros(FP_BANDS)
    vec = _log_band_spectrum(x, sample_rate)
    norm = float(np.linalg.norm(vec))
    return vec / norm if norm > 0 else vec


def audio_fingerprint(samples: np.ndarray, sample_rate: int) -> str:
    """Hex encoding of the quantized fingerprint vector.

    Each component lies in roughly [-1, 1] and is stored as a uint8 offset by
    128, so the hex string is exactly FP_BANDS bytes.
    """
    vec = np.clip(fingerprint_vector(samples, sample_rate), -1.0, 1.0)
    return ((vec * 127.0) + 128.0).astype(np.uint8).tobytes().hex()


def fingerprint_similarity(fp_a: str, fp_b: str) -> float:
    """Cosine similarity in [-1, 1] between two fingerprint vectors.

    Gain differences are removed before quantizing, so an identical segment at
    a different level scores ~1.0; unrelated spectra score far lower.
    """
    try:
        a = np.frombuffer(bytes.fromhex(fp_a), dtype=np.uint8).astype(np.float64)
        b = np.frombuffer(bytes.fromhex(fp_b), dtype=np.uint8).astype(np.float64)
    except ValueError:
        return 0.0
    if a.size == 0 or a.size != b.size:
        return 0.0
    a -= 128.0
    b -= 128.0
    na, nb = float(np.linalg.norm(a)), float(np.linalg.norm(b))
    if na == 0.0 or nb == 0.0:
        return 1.0 if np.array_equal(a, b) else 0.0
    return float(np.dot(a, b) / (na * nb))


def find_duplicates(fingerprints: list[str],
                    threshold: float = 0.95) -> dict[int, int]:
    """Map duplicate index -> representative (first) index.

    Exact equality is always a duplicate; near-identical vectors above
    `threshold` absorb quantization jitter (gain-variant re-encodes and
    leading-silence differences score >= ~0.99 on the fixture set, while
    unrelated programme scores < ~0.90).
    """
    mapping: dict[int, int] = {}
    reps: list[int] = []
    for i, fp in enumerate(fingerprints):
        match = None
        for r in reps:
            if fingerprints[r] == fp or fingerprint_similarity(fingerprints[r], fp) >= threshold:
                match = r
                break
        if match is None:
            reps.append(i)
        else:
            mapping[i] = match
    return mapping


def scan_silence_file(path, sample_rate: int, block_seconds: int = 30
                      ) -> list[tuple[int, int]]:
    """Stream a WAV file in blocks and return merged silent intervals in ms.

    Reading in bounded blocks keeps memory flat regardless of program length;
    intervals that straddle a block boundary are merged afterwards.
    """
    import soundfile as sf

    intervals: list[tuple[int, int]] = []
    block = sample_rate * block_seconds
    done = 0
    with sf.SoundFile(str(path)) as snd:
        while True:
            data = snd.read(block, dtype="int16", always_2d=False)
            if len(data) == 0:
                break
            base_ms = (done * 1000 + sample_rate // 2) // sample_rate
            for a, b in detect_silence(data, sample_rate, min_ms=0):
                intervals.append((base_ms + a, base_ms + b))
            done += len(data)
    return merge_intervals(intervals)
