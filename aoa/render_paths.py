"""Render paths: loudspeaker ring and binaural virtual loudspeakers.

Both paths consume the same encoded B-format and the same decoder matrix;
the binaural path additionally mixes the loudspeaker feeds to two ears
through a synthetic head-shadow model.
"""
from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from . import metrics
from .types import ChannelLayout


@dataclass
class RenderResult:
    signals: np.ndarray  # (frames, channels)
    rms: np.ndarray  # per-channel RMS
    peak: float


def _result(signals: np.ndarray) -> RenderResult:
    return RenderResult(signals, metrics.rms_per_channel(signals), metrics.peak(signals))


def render_speakers(b_frames: np.ndarray, matrix: np.ndarray) -> RenderResult:
    """Decode B-format (frames, 4) to loudspeaker feeds (frames, speakers)."""
    b_frames = np.asarray(b_frames, dtype=np.float64)
    if b_frames.ndim != 2 or b_frames.shape[1] != 4:
        raise ValueError(f"expected (frames, 4) B-format, got {b_frames.shape}")
    return _result(b_frames @ matrix.T)


def hrtf_ear_gains(layout: ChannelLayout) -> tuple[np.ndarray, np.ndarray]:
    """Synthetic (left, right) ear gains per loudspeaker.

    Zero-frequency head-shadow approximation: the ipsilateral ear is at
    0 dB, the contralateral ear is attenuated by 8 + 18*|sin(azimuth)| dB
    (worst-case 26 dB at +-90 deg). Loudspeakers on the front/back axis
    feed both ears symmetrically. Deterministic and device-free.
    """
    left, right = [], []
    for d in layout.directions:
        az = math.radians(d.azimuth_deg)
        s, c = math.sin(az), math.cos(az)
        shadow = 10.0 ** (-(8.0 + 18.0 * abs(s)) / 20.0)
        if abs(s) < 1e-9:  # front/back axis: symmetric to both ears
            gain = 1.0 if c > 0.0 else shadow
            left.append(gain)
            right.append(gain)
        elif s > 0.0:  # loudspeaker on the left
            left.append(1.0)
            right.append(shadow)
        else:
            left.append(shadow)
            right.append(1.0)
    return np.asarray(left), np.asarray(right)


def render_binaural(
    speaker_signals: np.ndarray, layout: ChannelLayout
) -> RenderResult:
    """Mix loudspeaker feeds down to a (frames, 2) binaural pair."""
    speaker_signals = np.asarray(speaker_signals, dtype=np.float64)
    if speaker_signals.shape[1] != layout.channel_count:
        raise ValueError(
            f"expected {layout.channel_count} loudspeaker channels, "
            f"got {speaker_signals.shape[1]}"
        )
    gl, gr = hrtf_ear_gains(layout)
    left = speaker_signals @ gl
    right = speaker_signals @ gr
    return _result(np.stack([left, right], axis=1))
