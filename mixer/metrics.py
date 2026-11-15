"""Peak, RMS, clipping-count, and clipping-run metrics."""

from __future__ import annotations

import math
from dataclasses import dataclass, field

import numpy as np


def amplitude_to_dbfs(value: float) -> float | None:
    if value <= 0.0 or not math.isfinite(value):
        return None
    return 20.0 * math.log10(value)


@dataclass
class MetricsSnapshot:
    peak_dbfs: float | None
    rms_dbfs: float | None
    clip_count: int
    clip_runs: int
    clip_times: list[float] = field(default_factory=list)

    def to_dict(self) -> dict[str, object]:
        return {
            "peak_dbfs": self.peak_dbfs,
            "rms_dbfs": self.rms_dbfs,
            "clip_count": int(self.clip_count),
            "clip_runs": int(self.clip_runs),
            "clip_times": list(self.clip_times),
        }


class MetricsAccumulator:
    """Accumulate statistics block by block in constant memory."""

    def __init__(self, sample_rate: int, max_clip_times: int = 5) -> None:
        if sample_rate <= 0:
            raise ValueError("sample rate must be positive")
        self.sample_rate = int(sample_rate)
        self.max_clip_times = int(max_clip_times)
        self.peak = 0.0
        self.sum_squares = 0.0
        self.sample_count = 0
        self.clip_count = 0
        self.clip_runs = 0
        self.clip_times: list[float] = []
        self._previous_clipped = False

    def update(self, data: np.ndarray, start_frame: int = 0) -> None:
        array = np.asarray(data, dtype=np.float64)
        if array.ndim == 1:
            array = array[:, None]
        if array.ndim != 2:
            raise ValueError("metrics input must be one- or two-dimensional")
        if array.shape[0] == 0 or array.shape[1] == 0:
            return
        finite = np.isfinite(array)
        cleaned = np.where(finite, array, 0.0)
        self.peak = max(self.peak, float(np.max(np.abs(cleaned), initial=0.0)))
        self.sum_squares += float(np.sum(np.square(cleaned)))
        self.sample_count += int(cleaned.size)
        clipped = np.abs(cleaned) >= 1.0
        self.clip_count += int(np.count_nonzero(clipped))
        frame_clipped = np.any(clipped, axis=1)
        if frame_clipped.size:
            transitions = np.diff(
                np.concatenate(([int(self._previous_clipped)], frame_clipped.astype(np.int8)))
            )
            self.clip_runs += int(np.count_nonzero(transitions == 1))
            self._previous_clipped = bool(frame_clipped[-1])
            if len(self.clip_times) < self.max_clip_times:
                indices = np.flatnonzero(frame_clipped)
                for index in indices:
                    if len(self.clip_times) >= self.max_clip_times:
                        break
                    self.clip_times.append(
                        (int(start_frame) + int(index)) / float(self.sample_rate)
                    )

    def snapshot(self) -> MetricsSnapshot:
        rms = math.sqrt(self.sum_squares / self.sample_count) if self.sample_count else 0.0
        return MetricsSnapshot(
            peak_dbfs=amplitude_to_dbfs(self.peak),
            rms_dbfs=amplitude_to_dbfs(rms),
            clip_count=self.clip_count,
            clip_runs=self.clip_runs,
            clip_times=list(self.clip_times),
        )


def compute_metrics(
    data: np.ndarray, sample_rate: int, start_frame: int = 0
) -> MetricsSnapshot:
    accumulator = MetricsAccumulator(sample_rate)
    accumulator.update(data, start_frame=start_frame)
    return accumulator.snapshot()


def peak_dbfs(data: np.ndarray) -> float | None:
    array = np.asarray(data, dtype=np.float64)
    if array.size == 0:
        return None
    return amplitude_to_dbfs(float(np.max(np.abs(array), initial=0.0)))


def rms_dbfs(data: np.ndarray) -> float | None:
    array = np.asarray(data, dtype=np.float64)
    if array.size == 0:
        return None
    return amplitude_to_dbfs(float(np.sqrt(np.mean(np.square(array)))))


def clip_stats(
    data: np.ndarray, sample_rate: int, start_frame: int = 0
) -> dict[str, object]:
    return compute_metrics(data, sample_rate, start_frame).to_dict()


Metrics = MetricsSnapshot
measure = compute_metrics
