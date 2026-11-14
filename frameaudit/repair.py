"""Timestamp anomaly detection and integer-only repair strategies."""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Optional

from .decode import FrameInfo


@dataclass(slots=True)
class FrameDecision:
    display_index: int
    repaired_pts: int
    action: str
    anomaly: Optional[str]


@dataclass(slots=True)
class RepairResult:
    strategy: str
    decisions: list[FrameDecision]
    anomaly_count: int
    duplicate_count: int
    rollback_count: int
    jump_count: int
    rounding_duplicate_count: int
    first_offset: bool
    max_jump_ms: int
    first_pts_ms: int
    residual_max_jump_ms: int
    monotonic: bool
    source_monotonic: bool
    ticks_per_frame: int
    max_jump_ticks: int


def ticks_per_frame(time_base: Fraction, fps: Fraction) -> int:
    """Convert a rational frame rate to a positive, rounded tick interval."""

    if time_base <= 0 or fps <= 0:
        raise ValueError("time base and frame rate must be positive")
    numerator = time_base.denominator * fps.denominator
    denominator = time_base.numerator * fps.numerator
    if denominator <= 0:
        raise ValueError("invalid frame rate")
    return max(1, (numerator + denominator // 2) // denominator)


def _round_div(numerator: int, denominator: int) -> int:
    if denominator <= 0:
        raise ValueError("denominator must be positive")
    if numerator >= 0:
        return (numerator + denominator // 2) // denominator
    return -((-numerator + denominator // 2) // denominator)


def ticks_to_ms(ticks: int, time_base: Fraction) -> int:
    return _round_div(ticks * 1000 * time_base.numerator, time_base.denominator)


def ms_to_ticks(milliseconds: int, time_base: Fraction) -> int:
    return _round_div(milliseconds * time_base.denominator, 1000 * time_base.numerator)


def _source_pts(frame: FrameInfo, fallback: int) -> int:
    return frame.pts if frame.pts is not None else fallback


def _detect(
    frames: list[FrameInfo],
    time_base: Fraction,
    frame_interval: int,
    max_jump_ticks: int,
) -> tuple[list[str | None], list[int], int, bool]:
    """Classify each frame against its raw predecessor.

    The returned values are anomalies, raw PTS values, and the observed maximum
    positive jump in milliseconds.
    """

    anomalies: list[str | None] = [None] * len(frames)
    raw: list[int] = []
    observed_jump_ms = 0
    source_monotonic = True
    previous: Optional[int] = None
    for index, frame in enumerate(frames):
        pts = _source_pts(frame, index * frame_interval)
        raw.append(pts)
        if index == 0:
            if pts != 0:
                anomalies[index] = "first_offset"
            previous = pts
            continue
        assert previous is not None
        delta = pts - previous
        if delta <= 0:
            source_monotonic = False
        if pts == previous:
            anomalies[index] = "duplicate"
        elif delta < 0:
            anomalies[index] = "rollback"
        elif delta > max_jump_ticks:
            anomalies[index] = "jump"
        elif (
            frame_interval > 1
            and 0 < delta and delta * 2 < frame_interval
            and ticks_to_ms(pts, time_base) == ticks_to_ms(previous, time_base)
        ):
            anomalies[index] = "rounding_duplicate"
        if delta > 0:
            observed_jump_ms = max(observed_jump_ms, ticks_to_ms(delta, time_base))
        previous = pts
    return anomalies, raw, observed_jump_ms, source_monotonic


def _repair_shift(
    raw: list[int], anomalies: list[str | None], frame_interval: int, max_jump_ticks: int
) -> list[FrameDecision]:
    decisions: list[FrameDecision] = []
    if not raw:
        return decisions

    offset = raw[0]
    correction = 0
    previous_output = 0
    for index, (pts, anomaly) in enumerate(zip(raw, anomalies)):
        aligned = pts - offset
        candidate = aligned - correction
        if index == 0:
            output = 0
            action = "first_offset" if anomaly == "first_offset" else "keep"
        else:
            delta = candidate - previous_output
            if anomaly == "rollback" or (anomaly is None and delta <= 0):
                output = previous_output + frame_interval
                action = "rollback"
            elif anomaly in {"duplicate", "rounding_duplicate"}:
                output = previous_output + frame_interval
                action = anomaly
            elif delta > max_jump_ticks or anomaly == "jump":
                output = previous_output + frame_interval
                correction += candidate - output
                action = "jump"
            else:
                output = candidate
                action = "keep"
        decisions.append(FrameDecision(index, output, action, anomaly))
        previous_output = output
    return decisions


def frame_pts_at(index: int, time_base: Fraction, fps: Fraction) -> int:
    numerator = index * time_base.denominator * fps.denominator
    denominator = time_base.numerator * fps.numerator
    return _round_div(numerator, denominator)


def _repair_cfr(raw: list[int], time_base: Fraction, fps: Fraction) -> list[FrameDecision]:
    return [
        FrameDecision(index, frame_pts_at(index, time_base, fps), "cfr", None)
        for index, _ in enumerate(raw)
    ]


def _residual_stats(decisions: list[FrameDecision], time_base: Fraction) -> tuple[int, bool]:
    if len(decisions) < 2:
        return 0, True
    residual = 0
    monotonic = True
    previous = decisions[0].repaired_pts
    for decision in decisions[1:]:
        delta = decision.repaired_pts - previous
        if delta <= 0:
            monotonic = False
        residual = max(residual, ticks_to_ms(delta, time_base))
        previous = decision.repaired_pts
    return residual, monotonic


def repair_timestamps(
    frames: list[FrameInfo],
    *,
    time_base: Fraction,
    fps: Fraction,
    strategy: str,
    max_jump_ms: int,
) -> RepairResult:
    """Detect timestamp defects and apply ``shift`` or ``cfr`` repair."""

    if strategy not in {"scan", "shift", "cfr"}:
        raise ValueError("strategy must be 'scan', 'shift', or 'cfr'")
    if max_jump_ms < 0:
        raise ValueError("max_jump_ms must be non-negative")

    interval = ticks_per_frame(time_base, fps)
    jump_ticks = ms_to_ticks(max_jump_ms, time_base)
    anomalies, raw, observed_jump_ms, source_monotonic = _detect(
        frames, time_base, interval, jump_ticks
    )
    if strategy == "cfr":
        decisions = _repair_cfr(raw, time_base, fps)
    else:
        decisions = _repair_shift(raw, anomalies, interval, jump_ticks)
    residual_jump, repaired_monotonic = _residual_stats(decisions, time_base)
    counts = {name: anomalies.count(name) for name in {
        "first_offset", "duplicate", "rollback", "jump", "rounding_duplicate"
    }}
    return RepairResult(
        strategy=strategy,
        decisions=decisions,
        anomaly_count=sum(value is not None for value in anomalies),
        duplicate_count=counts["duplicate"],
        rollback_count=counts["rollback"],
        jump_count=counts["jump"],
        rounding_duplicate_count=counts["rounding_duplicate"],
        first_offset=counts["first_offset"] > 0,
        max_jump_ms=observed_jump_ms,
        first_pts_ms=ticks_to_ms(raw[0], time_base) if raw else 0,
        residual_max_jump_ms=residual_jump,
        monotonic=repaired_monotonic,
        source_monotonic=source_monotonic,
        ticks_per_frame=interval,
        max_jump_ticks=jump_ticks,
    )
