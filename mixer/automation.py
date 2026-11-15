"""Automation-curve parsing and absolute-time interpolation."""

from __future__ import annotations

import csv
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Sequence

import numpy as np


class AutomationError(ValueError):
    """Raised when an automation CSV is malformed."""


@dataclass(frozen=True)
class AutomationCurve:
    """A piecewise-linear gain curve evaluated in absolute seconds."""

    times: np.ndarray
    gains_db: np.ndarray

    def __post_init__(self) -> None:
        times = np.asarray(self.times, dtype=np.float64).reshape(-1)
        gains = np.asarray(self.gains_db, dtype=np.float64).reshape(-1)
        if times.size != gains.size:
            raise AutomationError("automation time and gain arrays differ in length")
        if times.size and not np.all(np.isfinite(times)):
            raise AutomationError("automation time contains NaN or Inf")
        if not np.all(np.isfinite(gains)):
            raise AutomationError("automation gain contains NaN or Inf")
        if times.size and np.any(times < 0):
            raise AutomationError("automation time must be non-negative")
        if times.size > 1 and np.any(np.diff(times) <= 0):
            raise AutomationError("automation time must be strictly increasing")
        object.__setattr__(self, "times", times)
        object.__setattr__(self, "gains_db", gains)

    @classmethod
    def empty(cls) -> "AutomationCurve":
        """Return a curve that represents a constant 0 dB gain."""

        return cls(np.empty(0, dtype=np.float64), np.empty(0, dtype=np.float64))

    @classmethod
    def from_points(cls, points: Iterable[Sequence[float]]) -> "AutomationCurve":
        rows = list(points)
        if not rows:
            return cls.empty()
        if any(len(row) != 2 for row in rows):
            raise AutomationError("automation points must contain time_sec,gain_db")
        array = np.asarray(rows, dtype=np.float64)
        return cls(array[:, 0], array[:, 1])

    @classmethod
    def from_csv(cls, path: str | Path) -> "AutomationCurve":
        path = Path(path)
        try:
            with path.open("r", encoding="utf-8-sig", newline="") as handle:
                reader = csv.DictReader(handle, skipinitialspace=True)
                if reader.fieldnames is None:
                    return cls.empty()
                fields = {name.strip(): name for name in reader.fieldnames if name}
                if "time_sec" not in fields or "gain_db" not in fields:
                    raise AutomationError(
                        "automation CSV must contain time_sec,gain_db headers"
                    )
                times: list[float] = []
                gains: list[float] = []
                for line_number, row in enumerate(reader, start=2):
                    raw_time = (row.get(fields["time_sec"]) or "").strip()
                    raw_gain = (row.get(fields["gain_db"]) or "").strip()
                    if not raw_time and not raw_gain:
                        continue
                    try:
                        time_sec = float(raw_time)
                        gain_db = float(raw_gain)
                    except ValueError as exc:
                        raise AutomationError(
                            f"invalid numeric automation value on line {line_number}"
                        ) from exc
                    if not math.isfinite(time_sec) or not math.isfinite(gain_db):
                        raise AutomationError(
                            f"non-finite automation value on line {line_number}"
                        )
                    times.append(time_sec)
                    gains.append(gain_db)
        except FileNotFoundError as exc:
            raise AutomationError(f"automation file not found: {path}") from exc
        except OSError as exc:
            raise AutomationError(f"cannot read automation file {path}: {exc}") from exc
        return cls(np.asarray(times), np.asarray(gains))

    def __len__(self) -> int:
        return int(self.times.size)

    def evaluate(self, time_sec: float | np.ndarray) -> float | np.ndarray:
        """Linearly interpolate gain in dB, holding the endpoint values."""

        scalar = np.isscalar(time_sec)
        query = np.asarray(time_sec, dtype=np.float64)
        if self.times.size == 0:
            result = np.zeros_like(query, dtype=np.float64)
        elif self.times.size == 1:
            result = np.full_like(query, float(self.gains_db[0]), dtype=np.float64)
        else:
            result = np.interp(query, self.times, self.gains_db)
        return float(result) if scalar else result

    def __call__(self, time_sec: float | np.ndarray) -> float | np.ndarray:
        return self.evaluate(time_sec)


def load_automation(path: str | Path | None) -> AutomationCurve:
    """Load a curve path, treating ``None`` as a flat 0 dB curve."""

    return AutomationCurve.empty() if path is None else AutomationCurve.from_csv(path)


parse_automation = AutomationCurve.from_csv
load_curve = load_automation
Curve = AutomationCurve


def combine_curves(curves: Iterable[AutomationCurve]) -> AutomationCurve:
    """Return the dB-domain sum of several curves on their combined knots."""

    curve_list = list(curves)
    if not curve_list:
        return AutomationCurve.empty()
    knots = np.unique(np.concatenate([curve.times for curve in curve_list]))
    if knots.size == 0:
        return AutomationCurve.empty()
    gains = np.zeros_like(knots)
    for curve in curve_list:
        gains += np.asarray(curve.evaluate(knots), dtype=np.float64)
    return AutomationCurve(knots, gains)


def automation_times(duration_sec: float, step_sec: float) -> np.ndarray:
    """Build inclusive, step-aligned sample times for reporting."""

    if duration_sec < 0:
        raise AutomationError("duration must be non-negative")
    if step_sec <= 0 or not math.isfinite(step_sec):
        raise AutomationError("automation step must be positive and finite")
    count = int(math.floor(duration_sec / step_sec + 1e-9)) + 1
    return np.arange(count, dtype=np.float64) * step_sec
