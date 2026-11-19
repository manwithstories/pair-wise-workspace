"""Audio analysis: chunked-RMS silence detection and normalized fingerprints.

Silence is measured per fixed-size block with NumPy RMS, merged across block
boundaries; intervals shorter than 300 ms are not reported. Fingerprints are
computed on gain-normalized, silence-trimmed, resampled audio so duplicates
with different gain or edge padding still match.
"""

from __future__ import annotations

import hashlib

import numpy as np

SILENCE_THRESHOLD_DB = -50.0
MIN_SILENCE_MS = 300
BLOCK_MS = 20
FP_RATE = 8000          # fingerprint resample rate
FP_BANDS = 32           # spectral bands in the fingerprint


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
    """
    if samples.ndim > 1:
        samples = samples.mean(axis=1)
    block = max(1, sample_rate * block_ms // 1000)
    silent_blocks: list[bool] = []
    for start in range(0, len(samples), block):
        silent_blocks.append(_rms_db(samples[start:start + block]) < threshold_db)
    intervals: list[tuple[int, int]] = []
    run_start: int | None = None
    for i, is_silent in enumerate(silent_blocks):
        if is_silent and run_start is None:
            run_start = i
        elif not is_silent and run_start is not None:
            intervals.append((run_start * block_ms, i * block_ms))
            run_start = None
    if run_start is not None:
        intervals.append((run_start * block_ms, len(silent_blocks) * block_ms))
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


def audio_fingerprint(samples: np.ndarray, sample_rate: int) -> str:
    """Gain- and edge-silence-invariant fingerprint (hex digest).

    Mono mixdown -> RMS normalize -> trim edge silence -> resample to 8 kHz
    -> 32-band quantized log-energy vector -> sha256.
    """
    if samples.ndim > 1:
        samples = samples.mean(axis=1)
    x = samples.astype(np.float64)
    rms = float(np.sqrt(np.mean(x ** 2))) if x.size else 0.0
    if rms > 0:
        x = x / rms
    x = _trim_silence(x, sample_rate)
    if x.size == 0:
        return hashlib.sha256(b"silence").hexdigest()
    x = _resample_linear(x, sample_rate, FP_RATE)
    spectrum = np.abs(np.fft.rfft(x))
    band_edges = np.geomspace(1, len(spectrum) + 1, FP_BANDS + 1).astype(np.int64)
    band_energy = np.array([
        spectrum[band_edges[i]:band_edges[i + 1]].sum()
        for i in range(FP_BANDS)])
    peak = band_energy.max()
    if peak > 0:
        band_energy = band_energy / peak
    quantized = np.clip((band_energy * 255).astype(np.uint8), 0, 255)
    return hashlib.sha256(quantized.tobytes()).hexdigest()


def fingerprint_similarity(fp_a: str, fp_b: str) -> float:
    """Hamming similarity in [0, 1] between two fingerprint hex digests."""
    a = bytes.fromhex(fp_a)
    b = bytes.fromhex(fp_b)
    if len(a) != len(b) or not a:
        return 0.0
    same = sum(1 for x, y in zip(a, b) if x == y)
    return same / len(a)


def find_duplicates(fingerprints: list[str],
                    threshold: float = 0.90) -> dict[int, int]:
    """Map duplicate index -> representative (first) index.

    Exact digest equality is always a duplicate; near-identical digests above
    `threshold` absorb quantization jitter (e.g. gain-variant re-encodes).
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
