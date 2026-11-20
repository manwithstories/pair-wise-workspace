"""Unified rational time base, timestamp repair and A/V alignment.

A stream's native time base is kept as :class:`fractions.Fraction`, so every
conversion is exact.  Repairs never mutate the observed value: each record keeps
``raw``, ``fixed`` and the human-readable ``reason``.  When the source evidence
allows more than one plausible ordering the record is marked ``uncertain`` and
the interval it may occupy is recorded instead of inventing a value.
"""

from __future__ import annotations

import statistics
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable, Sequence

from .errors import MediaIssue

#: Default wrap period for 33-bit 90 kHz transport-style counters (2**33 ticks
#: is ~26.5 h; the 32-bit microsecond counter used by MPEG-2 PES is ~71 min).
DEFAULT_WRAP_PERIOD = 2**33
AUDIO_WRAP_PERIOD = 2**32


@dataclass(frozen=True)
class TimeBase:
    """Exact rational time base, e.g. ``1/90000``."""

    num: int = 1
    den: int = 90000

    def __post_init__(self) -> None:
        if self.num <= 0 or self.den <= 0:
            raise ValueError("time base must be positive")

    @classmethod
    def from_ratio(cls, value: Fraction | int | str) -> "TimeBase":
        frac = value if isinstance(value, Fraction) else Fraction(value)
        if frac <= 0:
            raise ValueError("time base must be positive")
        return cls(frac.numerator, frac.denominator)

    def seconds(self, ticks: int | Fraction) -> Fraction:
        return Fraction(ticks) * self.num / self.den

    def ticks(self, seconds: Fraction | float | int) -> int:
        value = Fraction(seconds) * self.den / self.num
        return _round_half_up(value)

    def rescale(self, ticks: int, target: "TimeBase") -> int:
        return _round_half_up(Fraction(ticks) * self.num * target.den / (self.den * target.num))

    @property
    def as_fraction(self) -> Fraction:
        return Fraction(self.num, self.den)

    def __str__(self) -> str:  # pragma: no cover - cosmetic
        return f"{self.num}/{self.den}"


def _round_half_up(value: Fraction) -> int:
    """Round a rational to the nearest integer, halves toward +infinity."""
    if value.denominator == 1:
        return value.numerator
    floor = value.numerator // value.denominator
    frac = value - floor
    return floor + (1 if frac >= Fraction(1, 2) else 0)


def _median_step(ticks: Sequence[int]) -> int:
    steps = [b - a for a, b in zip(ticks, ticks[1:]) if b != a]
    if not steps:
        return 1
    return max(1, int(statistics.median_low(steps)))


@dataclass
class StampDecision:
    """One timestamp after repair, with full provenance.

    ``raw`` is exactly what the container/bitstream delivered (``None`` when the
    value was absent).  ``fixed`` is the value used downstream.  ``reason`` names
    the repair that was applied (``"ok"`` when untouched).  ``uncertain`` marks a
    value inferred from ambiguous evidence; ``window_lo``/``window_hi`` then give
    the tick interval the true value may occupy.
    """

    raw: int | None
    fixed: int
    reason: str
    uncertain: bool = False
    window_lo: int | None = None
    window_hi: int | None = None
    duplicate_of: int | None = None
    note: str = ""

    def to_dict(self, time_base: TimeBase | None = None) -> dict[str, Any]:
        out: dict[str, Any] = {
            "raw": self.raw,
            "fixed": self.fixed,
            "reason": self.reason,
            "uncertain": self.uncertain,
        }
        if time_base is not None:
            out["fixed_seconds"] = float(time_base.seconds(self.fixed))
            if self.raw is not None:
                out["raw_seconds"] = float(time_base.seconds(self.raw))
        if self.window_lo is not None:
            out["window_lo"] = self.window_lo
            out["window_hi"] = self.window_hi
            if time_base is not None:
                out["window_lo_seconds"] = float(time_base.seconds(self.window_lo))
                out["window_hi_seconds"] = float(time_base.seconds(self.window_hi))
        if self.duplicate_of is not None:
            out["duplicate_of"] = self.duplicate_of
        if self.note:
            out["note"] = self.note
        return out


@dataclass
class TimelineRepair:
    """Result of mapping one stream's timestamps to a monotonic timeline."""

    time_base: TimeBase
    decisions: list[StampDecision]
    issues: list[MediaIssue] = field(default_factory=list)
    wrap_count: int = 0
    unit_ticks: int = 1

    @property
    def uncertain_count(self) -> int:
        return sum(1 for d in self.decisions if d.uncertain)

    def fixed_ticks(self) -> list[int]:
        return [d.fixed for d in self.decisions]

    def fixed_seconds(self) -> list[Fraction]:
        return [self.time_base.seconds(d.fixed) for d in self.decisions]

    def summary(self) -> dict[str, Any]:
        counts: dict[str, int] = {}
        for d in self.decisions:
            counts[d.reason] = counts.get(d.reason, 0) + 1
        return {
            "records": len(self.decisions),
            "wrap_count": self.wrap_count,
            "uncertain": self.uncertain_count,
            "repairs": counts,
            "unit_ticks": self.unit_ticks,
            "issues": [i.to_dict() for i in self.issues],
        }


def _unwrap(raw: Sequence[int | None], period: int) -> tuple[list[int | None], list[bool], int]:
    """Track wraps of a fixed-width counter.

    Returns the epoch-extended values, a per-item flag telling whether the value
    crossed a wrap boundary, and the number of wrap events.  Small backward
    jitter is left untouched so it can be classified as reordering later.
    """
    out: list[int | None] = []
    wrapped: list[bool] = []
    wraps = 0
    epochs = 0
    previous: int | None = None
    span = period // 2
    for value in raw:
        if value is None:
            out.append(None)
            wrapped.append(False)
            continue
        v = value + period if value < 0 else value
        crossed = False
        if previous is not None:
            delta = v - previous
            if delta < -span:
                epochs += 1
                wraps += 1
                crossed = True
            elif delta > span:
                epochs -= 1
        out.append(v + epochs * period)
        wrapped.append(crossed)
        previous = v
    return out, wrapped, wraps


def _reorder_pass(
    values: Sequence[int | None], window: int, limit: int,
) -> tuple[list[int | None], list[str]]:
    """Stable-sort short disorder runs back into value order.

    A run is a maximal span of values that sit at or below the running maximum
    and stay within ``window`` ticks of it, at most ``limit`` entries long.
    Sorting only such short, bounded runs cannot reorder genuinely distant
    discontinuities, which are reported as uncertain instead.
    """
    out = list(values)
    reasons = ["ok"] * len(values)
    n = len(values)
    i = 0
    while i < n:
        if values[i] is None:
            i += 1
            continue
        run_positions = [i]
        run_values = [values[i]]
        max_value = values[i]
        min_value = values[i]
        j = i + 1
        while j < n and len(run_positions) < limit:
            value = values[j]
            if value is None:
                j += 1
                continue
            candidate_max = max(max_value, value)
            candidate_min = min(min_value, value)
            if value <= max_value and candidate_max - candidate_min <= window:
                run_positions.append(j)
                run_values.append(value)
                min_value = candidate_min
                j += 1
                continue
            break
        if len(run_positions) > 1:
            ordered = sorted(run_values)
            for position, value in zip(run_positions, ordered):
                if out[position] != value:
                    out[position] = value
                    reasons[position] = "reordered"
            i = j
        else:
            i += 1
    return out, reasons


def repair_timestamps(
    raw_values: Sequence[int | None],
    *,
    time_base: TimeBase | None = None,
    wrap_period: int | None = DEFAULT_WRAP_PERIOD,
    nominal_step: int | None = None,
    reorder_window: int | None = None,
    gap_threshold: int | None = None,
    offsets: Sequence[int] | None = None,
    strict: bool = True,
) -> TimelineRepair:
    """Map raw stream timestamps onto a monotonic, gap-preserving timeline.

    Handled defects: wrap-around, duplicates, non-monotonic/reordered values and
    missing values.  ``fixed`` is always strictly increasing.  ``uncertain`` is
    set where ordering cannot be decided uniquely: large discontinuities and
    absent timestamps carry an explicit ``[window_lo, window_hi]`` interval.
    """
    n = len(raw_values)
    tb = time_base or TimeBase()
    offsets = list(offsets) if offsets is not None else [0] * n
    issues: list[MediaIssue] = []
    if n == 0:
        return TimelineRepair(time_base=tb, decisions=[], issues=issues)

    valid = sorted(v for v in raw_values if v is not None and v >= 0)
    step = int(nominal_step) if nominal_step else _median_step(valid)
    step = max(1, step)
    window = int(reorder_window) if reorder_window else 4 * step
    gap_limit = int(gap_threshold) if gap_threshold else max(4 * step, 2 * window)

    if wrap_period:
        unwrapped, wrapped, wraps = _unwrap(raw_values, int(wrap_period))
    else:
        unwrapped = [None if v is None else (v + 2**63 if v < 0 else v) for v in raw_values]
        wrapped = [False] * n
        wraps = 0

    pre_reason = ["ok"] * n
    if reorder_window is None:
        unwrapped, pre_reason = _reorder_pass(unwrapped, window, max(2, window // step))

    decisions: list[StampDecision] = []
    seen: dict[int, int] = {}
    previous: int | None = None

    def emit(decision: StampDecision, bump: bool = True) -> None:
        nonlocal previous
        if bump and previous is not None and decision.fixed <= previous:
            # Hard guarantee: the display timeline stays monotonic.
            decision.fixed = previous + step
        previous = decision.fixed if decision.fixed > (previous if previous is not None else -1) else previous
        decisions.append(decision)

    for i in range(n):
        raw = raw_values[i]
        off = offsets[i] if i < len(offsets) else 0
        value = unwrapped[i]

        if value is None:
            base = (previous + step) if previous is not None else 0
            uncertainty = window if window > 0 else step
            reason = "missing_filled" if previous is not None else "missing_leading"
            note = ("timestamp absent; placed on expected grid"
                    if previous is not None else
                    "leading timestamp absent; timeline origin unknown")
            lo = max(previous + 1 if previous is not None else 0, base - uncertainty)
            emit(StampDecision(
                raw=None, fixed=base, reason=reason, uncertain=True,
                window_lo=lo, window_hi=base + uncertainty,
                note=note))
            issues.append(MediaIssue(
                "timestamp_missing" if previous is not None else "timestamp_missing_leading",
                note, off, severity="warning", detail={"index": i}))
            continue

        if raw is not None and raw in seen:
            original = seen[raw]
            first_fixed = decisions[original].fixed
            if strict:
                candidate = max(first_fixed + step,
                                (previous + step) if previous is not None else step)
                window_lo, window_hi = candidate, candidate + window
                note = "duplicate timestamp; kept and shifted after its first occurrence"
            else:
                candidate = first_fixed
                window_lo, window_hi = first_fixed, first_fixed + step
                note = "duplicate timestamp; same presentation slot as first occurrence"
            emit(StampDecision(
                raw=raw, fixed=candidate, reason="duplicate", uncertain=True,
                window_lo=window_lo, window_hi=window_hi,
                duplicate_of=original, note=note), bump=strict)
            issues.append(MediaIssue(
                "timestamp_duplicate",
                f"duplicate timestamp value {raw}; shifted after first occurrence",
                off, severity="warning", detail={"index": i, "duplicate_of": original}))
            continue

        reordered = pre_reason[i]
        if previous is None:
            emit(StampDecision(raw=raw, fixed=value, reason=reordered))
        elif value > previous:
            gap = value - previous
            if gap > gap_limit:
                emit(StampDecision(
                    raw=raw, fixed=value, reason="gap", uncertain=True,
                    window_lo=previous + step, window_hi=value,
                    note="discontinuity larger than expected step; interval uncertain"))
                issues.append(MediaIssue(
                    "timestamp_gap", f"timestamp discontinuity of {gap} ticks",
                    off, severity="warning", detail={"index": i, "gap": gap}))
            elif wrapped[i]:
                emit(StampDecision(
                    raw=raw, fixed=value, reason="wrapped",
                    note="timestamp counter wrapped; epoch restored"))
            else:
                emit(StampDecision(raw=raw, fixed=value, reason=reordered))
                if reordered != "ok":
                    issues.append(MediaIssue(
                        "timestamp_reordered", "out-of-order timestamp restored to value order",
                        off, severity="warning", detail={"index": i}))
        else:
            distance = previous - value
            if distance > window:
                emit(StampDecision(
                    raw=raw, fixed=previous + step, reason="reordered_large", uncertain=True,
                    window_lo=value, window_hi=previous,
                    note="timestamp far behind its position; exact ordering ambiguous"))
                issues.append(MediaIssue(
                    "timestamp_reordered", "large backward jump; ordering ambiguous",
                    off, severity="warning",
                    detail={"index": i, "distance": distance}))
            else:
                emit(StampDecision(
                    raw=raw, fixed=previous + step, reason="reordered",
                    note="out-of-order timestamp re-sequenced by arrival order"))
                issues.append(MediaIssue(
                    "timestamp_reordered", "non-monotonic timestamp re-sequenced",
                    off, severity="warning", detail={"index": i, "distance": distance}))

        if raw is not None:
            seen[raw] = len(decisions) - 1

    return TimelineRepair(
        time_base=tb, decisions=decisions, issues=issues,
        wrap_count=wraps, unit_ticks=step)


def align_audio_to_video(
    audio_seconds: Sequence[Fraction | float],
    video_seconds: Sequence[Fraction | float],
    *,
    sample_rate: int,
    tolerance_ms: float = 40.0,
) -> dict[str, Any]:
    """Measure A/V sync: stream origin offset plus per-block residuals.

    Audio blocks and video frames have independent periods (e.g. 4096 samples
    at 48 kHz vs. a 33.3 ms frame), so a block start will not generally fall on
    a frame boundary.  Two separate numbers are therefore reported:

    * ``offset_ms`` - the origin shift between the first audio block and the
      first video frame (positive: audio starts later).  This is the actual
      sync error a player would hear.
    * ``max_alignment_ms`` - the worst distance from an origin-corrected audio
      block start to the nearest video frame on the regular frame grid.  With
      aligned origins this is bounded by half a frame period, and sample-level
      quantisation of block starts is included exactly.

    ``sample_offset`` is the origin shift rounded to whole audio samples, which
    is the only resolution at which PCM can actually be shifted.
    """
    av = [Fraction(x) for x in audio_seconds]
    vv = [Fraction(x) for x in video_seconds]
    if not av or not vv:
        return {"offset_ms": 0.0, "max_alignment_ms": 0.0, "mean_alignment_ms": 0.0,
                "within_tolerance": True, "pairs": 0, "sample_offset": 0,
                "frame_period_ms": 0.0, "sample_rate": sample_rate}
    if len(vv) > 1:
        frame_period = (vv[-1] - vv[0]) / (len(vv) - 1)
    else:
        frame_period = Fraction(1, 25)
    origin = av[0] - vv[0]
    grid_start = vv[0]
    residuals: list[Fraction] = []
    for a in av:
        target = a - origin
        # index of the nearest point on the regular frame grid (may lie past
        # the last frame, since audio may run to the end of the last frame).
        index = _round_half_up((target - grid_start) / frame_period)
        nearest = grid_start + index * frame_period
        residuals.append(abs(target - nearest))
    max_res = max(residuals)
    mean_res = sum(residuals, Fraction(0)) / len(residuals)
    max_ms = float(max_res) * 1000.0
    mean_ms = float(mean_res) * 1000.0
    return {
        "offset_ms": float(origin) * 1000.0,
        "max_alignment_ms": max_ms,
        "mean_alignment_ms": mean_ms,
        "within_tolerance": max_ms <= tolerance_ms,
        "pairs": len(residuals),
        "frame_period_ms": float(frame_period) * 1000.0,
        "sample_rate": sample_rate,
        "sample_offset": _round_half_up(origin * sample_rate),
    }


def jitter_stats(seconds: Iterable[Fraction | float]) -> dict[str, float]:
    """Summarise inter-frame spacing in milliseconds for manifest quality data."""
    values = sorted(Fraction(x) for x in seconds)
    if len(values) < 2:
        return {"frames": len(values), "mean_ms": 0.0, "max_ms": 0.0, "min_ms": 0.0}
    deltas = [(b - a) * 1000 for a, b in zip(values, values[1:])]
    floats = [float(d) for d in deltas]
    return {
        "frames": len(values),
        "mean_ms": sum(floats) / len(floats),
        "max_ms": max(floats),
        "min_ms": min(floats),
    }
