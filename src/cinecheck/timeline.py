"""Timestamp anomaly detection and equal-interval timeline repair.

All math is performed in stream ticks; human units (ms/fps) are derived by
the stats/export layers from the stream time base, never hard-coded here.

Detected anomalies, counted separately with the offending original value
retained for the report:

* ``duplicate_pts``   - two consecutive display frames share a pts
* ``negative_delta``  - pts regresses between consecutive frames
* ``gap``             - interval exceeds 1.5x the nominal frame length
* ``jitter``          - interval deviates from nominal beyond tolerance
* ``non_zero_start``  - first pts does not sit on the origin tick
* ``tail_residual``   - last interval is shorter than one full frame
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

GAP_FACTOR = Fraction(3, 2)
DEFAULT_TOLERANCE_MS = 1.0


@dataclass
class AnomalyEvent:
    kind: str
    display_index: int
    observed_pts: int
    detail: str
    value: int | None = None


@dataclass
class TimelineReport:
    pts: list[int]
    nominal_step_ticks: int
    tolerance_ticks: int
    counts: dict[str, int]
    events: list[AnomalyEvent] = field(default_factory=list)

    @property
    def total(self) -> int:
        return sum(self.counts.values())


@dataclass
class RepairResult:
    repaired_pts: list[int]
    repaired_count: int
    max_jump_ticks: int
    max_jump_index: int | None
    idempotent_pass: bool
    second_pass_repairs: int


def frame_step_ticks(time_base: Fraction, fps: Fraction) -> int:
    """Nominal frame duration expressed in whole stream ticks.

    Computed in exact rational arithmetic and floored so common rates such
    as 24 fps on a 1/12288 clock yield the true 512-tick step instead of a
    float-rounded neighbour.
    """

    # (tb.den / tb.num) / (fps.num / fps.den), evaluated as integer
    # cross multiplication so 12288/1 * 1/24 -> 12288*1 // (1*24) = 512.
    numerator = time_base.denominator * fps.denominator
    denominator = time_base.numerator * fps.numerator
    return max(1, numerator // denominator)


def _tolerance_ticks(time_base: Fraction, tolerance_ms: float) -> int:
    seconds_per_tick = float(time_base)
    ms_per_tick = seconds_per_tick * 1000.0
    return max(1, round(tolerance_ms / ms_per_tick))


def detect_anomalies(
    pts: list[int],
    time_base: Fraction,
    fps: Fraction,
    tolerance_ms: float = DEFAULT_TOLERANCE_MS,
) -> TimelineReport:
    """Classify every timestamp anomaly in a display-order pts list."""

    step = frame_step_ticks(time_base, fps)
    tol = _tolerance_ticks(time_base, tolerance_ms)
    counts = {
        "duplicate_pts": 0,
        "negative_delta": 0,
        "gap": 0,
        "jitter": 0,
        "non_zero_start": 0,
        "tail_residual": 0,
    }
    events: list[AnomalyEvent] = []
    if not pts:
        return TimelineReport(pts, step, tol, counts, events)

    if pts[0] != 0:
        counts["non_zero_start"] = 1
        events.append(
            AnomalyEvent("non_zero_start", 0, pts[0],
                         f"first pts {pts[0]} != 0", pts[0])
        )

    for index in range(1, len(pts)):
        delta = pts[index] - pts[index - 1]
        if delta == 0:
            counts["duplicate_pts"] += 1
            events.append(AnomalyEvent(
                "duplicate_pts", index, pts[index],
                f"pts repeats {pts[index - 1]}", pts[index]))
            continue
        if delta < 0:
            counts["negative_delta"] += 1
            events.append(AnomalyEvent(
                "negative_delta", index, pts[index],
                f"delta {delta} < 0", delta))
            continue
        if delta > step * GAP_FACTOR:
            counts["gap"] += 1
            events.append(AnomalyEvent(
                "gap", index, pts[index],
                f"delta {delta} > {int(step * GAP_FACTOR)}", delta))
        elif abs(delta - step) > tol:
            counts["jitter"] += 1
            events.append(AnomalyEvent(
                "jitter", index, pts[index],
                f"delta {delta} != {step} (+/-{tol})", delta))

    if len(pts) >= 2:
        tail_delta = pts[-1] - pts[-2]
        # A residual frame is one whose closing interval is degenerate or
        # shorter than a full frame: non-positive (duplicate/regression) or
        # below half the nominal step. A merely different-but-regular last
        # interval on a VFR clip is not a residual frame.
        if tail_delta <= 0 or 0 < tail_delta < step // 2:
            counts["tail_residual"] += 1
            events.append(AnomalyEvent(
                "tail_residual", len(pts) - 1, pts[-1],
                f"tail interval {tail_delta} under half step {step // 2}",
                tail_delta))

    return TimelineReport(list(pts), step, tol, counts, events)


def repair_timeline(
    pts: list[int],
    time_base: Fraction,
    fps: Fraction,
    tolerance_ms: float = DEFAULT_TOLERANCE_MS,
) -> RepairResult:
    """Rebuild an equal-interval axis at the declared frame rate.

    The repaired axis is ``i * step`` ticks. A frame is counted as
    repaired whenever its original pts differs from that axis beyond the
    configured tolerance. The largest absolute correction is reported in
    ticks; callers convert to ms with the stream time base.
    """

    step = frame_step_ticks(time_base, fps)
    tol = _tolerance_ticks(time_base, tolerance_ms)
    repaired_pts = [index * step for index in range(len(pts))]
    repaired_indexes = _continuity_repairs(pts, step, tol)
    max_jump = 0
    max_jump_index: int | None = None
    for index in repaired_indexes:
        jump = abs(repaired_pts[index] - pts[index])
        if jump > max_jump:
            max_jump = jump
            max_jump_index = index

    second = detect_anomalies(repaired_pts, time_base, fps, tolerance_ms)
    second_pass_repairs = len(
        _continuity_repairs(repaired_pts, step, tol)
    )
    return RepairResult(
        repaired_pts=repaired_pts,
        repaired_count=len(repaired_indexes),
        max_jump_ticks=max_jump,
        max_jump_index=max_jump_index,
        idempotent_pass=second.total == 0 and second_pass_repairs == 0,
        second_pass_repairs=second_pass_repairs,
    )


def _continuity_repairs(
    pts: list[int], step: int, tolerance_ticks: int
) -> list[int]:
    """Indexes that break equal-interval continuity.

    A frame counts as repaired only when its interval from the previous
    frame deviates from the nominal step beyond tolerance; a whole tail
    shifted by a constant offset therefore stops being flagged once it
    resumes a regular cadence. Frame zero is flagged solely on a non-zero
    origin, which :func:`detect_anomalies` reports independently.
    """

    repaired: list[int] = []
    if not pts:
        return repaired
    if pts[0] != 0:
        repaired.append(0)
    for index in range(1, len(pts)):
        if abs(pts[index] - pts[index - 1] - step) > tolerance_ticks:
            repaired.append(index)
    return repaired
