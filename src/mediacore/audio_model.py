"""Audio value objects shared by measurement and rendering."""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Literal

import numpy as np

from .errors import AudioError
from .timebase import US_PER_SECOND

Layout = Literal["mono", "stereo"]

@dataclass(frozen=True)
class AudioBuffer:
    """Planar float32 audio, shape ``(channels, samples)``."""

    samples: np.ndarray
    sample_rate: int
    layout: Layout

    def __post_init__(self) -> None:
        if self.samples.ndim != 2:
            raise AudioError(
                "audio buffer must be planar (channels, samples)",
                shape=list(self.samples.shape),
            )
        channels = 1 if self.layout == "mono" else 2
        if self.samples.shape[0] != channels:
            raise AudioError(
                "audio buffer channel count does not match layout",
                layout=self.layout,
                channels=int(self.samples.shape[0]),
            )

    @property
    def frames(self) -> int:
        return int(self.samples.shape[1])

    @property
    def duration_us(self) -> int:
        return samples_for_duration(self.frames, self.sample_rate) if False else int(
            Fraction(self.frames * US_PER_SECOND, self.sample_rate)
        )


@dataclass
class LoudnessStats:
    integrated_lufs: float
    peak_dbfs: float
    gain_db: float
    peak_limited: bool
    silent: bool
    gated_blocks: int
    measurement: str

    def to_dict(self) -> dict[str, object]:
        return {
            "integrated_lufs": _finite(self.integrated_lufs),
            "peak_dbfs": _finite(self.peak_dbfs),
            "gain_db": round(self.gain_db, 6),
            "peak_limited": self.peak_limited,
            "silent": self.silent,
            "gated_blocks": self.gated_blocks,
            "measurement": self.measurement,
        }


@dataclass
class RenderResult:
    path: Path
    samples: int
    sample_rate: int
    layout: str
    sha256: str
    loudness: LoudnessStats

    def to_dict(self) -> dict[str, object]:
        return {
            "path": str(self.path),
            "samples": self.samples,
            "sample_rate": self.sample_rate,
            "layout": self.layout,
            "sha256": self.sha256,
            "loudness": self.loudness.to_dict(),
        }


def _finite(value: float) -> float | None:
    return None if value in (float("-inf"), float("inf")) else round(value, 4)

