"""Timestamp repair kernel and jump (discontinuity) detection.

Operates on the display-order timeline produced by reorder.display_order.
Both the scan and repair CLI commands run this exact kernel so their
timeline conclusions are identical by construction.
"""

from __future__ import annotations

import statistics
from dataclasses import dataclass, field
from fractions import Fraction

from .framewalk import FrameRecord


@dataclass
class RepairRow:
    """One frame of the repaired display-order timeline."""

    frame_index: int
    pts: int | None
    dts: int | None
    repaired: bool
    source: FrameRecord


@dataclass
class Jump:
    frame_index: int  # row AFTER the gap
    delta_ticks: int
    delta_ms: float


@dataclass
class RepairResult:
    rows: list[RepairRow] = field(default_factory=list)
    events: dict[str, int] = field(default_factory=dict)
    interval_ticks: int = 1
    jumps: list[Jump] = field(default_factory=list)
    jump_threshold: float = 1.5

    @property
    def repaired_count(self) -> int:
        return sum(self.events.values())

    @property
    def max_jump_ms(self) -> float:
        return max((j.delta_ms for j in self.jumps), default=0.0)


def estimate_interval(ordered: list[FrameRecord], fallback: int | None = None) -> int:
    """Median of positive PTS deltas in display order (the nominal frame step)."""
    pts = [r.pts for r in ordered if r.pts is not None]
    deltas = [b - a for a, b in zip(pts, pts[1:]) if b - a > 0]
    if deltas:
        return max(1, int(statistics.median(deltas)))
    if fallback:
        return max(1, fallback)
    return 1


def repair_timeline(
    ordered: list[FrameRecord],
    jump_threshold: float = 1.5,
    time_base: Fraction = Fraction(1, 90000),
    fallback_interval: int | None = None,
) -> RepairResult:
    """Repair the display-order timeline and detect residual jumps.

    Steps: first-frame zeroing, duplicate-PTS collapse, monotonisation,
    DTS>PTS correction, DTS regression fix (decode order). Jumps are
    reported, not repaired - a gap may be genuine missing content.
    """
    result = RepairResult(jump_threshold=jump_threshold)
    interval = estimate_interval(ordered, fallback_interval)
    result.interval_ticks = interval
    rows = [
        RepairRow(i, r.pts, r.dts, False, r) for i, r in enumerate(ordered)
    ]

    def note(kind: str, count: int = 1) -> None:
        result.events[kind] = result.events.get(kind, 0) + count

    # 1. first-frame zeroing: shift the whole timeline so it starts at 0.
    anchor = next((r.pts for r in rows if r.pts is not None), None)
    if anchor:
        for row in rows:
            if row.pts is not None:
                row.pts -= anchor
            if row.dts is not None:
                row.dts -= anchor
        note("zero_shift")

    # 2. duplicate-PTS collapse + monotonisation (display order).
    previous_pts: int | None = None
    previous_raw: int | None = None
    for row in rows:
        raw = row.pts  # pre-repair value (after zero-shift)
        if row.pts is None:
            row.pts = (previous_pts + interval) if previous_pts is not None else 0
            row.repaired = True
            note("missing_pts")
        elif previous_pts is not None and row.pts <= previous_pts:
            if row.pts == previous_raw:
                # duplicate PTS: move onto the next slot (usually the hole
                # the duplicated timestamp was stolen from)
                row.pts = previous_pts + interval
                note("duplicate_pts")
            else:
                row.pts = previous_pts + interval
                note("monotonize")
            row.repaired = True
        previous_raw = raw
        previous_pts = row.pts

    # 3a. DTS must not exceed PTS.
    for row in rows:
        if row.dts is not None and row.pts is not None and row.dts > row.pts:
            row.dts = row.pts - interval
            row.repaired = True
            note("dts_gt_pts")

    # 3b. DTS must be monotonic in decode (packet) order.
    previous_dts: int | None = None
    for row in sorted(rows, key=lambda r: r.source.packet_index):
        if row.dts is None:
            if previous_dts is not None:
                row.dts = previous_dts + interval
            elif row.pts is not None:
                row.dts = row.pts - interval  # typical encoder delay
            else:
                row.dts = 0
            row.repaired = True
            note("dts_derived")
        elif previous_dts is not None and row.dts <= previous_dts:
            row.dts = previous_dts + interval
            row.repaired = True
            note("dts_regression")
        previous_dts = row.dts

    # 4. jump detection on the repaired display timeline.
    tick_ms = 1000.0 * float(time_base)
    threshold_ticks = jump_threshold * interval
    for earlier, later in zip(rows, rows[1:]):
        if earlier.pts is None or later.pts is None:
            continue
        delta = later.pts - earlier.pts
        if delta > threshold_ticks:
            result.jumps.append(
                Jump(
                    frame_index=later.frame_index,
                    delta_ticks=delta,
                    delta_ms=delta * tick_ms,
                )
            )
    result.rows = rows
    return result
