"""Signal and decoder quality metrics.

All metrics are deterministic functions of the rendered blocks; nothing
here generates or mutates audio.
"""
from __future__ import annotations

import math

import numpy as np

_EPS = 1e-12


def total_energy(x: np.ndarray) -> float:
    """Sum of squares over the whole block."""
    return float(np.sum(np.asarray(x, dtype=np.float64) ** 2))


def rms_per_channel(x: np.ndarray) -> np.ndarray:
    """RMS of each channel of a (frames, channels) block."""
    x = np.asarray(x, dtype=np.float64)
    if x.shape[0] == 0:
        raise ValueError("empty frame block")
    return np.sqrt(np.mean(x**2, axis=0))


def peak(x: np.ndarray) -> float:
    """Largest absolute sample value."""
    x = np.asarray(x, dtype=np.float64)
    return float(np.max(np.abs(x))) if x.size else 0.0


def condition_number(A: np.ndarray) -> float:
    """2-norm condition number via SVD (inf if exactly singular)."""
    s = np.linalg.svd(np.asarray(A, dtype=np.float64), compute_uv=False)
    if s.size == 0 or s[-1] <= 0.0:
        return float("inf")
    return float(s[0] / s[-1])


def energy_by_direction(per_source_frames: list[np.ndarray]) -> list[float]:
    """Total encoded energy contributed by each source."""
    return [total_energy(f) for f in per_source_frames]


def energy_error_db(e_in: list[float], e_out: list[float]) -> float:
    """Worst absolute energy-conservation error across sources, in dB."""
    worst = 0.0
    for i, o in zip(e_in, e_out):
        if i <= 0.0:
            continue
        worst = max(worst, abs(10.0 * math.log10(max(o, _EPS) / i)))
    return worst


def adjacent_crosstalk_db(matrix: np.ndarray, basis: np.ndarray) -> float:
    """Worst-case loudspeaker-to-loudspeaker crosstalk of the decoder.

    For a source sitting exactly on loudspeaker k, the target channel is k;
    the crosstalk is the loudest other channel relative to it. First-order
    decoding has inherently wide directivity, so this sits near -2 dB and
    is reported for information only.
    """
    G = matrix @ basis.T  # (speakers, speakers): response to source at each
    worst = -120.0
    for k in range(G.shape[1]):
        target = abs(G[k, k])
        if target <= 0.0:
            continue
        others = max(abs(G[j, k]) for j in range(G.shape[0]) if j != k)
        worst = max(worst, 20.0 * math.log10(max(others, _EPS) / target))
    return worst


def binaural_crosstalk_db(
    left_gains: np.ndarray, right_gains: np.ndarray
) -> float:
    """Worst-case interaural leakage of the binaural render path.

    Each virtual loudspeaker is fed alone; the crosstalk is the
    contralateral-to-ipsilateral ear-gain ratio. Set by the synthetic
    head-shadow curve (worst at +-90 deg azimuth).
    """
    worst = -120.0
    for gl, gr in zip(left_gains, right_gains):
        ipsi, contra = max(gl, gr), min(gl, gr)
        if ipsi > 0.0:
            worst = max(worst, 20.0 * math.log10(max(contra, _EPS) / ipsi))
    return worst


def interaural_leakage_db(
    left: np.ndarray, right: np.ndarray, azimuth_deg: float
) -> float:
    """Per-source contralateral leakage of a rendered binaural pair."""
    li = float(np.sqrt(np.mean(np.asarray(left) ** 2)))
    ri = float(np.sqrt(np.mean(np.asarray(right) ** 2)))
    if azimuth_deg == 0.0:
        return 0.0
    ipsi, contra = (li, ri) if azimuth_deg > 0.0 else (ri, li)
    if ipsi <= 0.0:
        return -120.0
    return 20.0 * math.log10(max(contra, _EPS) / ipsi)


def channel_spread_db(rms: np.ndarray) -> float:
    """Largest per-channel RMS deviation from the mean channel RMS, in dB."""
    rms = np.asarray(rms, dtype=np.float64)
    mean = float(np.mean(rms))
    if mean <= 0.0:
        return 0.0
    return float(
        np.max(np.abs(20.0 * np.log10(np.maximum(rms, _EPS) / mean)))
    )
