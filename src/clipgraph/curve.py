"""Piecewise automation curves shared by offline and streaming rendering."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .model import AutomationCurve, CurvePoint, GraphError

_MIN_GAIN = 1.0e-20


def value_to_db(value: float) -> float:
    """Convert a non-negative linear gain to dBFS."""
    if value <= 0.0:
        return -200.0
    return 20.0 * math.log10(value)


def db_to_gain(db: float) -> float:
    return float(10.0 ** (db / 20.0))


def _point_db(curve: AutomationCurve, value: float) -> float:
    return value if curve.domain == "db" else value_to_db(value)


def _interpolate(curve: AutomationCurve, first: CurvePoint, second: CurvePoint, fraction: np.ndarray) -> np.ndarray:
    if first.mode == "hold":
        return np.full_like(fraction, first.value, dtype=np.float64)
    if first.mode == "cosine":
        fraction = 0.5 - 0.5 * np.cos(np.pi * fraction)
    return first.value + (second.value - first.value) * fraction


def sample_curve(curve: AutomationCurve, times: np.ndarray | float) -> np.ndarray:
    """Sample one curve in its native domain (dB or linear)."""
    scalar = np.isscalar(times)
    t = np.atleast_1d(np.asarray(times, dtype=np.float64))
    if not np.all(np.isfinite(t)):
        raise GraphError("curve sample times must be finite", f"curve {curve.curve_id}")
    x = np.asarray([p.time for p in curve.points], dtype=np.float64)
    y = np.asarray([p.value for p in curve.points], dtype=np.float64)
    if t.size == 0:
        result = np.empty(0, dtype=np.float64)
        return float(result[0]) if scalar else result
    result = np.empty_like(t)
    result[t <= x[0]] = y[0]
    result[t >= x[-1]] = y[-1]
    active = (t > x[0]) & (t < x[-1])
    if np.any(active):
        pos = np.searchsorted(x, t[active], side="right") - 1
        frac = (t[active] - x[pos]) / (x[pos + 1] - x[pos])
        # Each active point can belong to a different segment; evaluate by segment.
        for segment in np.unique(pos):
            mask = pos == segment
            result[np.flatnonzero(active)[mask]] = _interpolate(
                curve, curve.points[int(segment)], curve.points[int(segment) + 1], frac[mask]
            )
    return float(result[0]) if scalar else result


def sample_curves_db(curves: tuple[AutomationCurve, ...], times: np.ndarray | float) -> np.ndarray:
    """Additively combine every curve attached to one node, in dB."""
    scalar = np.isscalar(times)
    t = np.atleast_1d(np.asarray(times, dtype=np.float64))
    total = np.zeros_like(t)
    for curve in curves:
        values = sample_curve(curve, t)
        total += np.asarray([_point_db(curve, float(v)) for v in np.atleast_1d(values)])
    return float(total[0]) if scalar else total


def gain_at_db(curves: tuple[AutomationCurve, ...], times: np.ndarray | float, gain_db: float = 0.0) -> np.ndarray:
    """Return linear gain for combined automation plus a static node gain."""
    scalar = np.isscalar(times)
    combined = sample_curves_db(curves, times)
    values = np.clip(np.atleast_1d(combined) + float(gain_db), -300.0, 300.0)
    gains = np.power(10.0, values / 20.0)
    gains = np.maximum(gains, _MIN_GAIN)
    return float(gains[0]) if scalar else gains


def frame_times(start_frame: int, end_frame: int, sample_rate: int) -> np.ndarray:
    if end_frame < start_frame:
        raise GraphError("end frame precedes start frame", "curve range")
    frames = np.arange(start_frame, end_frame, dtype=np.int64)
    return frames.astype(np.float64) / float(sample_rate)


def sample_range(
    curves: tuple[AutomationCurve, ...],
    start_frame: int,
    end_frame: int,
    sample_rate: int,
    step_frames: int = 1,
    block_boundaries: tuple[int, ...] = (),
) -> tuple[np.ndarray, np.ndarray]:
    """Sample curves at a fixed frame stride and/or explicit block boundaries."""
    if step_frames < 1:
        raise GraphError("step_frames must be positive", "curve range")
    frames = np.arange(start_frame, end_frame, step_frames, dtype=np.int64)
    if block_boundaries:
        frames = np.unique(np.concatenate((frames, np.asarray(block_boundaries, dtype=np.int64))))
        frames = frames[(frames >= start_frame) & (frames <= end_frame)]
    times = frames.astype(np.float64) / float(sample_rate)
    return times, sample_curves_db(curves, times)


@dataclass(frozen=True)
class CurveStats:
    curve_id: str
    samples: int
    min_db: float
    max_db: float
    first_value: float

    def to_dict(self) -> dict[str, float | int | str]:
        return {
            "curve_id": self.curve_id,
            "samples": self.samples,
            "min_db": self.min_db,
            "max_db": self.max_db,
            "first_value": self.first_value,
        }


def curve_stats(curve: AutomationCurve, duration: float, sample_rate: int) -> CurveStats:
    samples = max(1, int(round(duration * sample_rate)))
    if samples > 1_000_000:
        times = np.linspace(0.0, duration, samples, endpoint=False)
    else:
        times = np.arange(samples, dtype=np.float64) / sample_rate
    values = np.atleast_1d(sample_curve(curve, times)).astype(np.float64)
    db_values = values if curve.domain == "db" else np.asarray([value_to_db(float(v)) for v in values])
    return CurveStats(curve.curve_id, samples, float(np.min(db_values)), float(np.max(db_values)), float(db_values[0]))
