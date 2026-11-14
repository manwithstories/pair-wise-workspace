"""Peak, RMS, and clipping measurements for float audio."""
from __future__ import annotations

from dataclasses import dataclass, field
import math

import numpy as np

SILENCE_DBFS = -120.0


def _db(value: float, amplitude: bool = True) -> float:
    if value <= 0.0 or not math.isfinite(value):
        return SILENCE_DBFS
    if amplitude:
        return max(SILENCE_DBFS, 20.0 * math.log10(value))
    return max(SILENCE_DBFS, 10.0 * math.log10(value))


@dataclass(frozen=True)
class Metric:
    name: str
    peak_dbfs: float
    rms_dbfs: float
    clip_samples: int

    def as_dict(self) -> dict[str, object]:
        return {
            "name": self.name,
            "peak_dbfs": round(self.peak_dbfs, 6),
            "rms_dbfs": round(self.rms_dbfs, 6),
            "clip_samples": int(self.clip_samples),
        }


@dataclass
class _Accumulator:
    peak: float = 0.0
    sum_sq: float = 0.0
    samples: int = 0
    clips: int = 0

    def observe(self, audio: np.ndarray) -> None:
        data = np.asarray(audio, dtype=np.float64)
        if data.size == 0:
            return
        magnitude = np.abs(data)
        self.peak = max(self.peak, float(magnitude.max()))
        self.sum_sq += float(np.square(data).sum())
        self.samples += int(data.size)
        self.clips += int(np.count_nonzero(magnitude > 1.0))

    def merge(self, other: "_Accumulator") -> None:
        self.peak = max(self.peak, other.peak)
        self.sum_sq += other.sum_sq
        self.samples += other.samples
        self.clips += other.clips

    def metric(self, name: str) -> Metric:
        rms = math.sqrt(self.sum_sq / self.samples) if self.samples else 0.0
        return Metric(name, _db(self.peak), _db(rms), self.clips)


class Meter:
    """Accumulate final metrics and report the latest block separately."""

    def __init__(self, name: str):
        self.name = name
        self._total = _Accumulator()
        self._last = _Accumulator()

    def update(self, audio: np.ndarray) -> Metric:
        self._last = _Accumulator()
        self._last.observe(audio)
        self._total.merge(self._last)
        return self._last.metric(self.name)

    def total(self) -> Metric:
        return self._total.metric(self.name)

    def last(self) -> Metric:
        return self._last.metric(self.name)


class MeterBank:
    def __init__(self, names: list[str]):
        self._meters = {name: Meter(name) for name in names}

    def update(self, name: str, audio: np.ndarray) -> Metric:
        return self._meters[name].update(audio)

    def get(self, name: str) -> Meter:
        return self._meters[name]

    def total(self, name: str) -> Metric:
        return self._meters[name].total()

    def last(self, name: str) -> Metric:
        return self._meters[name].last()

    def last_all(self) -> dict[str, Metric]:
        return {name: meter.last() for name, meter in self._meters.items()}
