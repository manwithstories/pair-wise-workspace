"""Automation curve parsing and sample-accurate gain evaluation."""

from __future__ import annotations

from dataclasses import dataclass
import math
from typing import Any, Iterable

import numpy as np

from .errors import ManifestError


CONTROL_RATE = 1000.0
KINDS = {"linear", "exponential", "hold"}


def _finite(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ManifestError(f"{field} must be a finite number", details={"field": field})
    number = float(value)
    if not math.isfinite(number):
        raise ManifestError(f"{field} must be finite", details={"field": field, "value": str(value)})
    return number


def _db(value: Any, field: str) -> float:
    number = _finite(value, field)
    if number < -240.0 or number > 120.0:
        raise ManifestError(
            f"{field} is outside the supported dB range [-240, 120]",
            details={"field": field, "value": number},
        )
    return number


@dataclass(frozen=True)
class Segment:
    start: float
    end: float
    kind: str
    from_db: float
    to_db: float

    def value(self, times: np.ndarray) -> np.ndarray:
        span = self.end - self.start
        unit = np.clip((times - self.start) / span, 0.0, 1.0)
        if self.kind == "hold":
            return np.full(times.shape, self.from_db, dtype=np.float64)
        if self.kind == "linear":
            return self.from_db + (self.to_db - self.from_db) * unit
        low = 10.0 ** (self.from_db / 20.0)
        high = 10.0 ** (self.to_db / 20.0)
        if low <= 0.0 or high <= 0.0:
            # The supported dB floor is above zero, but keep this robust.
            return self.from_db + (self.to_db - self.from_db) * unit
        return 20.0 * np.log10(low * np.power(high / low, unit))


def _point_value(point: dict[str, Any], field: str) -> tuple[float, float]:
    if not isinstance(point, dict):
        raise ManifestError(f"{field} points must be objects")
    time = _finite(point.get("t", point.get("time")), f"{field}.t")
    gain = _db(point.get("gain_db", point.get("db")), f"{field}.gain_db")
    if time < 0.0:
        raise ManifestError(f"{field}.t must be non-negative", details={"t": time})
    return time, gain


def _segments_from_item(item: Any, index: int) -> list[Segment]:
    field = f"automation[{index}]"
    if not isinstance(item, dict):
        raise ManifestError(f"{field} must be an object")
    kind = str(item.get("type", "linear")).lower()
    if kind not in KINDS:
        raise ManifestError(
            f"unknown automation type {kind!r}",
            details={"field": f"{field}.type", "allowed": sorted(KINDS)},
        )
    if "points" in item:
        points = item["points"]
        if not isinstance(points, list) or len(points) < 2:
            raise ManifestError(f"{field}.points must contain at least two points")
        parsed = [_point_value(point, field) for point in points]
        parsed.sort(key=lambda pair: pair[0])
        if any(a[0] >= b[0] for a, b in zip(parsed, parsed[1:])):
            raise ManifestError(f"{field}.points times must be strictly increasing")
        if kind == "hold":
            return [Segment(parsed[0][0], parsed[-1][0], "hold", parsed[0][1], parsed[0][1])]
        return [
            Segment(left[0], right[0], kind, left[1], right[1])
            for left, right in zip(parsed, parsed[1:])
        ]
    start = _finite(item.get("start"), f"{field}.start")
    end = _finite(item.get("end"), f"{field}.end")
    if start < 0.0 or end <= start:
        raise ManifestError(
            f"{field} requires 0 <= start < end",
            details={"start": start, "end": end},
        )
    from_db = _db(item.get("from_db"), f"{field}.from_db")
    to_db = _db(item.get("to_db", from_db if kind == "hold" else None), f"{field}.to_db")
    return [Segment(start, end, kind, from_db, to_db)]


@dataclass(frozen=True)
class AutomationCurve:
    segments: tuple[Segment, ...]
    duration_s: float
    control_rate: float = CONTROL_RATE

    @classmethod
    def parse(cls, raw: Any, duration_s: float) -> "AutomationCurve | None":
        if raw is None:
            return None
        if not isinstance(raw, list) or not raw:
            raise ManifestError("automation must be a non-empty list", details={"field": "automation"})
        segments: list[Segment] = []
        for index, item in enumerate(raw):
            segments.extend(_segments_from_item(item, index))
        segments.sort(key=lambda segment: (segment.start, segment.end))
        for left, right in zip(segments, segments[1:]):
            if right.start < left.end:
                raise ManifestError(
                    "automation segments overlap",
                    details={"left_end": left.end, "right_start": right.start},
                )
        return cls(tuple(segments), float(duration_s), CONTROL_RATE)

    @property
    def points(self) -> int:
        if not self.segments or self.duration_s <= 0.0:
            return 0
        return int(math.ceil(self.duration_s * self.control_rate))

    @property
    def breakpoints(self) -> list[dict[str, float]]:
        result: list[dict[str, float]] = []
        for segment in self.segments:
            for time, gain in ((segment.start, segment.from_db), (segment.end, segment.to_db)):
                if result and math.isclose(result[-1]["t"], time, abs_tol=1e-12):
                    result[-1]["gain_db"] = gain
                else:
                    result.append({"t": time, "gain_db": gain})
        return result

    def values_at(self, times: Iterable[float] | np.ndarray) -> np.ndarray:
        sample = np.asarray(times, dtype=np.float64)
        sample = np.atleast_1d(sample)
        if not self.segments:
            return np.zeros_like(sample)
        starts = np.asarray([segment.start for segment in self.segments], dtype=np.float64)
        chosen = np.searchsorted(starts, sample, side="right") - 1
        values = np.empty_like(sample)
        values[chosen < 0] = self.segments[0].from_db
        for index, segment in enumerate(self.segments):
            mask = chosen == index
            inside = mask & (sample <= segment.end)
            outside = mask & (sample > segment.end)
            if np.any(inside):
                values[inside] = segment.value(sample[inside])
            if np.any(outside):
                values[outside] = segment.to_db
        return values

    def gain_db_at_samples(
        self, sample_indices: np.ndarray, sample_rate: int, static_db: float = 0.0
    ) -> np.ndarray:
        samples = np.asarray(sample_indices, dtype=np.float64)
        control_position = (samples / float(sample_rate)) * self.control_rate
        lower_index = np.floor(control_position).astype(np.int64)
        fraction = control_position - lower_index
        lower_time = lower_index.astype(np.float64) / self.control_rate
        upper_time = (lower_index + 1).astype(np.float64) / self.control_rate
        lower = self.values_at(lower_time)
        upper = self.values_at(upper_time)
        return lower + (upper - lower) * fraction + static_db


def db_to_gain(gain_db: np.ndarray | float) -> np.ndarray:
    return np.power(10.0, np.asarray(gain_db, dtype=np.float32) / 20.0, dtype=np.float32)
