"""Decode-order -> display-order reordering with a bounded window.

A capacity-16 window mirrors a hardware decoder reorder buffer. Records are
keyed by PTS; missing DTS/PTS are inferred from decode order and the nominal
frame interval. If a record arrives whose key is smaller than an already
emitted key, the window was too small: warn ``reorder_overflow`` and fall
back to DTS order.
"""

from __future__ import annotations

from .decoder import FrameRecord

WINDOW_CAPACITY = 16
REORDER_OVERFLOW = "reorder_overflow"


def _inferred_dts(record: FrameRecord, interval: float) -> float:
    if record.dts is not None:
        return record.dts
    # Missing DTS: infer from decode order and the nominal frame rate.
    return record.decode_index * interval


def _sort_key(record: FrameRecord, interval: float) -> float:
    if record.pts is not None:
        return record.pts
    return _inferred_dts(record, interval)


def reorder(records, interval: float, capacity: int = WINDOW_CAPACITY):
    """Return (display_order_records, warnings)."""
    if not records:
        return [], []

    window: list[FrameRecord] = []
    emitted: list[FrameRecord] = []
    last_emitted_key: float | None = None
    overflow = False

    for record in records:
        key = _sort_key(record, interval)
        if last_emitted_key is not None and key < last_emitted_key:
            overflow = True
        window.append(record)
        if len(window) >= capacity:
            oldest = min(window, key=lambda r: _sort_key(r, interval))
            window.remove(oldest)
            emitted.append(oldest)
            last_emitted_key = _sort_key(oldest, interval)

    emitted.extend(sorted(window, key=lambda r: _sort_key(r, interval)))

    if not overflow:
        return emitted, []

    # Fallback: the window could not hold the reordering depth, emit in
    # (inferred) DTS order instead.
    fallback = sorted(
        records,
        key=lambda r: (_inferred_dts(r, interval), r.decode_index),
    )
    return fallback, [REORDER_OVERFLOW]
