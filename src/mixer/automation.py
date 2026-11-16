"""Piecewise-linear automation with clamping, slew limiting and de-duplication.

Gain is evaluated per frame, not per block edge, so a 256-frame stream and a
1024-frame offline render hit the same automation value at the same frame — a
breakpoint falling mid-block would otherwise be smoothed differently by each
block size.

The slew limit is applied to the curve's own segments (slope limiting): the
piecewise-linear curve is rewritten so no segment rises or falls faster than
``max_slew_db_per_s``. That is deterministic, block-independent, and cheaper
than replaying an integrator, while still guaranteeing the maximum gain change
rate the requirement asks for.
"""

from __future__ import annotations

import math
from typing import Iterable, Sequence

import numpy as np

from .errors import MixerError
from .model import AutomationPoint

MIN_DB = -120.0
MAX_DB = 24.0


def dedupe_points(points: Iterable[AutomationPoint]) -> list[AutomationPoint]:
    """Sort by time; for repeated timestamps the LAST declaration wins."""
    latest: dict[float, AutomationPoint] = {}
    for point in points:
        latest[float(point.time)] = point
    return [latest[t] for t in sorted(latest)]


class AutomationCurve:
    """A time->dB curve with boundary clamping and a slope-limited variant."""

    __slots__ = ("_times", "_gains", "_default_db", "_rate", "_limited")

    def __init__(
        self,
        points: Sequence[AutomationPoint] = (),
        default_db: float = 0.0,
        max_slew_db_per_s: float | None = None,
        rate: int = 48000,
    ) -> None:
        cleaned = dedupe_points(points)
        for point in cleaned:
            if not math.isfinite(point.time) or not math.isfinite(point.gain_db):
                raise MixerError("invalid_automation", "automation points must be finite")
        if cleaned and min(p.time for p in cleaned) < 0.0:
            raise MixerError("invalid_automation", "automation time must be >= 0")
        if max_slew_db_per_s is not None:
            slew = abs(float(max_slew_db_per_s))
            if slew <= 0.0 or not math.isfinite(slew):
                raise MixerError("invalid_slew", "max slew must be a positive dB/s value")
        else:
            slew = None
        self._times = np.array([p.time for p in cleaned], dtype=np.float64)
        self._gains = np.clip(
            np.array([p.gain_db for p in cleaned], dtype=np.float64), MIN_DB, MAX_DB
        )
        self._default_db = float(np.clip(default_db, MIN_DB, MAX_DB))
        self._rate = int(rate)
        self._limited = self._slope_limit(slew) if slew is not None else None

    # -- construction -----------------------------------------------------
    @classmethod
    def from_track(cls, track, rate: int = 48000) -> "AutomationCurve":
        return cls(track.automation, track.gain_db, track.max_slew_db_per_s, rate)

    def _slope_limit(self, slew: float) -> tuple[np.ndarray, np.ndarray]:
        """Rewrite breakpoints so no segment exceeds ``slew`` dB/s."""
        if self._times.size == 0:
            return self._times, self._gains
        gains = np.empty_like(self._gains)
        gains[0] = self._gains[0]
        for i in range(1, self._gains.size):
            dt = float(self._times[i] - self._times[i - 1])
            travel = slew * dt
            target = self._gains[i]
            gains[i] = min(max(target, gains[i - 1] - travel), gains[i - 1] + travel)
        return self._times, gains

    # -- queries ----------------------------------------------------------
    @property
    def point_count(self) -> int:
        return int(self._times.size)

    @property
    def duration(self) -> float:
        return float(self._times[-1]) if self._times.size else 0.0

    @property
    def is_empty(self) -> bool:
        return self._times.size == 0

    @property
    def is_slew_limited(self) -> bool:
        return self._limited is not None

    def points(self, limited: bool = False) -> list[AutomationPoint]:
        times, gains = self._limited if (limited and self._limited) else (self._times, self._gains)
        return [AutomationPoint(float(t), float(g)) for t, g in zip(times, gains)]

    def value_at(self, time_s: np.ndarray | float) -> np.ndarray | float:
        """Linear interpolation between breakpoints, clamped outside them."""
        times, gains = self._limited if self._limited else (self._times, self._gains)
        samples = np.asarray(time_s, dtype=np.float64)
        if times.size == 0:
            if samples.ndim == 0:
                return self._default_db
            return np.full(samples.shape, self._default_db)
        result = np.interp(samples, times, gains)
        return float(result) if samples.ndim == 0 else result

    # -- sampling ---------------------------------------------------------
    def sample_db(self, frames: int, rate: int, start_frame: int = 0) -> np.ndarray:
        """Per-frame dB values for ``frames`` frames from ``start_frame``."""
        if frames <= 0:
            return np.zeros(0, dtype=np.float64)
        indexes = np.arange(start_frame, start_frame + frames, dtype=np.float64)
        return np.asarray(self.value_at(indexes / float(rate)), dtype=np.float64)

    def sample_edges(self, boundaries: Iterable[int], rate: int) -> list[float]:
        """dB at each block start frame (block-edge sampling report)."""
        return [float(self.value_at(frame / float(rate))) for frame in boundaries]

    def check_continuity(self, boundaries: Iterable[int], rate: int) -> list[float]:
        """Measured dB jump across each block boundary (should stay tiny)."""
        result = []
        for frame in boundaries:
            before = float(self.value_at(max(0, frame - 1) / rate))
            after = float(self.value_at(frame / rate))
            result.append(abs(after - before))
        return result

    def max_segment_slope(self) -> float:
        """Steepest dB/s slope in the curve (post-limiting, when enabled)."""
        times, gains = self._limited if self._limited else (self._times, self._gains)
        if times.size < 2:
            return 0.0
        dt = np.diff(times)
        dg = np.abs(np.diff(gains))
        mask = dt > 0
        if not mask.any():
            return 0.0
        return float(np.max(dg[mask] / dt[mask]))


class GainEvaluator:
    """Per-track gain evaluator shared by the offline and streaming engines.

    Gain (dB) = track static gain + automation curve. ``linear_gains`` returns
    the per-frame linear gain for the next block; ``advance`` moves the cursor
    across a silent gap without mixing so both paths stay frame-aligned.
    """

    def __init__(self, track, rate: int, block_size: int) -> None:
        self.track = track
        self.name = track.name
        self.rate = rate
        self.block_size = block_size
        self.curve = AutomationCurve.from_track(track, rate)
        self.reset()

    def reset(self) -> None:
        self._cursor = 0
        self._max_delta = 0.0
        self._evaluations = 0
        self._block_edges = 0
        self._last_db = float(self.curve.value_at(0.0))

    @property
    def cursor(self) -> int:
        return self._cursor

    @property
    def current_db(self) -> float:
        return self._last_db

    @property
    def max_delta_db(self) -> float:
        return self._max_delta

    @property
    def evaluations(self) -> int:
        return self._evaluations

    @property
    def block_edges(self) -> int:
        return self._block_edges

    def linear_gains(self, frames: int) -> np.ndarray:
        """Per-frame linear gain for the next ``frames`` frames from the cursor."""
        db = self.curve.sample_db(frames, self.rate, self._cursor)
        if db.size:
            previous = self._last_db
            steps = np.abs(np.diff(np.concatenate(([previous], db))))
            self._max_delta = max(self._max_delta, float(steps.max()))
            self._last_db = float(db[-1])
        self._evaluations += frames
        self._block_edges += 2
        self._cursor += frames
        return np.power(10.0, db / 20.0)

    def advance(self, frames: int) -> None:
        """Skip ``frames`` frames without mixing (silence), keeping the cursor true."""
        if frames <= 0:
            return
        db = self.curve.sample_db(frames, self.rate, self._cursor)
        if db.size:
            self._last_db = float(db[-1])
        self._cursor += frames

    def gain_at(self, time_s: float) -> float:
        return float(10.0 ** (float(self.curve.value_at(time_s)) / 20.0))

    def block_edge_db(self, frames: int) -> tuple[float, float]:
        """The block's start and end dB values (continuity reporting)."""
        return self._last_db, float(self.curve.value_at((self._cursor + frames) / self.rate))
