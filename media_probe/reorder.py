"""Frame reordering kernel: restore display order from decode order.

Stable sort by PTS; ties are broken by DTS, then by packet (decode) order.
Shared by both the scan and repair pipelines.
"""

from __future__ import annotations

from .framewalk import FrameRecord

_NO_TS = (1 << 62)  # sorts missing timestamps last without overflowing


def _sort_key(record: FrameRecord):
    pts = record.pts if record.pts is not None else _NO_TS
    dts = record.dts if record.dts is not None else _NO_TS
    return (pts, dts, record.packet_index)


def display_order(records: list[FrameRecord]) -> list[FrameRecord]:
    """Return records sorted into presentation (display) order.

    Python's sort is stable, so equal (pts, dts) keys keep packet order.
    """
    return sorted(records, key=_sort_key)


def derive_missing_dts(ordered: list[FrameRecord], interval: int) -> list[str]:
    """Fill in missing DTS values in display order (monotonic, pts-capped).

    Returns a list of notes describing how many values were derived.
    Mutates the given records.
    """
    derived = 0
    previous = None
    for record in ordered:
        if record.dts is None:
            candidate = previous + interval if previous is not None else 0
            if record.pts is not None:
                candidate = min(candidate, record.pts)
            record.dts = candidate
            derived += 1
        previous = record.dts
    if derived:
        return [f"derived dts for {derived} frames"]
    return []
