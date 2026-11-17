"""Core data structures shared across the aoa toolkit."""
from __future__ import annotations

import math
from dataclasses import dataclass, field
from enum import Enum

import numpy as np


class NormMode(str, Enum):
    """Supported first-order ambisonics conventions."""

    ACN_SN3D = "acn-sn3d"
    ACN_N3D = "acn-n3d"
    FUMA = "fuma"

    @classmethod
    def parse(cls, text: str) -> "NormMode":
        key = str(text).strip().lower()
        for mode in cls:
            if mode.value == key:
                return mode
        raise ValueError(
            f"unknown normalization convention {text!r}; "
            f"expected one of {[m.value for m in cls]}"
        )


class DirectionError(ValueError):
    """Raised when a direction falls outside the supported ranges."""


class NumericalError(RuntimeError):
    """Raised on NaN/Inf, clipping, or unstable numerical state."""


@dataclass(frozen=True)
class Direction:
    """A source/loudspeaker direction on the unit sphere.

    Azimuth in [-180, 180] degrees (0 = front, positive = left).
    Elevation in [-90, 90] degrees (positive = up).
    """

    azimuth_deg: float
    elevation_deg: float = 0.0

    def __post_init__(self) -> None:
        if not -180.0 <= self.azimuth_deg <= 180.0:
            raise DirectionError(
                f"azimuth {self.azimuth_deg} out of range [-180, 180]"
            )
        if not -90.0 <= self.elevation_deg <= 90.0:
            raise DirectionError(
                f"elevation {self.elevation_deg} out of range [-90, 90]"
            )

    def unit_vector(self) -> np.ndarray:
        """Return (x, y, z) with x = front, y = left, z = up."""
        az = math.radians(self.azimuth_deg)
        el = math.radians(self.elevation_deg)
        ce = math.cos(el)
        return np.array(
            [ce * math.cos(az), ce * math.sin(az), math.sin(el)],
            dtype=np.float64,
        )


@dataclass
class AudioFrame:
    """A block of multi-channel audio: samples shaped (frames, channels)."""

    samples: np.ndarray
    sample_rate: int

    def __post_init__(self) -> None:
        arr = np.asarray(self.samples)
        if arr.ndim != 2:
            raise ValueError("samples must be a 2-D array (frames, channels)")
        if self.sample_rate <= 0:
            raise ValueError("sample_rate must be positive")
        self.samples = arr

    @property
    def frame_count(self) -> int:
        return int(self.samples.shape[0])

    @property
    def channel_count(self) -> int:
        return int(self.samples.shape[1])


@dataclass(frozen=True)
class ChannelLayout:
    """A named set of loudspeaker directions."""

    name: str
    directions: tuple[Direction, ...]

    @property
    def channel_count(self) -> int:
        return len(self.directions)


@dataclass
class Diagnostics:
    """Numerical health information collected through the pipeline."""

    warnings: list[str] = field(default_factory=list)
    peak: float = 0.0
    clipped_samples: int = 0
    condition_number: float = 1.0
    strategy: str = ""

    def warn(self, message: str) -> None:
        self.warnings.append(message)
