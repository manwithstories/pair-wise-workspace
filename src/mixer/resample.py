"""Rate conversion to the 48 kHz target via piecewise-linear interpolation.

The resampler never copies a whole track: it maps a destination frame range
back to the source grid and samples only that window. Windows are advanced
per block by the streaming engine, so memory stays proportional to one block
regardless of timeline length.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Sequence

import numpy as np

from .errors import MixerError, EXIT_FORMAT
from .model import SUPPORTED_RATES, TARGET_RATE


def validate_rate(rate: int, track: str | None = None) -> int:
    if rate not in SUPPORTED_RATES:
        raise MixerError(
            "invalid_rate",
            f"unsupported sample rate {rate}; expected one of {SUPPORTED_RATES}",
            EXIT_FORMAT,
            track=track,
            details={"rate": rate, "supported": list(SUPPORTED_RATES)},
        )
    return int(rate)


@dataclass(frozen=True)
class ResamplePlan:
    """Maps destination frames to source frames for one clip."""

    source_rate: int
    target_rate: int
    source_offset: int
    dest_start: int
    dest_frames: int
    source_frames: int

    @property
    def ratio(self) -> float:
        return self.source_rate / float(self.target_rate)

    @property
    def is_passthrough(self) -> bool:
        return self.source_rate == self.target_rate

    def source_span(self) -> tuple[float, float]:
        """Half-open source frame range this plan reads, in source frames."""
        first = self.source_offset + self.dest_start * self.ratio
        last = self.source_offset + (self.dest_start + self.dest_frames) * self.ratio
        return first, last

    def to_dict(self) -> dict:
        return {
            "source_rate": self.source_rate,
            "target_rate": self.target_rate,
            "source_offset": self.source_offset,
            "dest_start": self.dest_start,
            "dest_frames": self.dest_frames,
            "source_frames": self.source_frames,
        }


def frames_for_duration(seconds: float, rate: int) -> int:
    return max(0, int(round(seconds * rate)))


def resample_duration_frames(source_frames: int, source_rate: int, target_rate: int) -> int:
    if source_rate <= 0:
        return 0
    return int(round(source_frames * target_rate / float(source_rate)))


class LinearResampler:
    """Piecewise-linear resampling of a pre-read source window."""

    def __init__(self, source: np.ndarray, source_rate: int, target_rate: int = TARGET_RATE):
        if source.ndim != 2:
            raise ValueError("resampler expects a 2-D (frames, channels) array")
        self.source = source
        self.source_rate = validate_rate(source_rate)
        self.target_rate = validate_rate(target_rate)

    def convert(self, count: int, start_frame: int = 0) -> np.ndarray:
        """Resample ``count`` destination frames starting at ``start_frame``.

        Reads at most ``ceil(count * ratio) + 2`` source frames, zero-padding
        past the end of the source so short tails and empty clips are handled
        without special cases.
        """
        channels = self.source.shape[1]
        if count <= 0:
            return np.zeros((0, channels), dtype=np.float64)
        if self.source.shape[0] == 0:
            return np.zeros((count, channels), dtype=np.float64)

        ratio = self.source_rate / float(self.target_rate)
        positions = start_frame * ratio + np.arange(count, dtype=np.float64) * ratio
        index = np.floor(positions).astype(np.int64)
        frac = positions - index

        valid = (index >= 0) & (index < self.source.shape[0])
        base = np.zeros((count, channels), dtype=np.float64)
        base[valid] = self.source[index[valid]]
        nxt = np.zeros((count, channels), dtype=np.float64)
        next_valid = (index + 1 >= 0) & (index + 1 < self.source.shape[0])
        nxt[next_valid] = self.source[index[next_valid] + 1]
        delta = nxt - base
        return base + delta * frac[:, None]

    def convert_all(self) -> np.ndarray:
        frames = resample_duration_frames(self.source.shape[0], self.source_rate, self.target_rate)
        return self.convert(frames, 0)


def trim_or_pad(source: np.ndarray, offset: int, length: int | None) -> np.ndarray:
    """Apply a clip's source offset/length, padding with zeros on the tail."""
    if offset < 0:
        raise MixerError("invalid_clip_offset", "clip source_offset_frames must be >= 0", EXIT_FORMAT)
    if length is not None and length < 0:
        raise MixerError("invalid_clip_length", "clip length_frames must be >= 0", EXIT_FORMAT)
    channels = source.shape[1]
    window = source[offset:] if offset else source
    if length is None:
        return window
    if window.shape[0] >= length:
        return window[:length]
    pad = np.zeros((length - window.shape[0], channels), dtype=np.float64)
    return np.concatenate((window, pad), axis=0)


def pad_tail(source: np.ndarray, target_frames: int) -> np.ndarray:
    """Zero-extend a clip to the track's target length (tail padding)."""
    if source.shape[0] >= target_frames:
        return source[:target_frames]
    return np.concatenate(
        (source, np.zeros((target_frames - source.shape[0], source.shape[1]))), axis=0
    )


def resample_array(
    source: np.ndarray, source_rate: int, target_rate: int = TARGET_RATE
) -> np.ndarray:
    """Whole-array helper (fixtures and tests); block use ``LinearResampler``."""
    return LinearResampler(source, source_rate, target_rate).convert_all()
