"""Breakpoint automation evaluated at audio sample resolution."""

from __future__ import annotations

from collections.abc import Iterable, Sequence
import math

import numpy as np

from .errors import ManifestError
from .util import db_to_linear, require_finite


class AutomationCurve:
    """A piecewise gain curve defined in dB at arbitrary times in seconds.

    Breakpoints are sorted at construction. Evaluation happens at each sample
    time, so a breakpoint between two samples produces a genuine ramp instead
    of a step that could click. Exponential mode interpolates linear amplitude
    geometrically and rejects points that resolve to a non-positive gain.
    """

    def __init__(
        self,
        points: Iterable[Sequence[object] | dict] = (),
        interpolation: str = "linear",
        field: str = "automation",
    ) -> None:
        self.field = field
        self.interpolation = interpolation.lower()
        if self.interpolation not in {"linear", "exponential"}:
            raise ManifestError(
                "interpolation must be 'linear' or 'exponential'",
                f"{field}.interpolation",
            )
        parsed: list[tuple[float, float]] = []
        for index, raw in enumerate(points):
            parsed.append(self._parse_point(raw, f"{field}.points[{index}]"))
        parsed.sort(key=lambda item: item[0])
        for previous, current in zip(parsed, parsed[1:]):
            if current[0] == previous[0]:
                raise ManifestError(
                    f"duplicate breakpoint time {current[0]!r}",
                    f"{field}.points",
                )
        self._times = np.asarray([item[0] for item in parsed], dtype=np.float64)
        self._db = np.asarray([item[1] for item in parsed], dtype=np.float64)
        if self.interpolation == "exponential" and len(self._db):
            gains = np.asarray(db_to_linear(self._db), dtype=np.float64)
            if np.any(~np.isfinite(gains)) or np.any(gains <= 0.0):
                raise ManifestError(
                    "exponential interpolation requires positive gain",
                    f"{field}.points",
                )

    def _parse_point(self, raw, field: str) -> tuple[float, float]:
        if isinstance(raw, dict):
            if "time" not in raw:
                raise ManifestError("breakpoint is missing 'time'", field)
            if "db" in raw:
                db = raw["db"]
            elif "gain_db" in raw:
                db = raw["gain_db"]
            elif "gain" in raw:
                gain = require_finite("gain", raw["gain"], field=f"{field}.gain")
                if self.interpolation == "exponential" and gain <= 0.0:
                    raise ManifestError(
                        "exponential interpolation requires positive gain",
                        f"{field}.gain",
                    )
                db = 20.0 * math.log10(gain) if gain > 0.0 else -np.inf
            else:
                raise ManifestError(
                    "breakpoint needs 'db', 'gain_db', or 'gain'", field
                )
            time_value = raw["time"]
        else:
            try:
                time_value, db = raw
            except (TypeError, ValueError) as exc:
                raise ManifestError(
                    "breakpoint must be [time_seconds, gain_db]", field
                ) from exc
        time_seconds = require_finite("time", time_value, field=f"{field}.time")
        if time_seconds < 0.0:
            raise ManifestError("time must be non-negative", f"{field}.time")
        gain_db = require_finite("gain_db", db, field=f"{field}.db")
        return time_seconds, gain_db

    @property
    def breakpoints(self) -> np.ndarray:
        if not len(self._times):
            return np.empty((0, 2), dtype=np.float64)
        return np.column_stack((self._times, self._db))

    def sample_db(self, times: np.ndarray | float) -> np.ndarray:
        """Evaluate the curve at times in seconds, returning dB values."""
        points = np.asarray(times, dtype=np.float64)
        if not len(self._times):
            return np.zeros_like(points)
        if self.interpolation == "linear" or len(self._times) == 1:
            return np.interp(points, self._times, self._db)

        gains = np.asarray(db_to_linear(self._db), dtype=np.float64)
        segment = np.searchsorted(self._times, points, side="right") - 1
        segment = np.clip(segment, 0, len(self._times) - 2)
        t0 = self._times[segment]
        t1 = self._times[segment + 1]
        fraction = np.clip((points - t0) / (t1 - t0), 0.0, 1.0)
        log_gain = np.log(gains[segment]) + fraction * (
            np.log(gains[segment + 1]) - np.log(gains[segment])
        )
        return 20.0 * log_gain / np.log(10.0)

    def sample_frames(self, start: int, count: int, sample_rate: int) -> np.ndarray:
        if count < 0:
            raise ValueError("count must be non-negative")
        frames = np.arange(start, start + count, dtype=np.float64)
        return self.sample_db(frames / float(sample_rate))

    def gain_db_at_frames(
        self, start: int, count: int, sample_rate: int, base_db: float = 0.0
    ) -> np.ndarray:
        return base_db + self.sample_frames(start, count, sample_rate)

    def gain_linear_at_frames(
        self, start: int, count: int, sample_rate: int, base_db: float = 0.0
    ) -> np.ndarray:
        return np.asarray(
            db_to_linear(self.gain_db_at_frames(start, count, sample_rate, base_db)),
            dtype=np.float64,
        )

    def block_boundary_db(
        self,
        total_frames: int,
        block_size: int,
        sample_rate: int,
        base_db: float = 0.0,
    ) -> list[float]:
        starts = np.arange(0, total_frames, block_size, dtype=np.float64)
        values = base_db + self.sample_db(starts / float(sample_rate))
        return [round(float(value), 6) for value in values]


def automation_from_manifest(value, field: str = "automation") -> AutomationCurve:
    """Parse the compact manifest form into an :class:`AutomationCurve`."""
    if value is None:
        return AutomationCurve((), field=field)
    if isinstance(value, list):
        return AutomationCurve(value, field=field)
    if not isinstance(value, dict):
        raise ManifestError("automation must be a mapping or point list", field)
    points = value.get("points", [])
    interpolation = value.get("interpolation", "linear")
    return AutomationCurve(points, interpolation=interpolation, field=field)

