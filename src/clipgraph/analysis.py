"""Streaming audio measurements: peak, windowed RMS, clipping, and true peak."""

from __future__ import annotations

import math
from dataclasses import dataclass, field

import numpy as np

from .io import AudioIOError
from .model import VALID_RATES

_FLOOR = 1.0e-10


def amplitude_to_db(value: float) -> float:
    return 20.0 * math.log10(max(float(value), _FLOOR))


def _catmull_rom(p0: np.ndarray, p1: np.ndarray, p2: np.ndarray, p3: np.ndarray, t: float) -> np.ndarray:
    t2 = t * t
    t3 = t2 * t
    return 0.5 * (
        2.0 * p1
        + (-p0 + p2) * t
        + (2.0 * p0 - 5.0 * p1 + 4.0 * p2 - p3) * t2
        + (-p0 + 3.0 * p1 - 3.0 * p2 + p3) * t3
    )


@dataclass
class MetricsAccumulator:
    sample_rate: int
    window_ms: float = 400.0
    channels: int = 0
    peak: float = 0.0
    true_peak: float = 0.0
    frames: int = 0
    sum_sq: float = 0.0
    clip_samples: int = 0
    clip_runs: int = 0
    _clip_active: bool = False
    _tp_history: np.ndarray | None = field(default=None, repr=False)
    _window_sum: float = 0.0
    _window_frames: int = 0
    window_rms_db: list[float] = field(default_factory=list)

    def __post_init__(self) -> None:
        self.window_frames = max(1, int(round(self.sample_rate * self.window_ms / 1000.0)))

    def _true_peak_segments(self, block: np.ndarray) -> None:
        if block.size == 0:
            return
        if self._tp_history is None:
            self._tp_history = np.zeros((3, block.shape[1]), dtype=np.float64)
        combined = np.concatenate((self._tp_history, block.astype(np.float64, copy=False)), axis=0)
        if len(combined) >= 4:
            p0, p1, p2, p3 = combined[:-3], combined[1:-2], combined[2:-1], combined[3:]
            for fraction in (0.25, 0.5, 0.75):
                interpolated = _catmull_rom(p0, p1, p2, p3, fraction)
                self.true_peak = max(self.true_peak, float(np.max(np.abs(interpolated))))
        self._tp_history = combined[-3:].copy()

    def add(self, signal: np.ndarray) -> None:
        data = np.asarray(signal, dtype=np.float64)
        if data.ndim == 1:
            data = data[:, None]
        if data.shape[0] == 0:
            return
        if self.channels == 0:
            self.channels = data.shape[1]
        if data.shape[1] != self.channels:
            raise ValueError(f"channel count changed from {self.channels} to {data.shape[1]}")
        if not np.all(np.isfinite(data)):
            data = np.nan_to_num(data, nan=0.0, posinf=0.0, neginf=0.0)
        frames = data.shape[0]
        self.frames += frames
        self.sum_sq += float(np.sum(np.square(data, dtype=np.float64)))
        self.peak = max(self.peak, float(np.max(np.abs(data))))
        self.true_peak = max(self.true_peak, self.peak)
        clipping = np.abs(data) >= 1.0
        self.clip_samples += int(np.count_nonzero(clipping))
        frame_clip = np.any(clipping, axis=1)
        previous = np.concatenate((np.asarray([self._clip_active]), frame_clip[:-1]))
        self.clip_runs += int(np.count_nonzero(frame_clip & ~previous))
        self._clip_active = bool(frame_clip[-1])
        self._true_peak_segments(data)
        offset = 0
        while offset < frames:
            count = min(self.window_frames - self._window_frames, frames - offset)
            self._window_sum += float(np.sum(np.square(data[offset : offset + count], dtype=np.float64)))
            self._window_frames += count
            offset += count
            if self._window_frames == self.window_frames:
                self._close_window()

    def _close_window(self) -> None:
        rms = math.sqrt(self._window_sum / max(1, self._window_frames * self.channels))
        self.window_rms_db.append(amplitude_to_db(rms))
        self._window_sum = 0.0
        self._window_frames = 0

    def finish(self) -> None:
        if self._window_frames:
            self._close_window()
        if self._tp_history is not None:
            tail = np.zeros((3, self.channels), dtype=np.float64)
            combined = np.concatenate((self._tp_history, tail), axis=0)
            if len(combined) >= 4:
                p0, p1, p2, p3 = combined[:-3], combined[1:-2], combined[2:-1], combined[3:]
                for fraction in (0.25, 0.5, 0.75):
                    interpolated = _catmull_rom(p0, p1, p2, p3, fraction)
                    self.true_peak = max(self.true_peak, float(np.max(np.abs(interpolated))))
            self._tp_history = None

    def summary(self) -> dict[str, float | int]:
        rms = math.sqrt(self.sum_sq / max(1, self.frames * max(1, self.channels)))
        return {
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "frames": self.frames,
            "duration_sec": self.frames / self.sample_rate,
            "peak": self.peak,
            "peak_dbfs": amplitude_to_db(self.peak),
            "rms": rms,
            "rms_dbfs": amplitude_to_db(rms),
            "clip_samples": self.clip_samples,
            "clip_runs": self.clip_runs,
            "true_peak": self.true_peak,
            "true_peak_dbtp": amplitude_to_db(self.true_peak),
            "window_ms": self.window_ms,
            "oversample_factor": 4,
            "rms_windows_dbfs": list(self.window_rms_db),
        }


def analyze_array(signal: np.ndarray, sample_rate: int, window_ms: float = 400.0) -> dict:
    data = np.asarray(signal)
    if data.ndim == 1:
        data = data[:, None]
    accumulator = MetricsAccumulator(sample_rate, window_ms, data.shape[1])
    accumulator.add(data)
    accumulator.finish()
    return accumulator.summary()


def analyze_wav(path: str, window_ms: float = 400.0, block_size: int = 65536) -> dict:
    import soundfile as sf

    try:
        with sf.SoundFile(path, "r") as handle:
            if handle.samplerate not in VALID_RATES:
                raise AudioIOError(f"invalid sample rate {handle.samplerate} in {path}")
            accumulator = MetricsAccumulator(handle.samplerate, window_ms, handle.channels)
            while True:
                data = handle.read(block_size, dtype="float32", always_2d=True)
                if len(data) == 0:
                    break
                accumulator.add(data)
    except (OSError, RuntimeError) as exc:
        if isinstance(exc, AudioIOError):
            raise
        raise AudioIOError(f"cannot analyze audio {path}: {exc}") from exc
    accumulator.finish()
    return accumulator.summary()
