"""Display-order reordering with a bounded window (<=32 frames).

H.264 packets arrive in decode order (I, P and the B frames that reference
them); the mapping pipeline needs strict display order. The window holds
at most ``depth`` frames and flushes the smallest pts each time it fills,
which bounds memory regardless of film length. A missing pts is inferred
from the packet dts and flagged, and a pts regression relative to the last
flushed frame is counted as an ``order_violation``.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

from .decode import DecodedFrame

DEFAULT_REORDER_DEPTH = 32


@dataclass(frozen=True)
class ReorderedFrame:
    """A frame placed in display order."""

    display_index: int
    pts: int
    pts_inferred: bool
    original_pts: int | None
    dts: int | None
    keyframe: bool
    pict_type: str
    packet_index: int


@dataclass
class ReorderResult:
    frames: list[ReorderedFrame]
    order_violations: int
    inferred_pts: int
    window_peak: int


def _sort_key(frame: DecodedFrame) -> int:
    if frame.pts is not None:
        return int(frame.pts)
    if frame.dts is not None:
        return int(frame.dts)
    # Neither timestamp exists: fall back to decode order so the frame
    # stays in a deterministic position instead of raising at runtime.
    return frame.order


def reorder_frames(
    frames: Iterable[DecodedFrame],
    depth: int = DEFAULT_REORDER_DEPTH,
) -> ReorderResult:
    """Stable-sort decode-order frames into display order.

    ``depth`` is clamped to [1, 32] so the callers can never request an
    unbounded buffer. The stable key is pts, falling back to dts, then to
    decode order for frames with no timestamps at all.
    """

    depth = max(1, min(DEFAULT_REORDER_DEPTH, int(depth)))
    window: list[DecodedFrame] = []
    ordered: list[ReorderedFrame] = []
    violations = 0
    inferred = 0
    peak = 0
    last_key: int | None = None
    display_index = 0

    def flush() -> None:
        nonlocal last_key, display_index, violations, inferred
        window.sort(key=_sort_key)
        chosen = window.pop(0)
        key = _sort_key(chosen)
        if last_key is not None and key < last_key:
            violations += 1
        last_key = key
        if chosen.pts is None:
            inferred += 1
        ordered.append(
            ReorderedFrame(
                display_index=display_index,
                pts=key,
                pts_inferred=chosen.pts is None,
                original_pts=chosen.pts,
                dts=chosen.dts,
                keyframe=chosen.keyframe,
                pict_type=chosen.pict_type,
                packet_index=chosen.packet_index,
            )
        )
        display_index += 1

    for frame in frames:
        window.append(frame)
        peak = max(peak, len(window))
        if len(window) >= depth:
            flush()
    while window:
        flush()
    return ReorderResult(
        frames=ordered,
        order_violations=violations,
        inferred_pts=inferred,
        window_peak=peak,
    )
