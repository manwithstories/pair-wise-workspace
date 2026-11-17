"""Deterministic, seed-driven fixtures: directions, signals, layouts, PCM.

Everything is generated in code from an explicit seed — no external files,
no network, no real devices. np.random.default_rng (PCG64) is stable
across NumPy versions, so fixtures are reproducible.
"""
from __future__ import annotations

import numpy as np

from .types import AudioFrame, ChannelLayout, Direction, NumericalError

#: well-spread base azimuths; seeded jitter keeps sources off loudspeakers
BASE_AZIMUTHS = (-150.0, -90.0, -30.0, 30.0, 90.0, 150.0)

#: elevation cap keeps energy-conservation error of a horizontal ring < 1 dB
MAX_FIXTURE_ELEVATION_DEG = 30.0


def _wrap_azimuth(az: float) -> float:
    return (az + 180.0) % 360.0 - 180.0


def make_directions(seed: int, count: int = 6) -> list[Direction]:
    """`count` reproducible source directions (seeded jitter, capped elevation)."""
    rng = np.random.default_rng(seed)
    dirs = []
    for i in range(count):
        base = BASE_AZIMUTHS[i % len(BASE_AZIMUTHS)]
        az = _wrap_azimuth(base + float(rng.uniform(-8.0, 8.0)))
        el = float(rng.uniform(-MAX_FIXTURE_ELEVATION_DEG, MAX_FIXTURE_ELEVATION_DEG))
        dirs.append(Direction(az, el))
    return dirs


def make_source_signals(
    seed: int,
    count: int,
    frames: int,
    sample_rate: int,
    inject_silence: bool = False,
    inject_clipping: bool = False,
) -> np.ndarray:
    """`count` distinct deterministic mono signals, shaped (count, frames).

    inject_silence: zero the middle half of source 0 (silent-frame path).
    inject_clipping: boost source 1 so the mix exceeds full scale.
    """
    rng = np.random.default_rng(seed + 1)
    t = np.arange(frames, dtype=np.float64) / float(sample_rate)
    out = np.zeros((count, frames), dtype=np.float64)
    for i in range(count):
        f1 = 110.0 + 25.0 * i + float(rng.uniform(-5.0, 5.0))
        f2 = 2.01 * f1
        phase = float(rng.uniform(0.0, 2.0 * np.pi))
        out[i] = 0.15 * np.sin(2.0 * np.pi * f1 * t + phase)
        out[i] += 0.05 * np.sin(2.0 * np.pi * f2 * t)
    if inject_silence and count:
        out[0, frames // 4 : frames // 2] = 0.0
    if inject_clipping and count > 1:
        out[1] *= 8.0
    return out


_LAYOUT_AZIMUTHS = {
    "quad": (45.0, 135.0, -135.0, -45.0),
    "ring6": tuple(float(a) for a in range(0, 360, 60)),
    "ring8": tuple(float(a) for a in range(0, 360, 45)),
    # pathological: duplicated/collinear directions, for stability tests
    "singular": (0.0, 0.0, 10.0, 10.0),
}


def layout_names() -> tuple[str, ...]:
    return tuple(_LAYOUT_AZIMUTHS)


def make_layout(name: str) -> ChannelLayout:
    """Named loudspeaker layout; unknown names are rejected."""
    key = str(name).strip().lower()
    if key not in _LAYOUT_AZIMUTHS:
        raise ValueError(
            f"unknown layout {name!r}; expected one of {sorted(_LAYOUT_AZIMUTHS)}"
        )
    dirs = tuple(Direction(_wrap_azimuth(a), 0.0) for a in _LAYOUT_AZIMUTHS[key])
    return ChannelLayout(key, dirs)


def to_pcm16(samples: np.ndarray) -> np.ndarray:
    """Float [-1, 1] -> int16 PCM (caller must validate range first).

    Symmetric scaling: write and read both use 2**15, so the roundtrip
    error is bounded by half a quantization step (0.5 / 32768).
    """
    clipped = np.clip(np.asarray(samples, dtype=np.float64), -1.0, 32767.0 / 32768.0)
    return np.round(clipped * 32768.0).astype("<i2")


def from_pcm16(pcm: np.ndarray) -> np.ndarray:
    """int16 PCM -> float64 in [-1, 1]."""
    return np.asarray(pcm).astype(np.float64) / 32768.0


def validate_for_write(frame: AudioFrame) -> None:
    """Pre-write checks: empty blocks, NaN/Inf, and clipping are rejected."""
    if frame.frame_count == 0:
        raise ValueError("refusing to write an empty frame block")
    if not np.all(np.isfinite(frame.samples)):
        raise NumericalError("refusing to write NaN/Inf samples")
    if float(np.max(np.abs(frame.samples))) > 1.0:
        raise NumericalError(
            "refusing to write clipped samples (peak > 1.0); normalize first"
        )
