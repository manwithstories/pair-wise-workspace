"""Streaming level evidence and JSON-safe report structures."""

from __future__ import annotations

import math
from typing import Any

import numpy as np

from .automation import AutomationCurve
from .manifest import Project, Track


MARKER = "-inf"
STATS_CHUNK = 4096


def dbfs(value: float) -> float | str:
    if not math.isfinite(value) or value <= 0.0:
        return MARKER
    return float(20.0 * math.log10(value))


class LevelAccumulator:
    def __init__(self) -> None:
        self.peak = 0.0
        self.square_sum = 0.0
        self.samples = 0
        self.values = 0
        self.clip_count = 0
        self._residual: np.ndarray | None = None

    def update(self, block: np.ndarray) -> None:
        audio = np.asarray(block, dtype=np.float32)
        if audio.size == 0:
            return
        if self._residual is None:
            self._residual = np.zeros((audio.shape[0], 0), dtype=np.float32)
        if audio.shape[0] != self._residual.shape[0]:
            raise ValueError("statistics channel layout changed between blocks")
        self.samples += audio.shape[1]
        self.values += audio.size
        offset = 0
        if self._residual.shape[1]:
            needed = STATS_CHUNK - self._residual.shape[1]
            prefix = audio[:, :needed]
            self._consume(np.concatenate((self._residual, prefix), axis=1))
            offset = prefix.shape[1]
        while offset + STATS_CHUNK <= audio.shape[1]:
            self._consume(audio[:, offset : offset + STATS_CHUNK])
            offset += STATS_CHUNK
        self._residual = np.array(audio[:, offset:], dtype=np.float32, copy=True)

    def _consume(self, chunk: np.ndarray) -> None:
        if chunk.shape[1] == 0:
            return
        absolute = np.abs(chunk)
        self.peak = max(self.peak, float(np.max(absolute)))
        self.square_sum += float(np.sum(np.square(chunk, dtype=np.float64), dtype=np.float64))
        self.clip_count += int(np.count_nonzero(absolute >= 1.0))

    def evidence(self) -> dict[str, Any]:
        if self._residual is not None and self._residual.shape[1]:
            self._consume(self._residual)
            self._residual = self._residual[:, :0]
        rms = math.sqrt(self.square_sum / self.values) if self.values else 0.0
        return {
            "peak_dbfs": dbfs(self.peak),
            "rms_dbfs": dbfs(rms),
            "samples": self.samples,
            "clip_count": self.clip_count,
        }


class TrackEvidence:
    def __init__(self, track: Track, sample_rate: int, duration_frames: int) -> None:
        self.track = track
        self.sample_rate = sample_rate
        self.duration_frames = duration_frames
        self.level = LevelAccumulator()
        self.gain_db_min: float | None = None
        self.gain_db_max: float | None = None

    def update_gain(self, values: np.ndarray | float) -> None:
        array = np.asarray(values, dtype=np.float64)
        if array.size == 0:
            return
        low, high = float(np.min(array)), float(np.max(array))
        self.gain_db_min = low if self.gain_db_min is None else min(self.gain_db_min, low)
        self.gain_db_max = high if self.gain_db_max is None else max(self.gain_db_max, high)

    def as_dict(self, project: Project) -> dict[str, Any]:
        level = self.level.evidence()
        ratio = self.track.resampled_frames / self.track.samples_in if self.track.samples_in else None
        return {
            "id": self.track.id,
            "peak_dbfs": level["peak_dbfs"],
            "rms_dbfs": level["rms_dbfs"],
            "clip_count": level["clip_count"],
            "samples_in": self.track.samples_in,
            "samples_out": self.level.samples,
            "resampled_samples": self.track.resampled_frames,
            "samples_resampled": self.track.resampled_frames,
            "resample_ratio": ratio,
            "file_sample_rate": self.track.file_rate,
            "project_sample_rate": project.sample_rate,
            "gain_db_min": self.gain_db_min,
            "gain_db_max": self.gain_db_max,
            "automation_points": self.track.automation.points if self.track.automation else 0,
            "automation_breakpoints": len(self.track.automation.breakpoints) if self.track.automation else 0,
        }


class MixEvidence:
    def __init__(self, project: Project) -> None:
        self.project = project
        self.output = LevelAccumulator()
        self.pre_limiter = LevelAccumulator()
        self.has_limiter = False

    def as_dict(self) -> dict[str, Any]:
        level = self.output.evidence()
        before = self.pre_limiter.clip_count if self.has_limiter else level["clip_count"]
        after = level["clip_count"]
        return {
            "peak_dbfs": level["peak_dbfs"],
            "rms_dbfs": level["rms_dbfs"],
            "clip_count": after,
            "clip_count_before_limiter": before,
            "clip_count_after_limiter": after,
            "samples_out": level["samples"],
            "duration_s": level["samples"] / self.project.sample_rate,
            "channels": self.project.channels,
        }


def automation_breakpoints(curve: AutomationCurve | None) -> list[dict[str, float]]:
    return curve.breakpoints if curve is not None else []
