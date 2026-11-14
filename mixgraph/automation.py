"""Gain automation curves with sample-accurate evaluation."""
from __future__ import annotations

from dataclasses import dataclass
import math
from typing import Any, Iterable

import numpy as np

_EPS = 1e-12
_CURVES = {"linear", "hold", "exponential"}


class AutomationError(ValueError):
    """Raised when an automation curve is malformed."""


def db_to_linear(db: np.ndarray | float) -> np.ndarray | float:
    return np.power(10.0, np.asarray(db, dtype=np.float64) / 20.0)


def linear_to_db(value: np.ndarray | float, floor_db: float = -120.0) -> np.ndarray | float:
    magnitude = np.maximum(np.abs(np.asarray(value, dtype=np.float64)), 1e-12)
    return np.maximum(20.0 * np.log10(magnitude), floor_db)


@dataclass(frozen=True)
class AutomationPoint:
    time: float
    gain_db: float
    curve: str = "linear"


class Automation:
    """A sequence of time/dB points.

    A point's curve describes the interval to the following point.  ``linear``
    interpolates linear amplitude, while ``exponential`` interpolates dB.  The
    value is held before the first and after the last point.
    """

    def __init__(self, points: Iterable[AutomationPoint]):
        self.points = tuple(points)
        if not self.points:
            raise AutomationError("automation curve has no breakpoints")

    @classmethod
    def from_json(cls, raw: Any, node: str = "<unknown>") -> "Automation":
        if raw is None:
            return cls(())
        if not isinstance(raw, dict):
            raise AutomationError(f"automation for node '{node}' must be an object")
        raw_points = raw.get("points")
        if not isinstance(raw_points, list) or not raw_points:
            raise AutomationError(f"automation for node '{node}' has no breakpoints")
        points: list[AutomationPoint] = []
        previous: float | None = None
        for index, item in enumerate(raw_points):
            if not isinstance(item, dict):
                raise AutomationError(
                    f"automation point {index} for node '{node}' must be an object"
                )
            try:
                time = float(item["time"])
                gain = float(item.get("gain_db", item.get("db")))
            except (KeyError, TypeError, ValueError) as exc:
                raise AutomationError(
                    f"automation point {index} for node '{node}' needs time and gain_db"
                ) from exc
            curve = str(item.get("curve", "linear")).lower()
            if curve not in _CURVES:
                raise AutomationError(
                    f"automation point {index} for node '{node}' has unsupported curve '{curve}'"
                )
            if not math.isfinite(time) or not math.isfinite(gain):
                raise AutomationError(
                    f"automation point {index} for node '{node}' must be finite"
                )
            if time < 0.0:
                raise AutomationError(
                    f"automation point {index} for node '{node}' has negative time"
                )
            if previous is not None and time <= previous:
                raise AutomationError(
                    f"automation points for node '{node}' must be strictly increasing"
                )
            previous = time
            points.append(AutomationPoint(time, gain, curve))
        return cls(points)

    def _values(self, times: np.ndarray, amplitude: bool) -> np.ndarray:
        points = self.points
        if len(points) == 1:
            value = db_to_linear(points[0].gain_db) if amplitude else points[0].gain_db
            return np.full(times.shape, value, dtype=np.float64)

        breakpoints = np.asarray([p.time for p in points], dtype=np.float64)
        segment = np.searchsorted(breakpoints, times, side="right") - 1
        result = np.empty(times.shape, dtype=np.float64)

        before = segment < 0
        after = segment >= len(points) - 1
        middle = ~(before | after)
        result[before] = (
            db_to_linear(points[0].gain_db) if amplitude else points[0].gain_db
        )
        result[after] = (
            db_to_linear(points[-1].gain_db) if amplitude else points[-1].gain_db
        )
        for index in np.unique(segment[middle]):
            mask = middle & (segment == index)
            start = points[index]
            end = points[index + 1]
            fraction = np.clip(
                (times[mask] - start.time) / (end.time - start.time), 0.0, 1.0
            )
            if start.curve == "hold":
                value = (
                    db_to_linear(start.gain_db) if amplitude else start.gain_db
                )
                result[mask] = value
            elif start.curve == "linear" and amplitude:
                a0 = float(db_to_linear(start.gain_db))
                a1 = float(db_to_linear(end.gain_db))
                result[mask] = a0 + (a1 - a0) * fraction
            elif start.curve == "linear":
                a0 = float(db_to_linear(start.gain_db))
                a1 = float(db_to_linear(end.gain_db))
                interpolated = a0 + (a1 - a0) * fraction
                result[mask] = 20.0 * np.log10(np.maximum(interpolated, _EPS))
            else:
                interpolated_db = start.gain_db + (end.gain_db - start.gain_db) * fraction
                result[mask] = (
                    db_to_linear(interpolated_db) if amplitude else interpolated_db
                )
        return result

    def gain_at(self, times: np.ndarray | float) -> np.ndarray:
        """Return linear gain at seconds."""
        values = np.asarray(times, dtype=np.float64)
        return self._values(values, amplitude=True)

    def gain_db_at(self, times: np.ndarray | float) -> np.ndarray:
        """Return gain in dB at seconds."""
        values = np.asarray(times, dtype=np.float64)
        return self._values(values, amplitude=False)
