"""Bounded presentation-order buffering for decoded video frames."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Optional

from .decode import FrameInfo


DEFAULT_REORDER_CAPACITY = 4


@dataclass(slots=True)
class ReorderResult:
    frames: list[FrameInfo]
    reordered_frames: int
    max_reorder_depth: int
    fallback_to_decode_order: bool


class BoundedReorderBuffer:
    """A deterministic min-PTS buffer with a fixed look-ahead window."""

    def __init__(self, capacity: int = DEFAULT_REORDER_CAPACITY) -> None:
        if capacity < 1:
            raise ValueError("reorder capacity must be positive")
        self.capacity = capacity
        self._items: list[tuple[int, int, FrameInfo]] = []

    def push(self, frame: FrameInfo) -> list[FrameInfo]:
        pts = frame.pts if frame.pts is not None else frame.decode_index
        self._items.append((pts, frame.decode_index, frame))
        released: list[FrameInfo] = []
        while len(self._items) >= self.capacity:
            released.append(self._pop_smallest())
        return released

    def _pop_smallest(self) -> FrameInfo:
        best = min(range(len(self._items)), key=lambda index: self._items[index][:2])
        return self._items.pop(best)[2]

    def drain(self) -> list[FrameInfo]:
        released: list[FrameInfo] = []
        while self._items:
            released.append(self._pop_smallest())
        return released


def _has_complete_pts(frames: list[FrameInfo]) -> bool:
    return bool(frames) and all(frame.pts is not None for frame in frames)


def reorder_frames(
    frames: Iterable[FrameInfo], *, capacity: int = DEFAULT_REORDER_CAPACITY
) -> ReorderResult:
    """Return stable PTS order and reorder statistics.

    Equal PTS values are ordered by decode index.  If PTS is unavailable for any
    frame in an elementary stream, the complete input falls back to decode order.
    The algorithm retains at most ``capacity`` metadata records at once.
    """

    source = list(frames)
    if not _has_complete_pts(source):
        return ReorderResult(
            frames=source,
            reordered_frames=0,
            max_reorder_depth=0,
            fallback_to_decode_order=True,
        )

    buffer = BoundedReorderBuffer(capacity=capacity)
    ordered: list[FrameInfo] = []
    for frame in source:
        ordered.extend(buffer.push(frame))
    ordered.extend(buffer.drain())

    reordered = 0
    max_depth = 0
    for display_index, frame in enumerate(ordered):
        max_depth = max(max_depth, abs(display_index - frame.decode_index))
    previous_pts: Optional[int] = None
    for frame in source:
        if previous_pts is not None and frame.pts is not None and frame.pts < previous_pts:
            reordered += 1
        previous_pts = frame.pts
    return ReorderResult(
        frames=ordered,
        reordered_frames=reordered,
        max_reorder_depth=max_depth,
        fallback_to_decode_order=False,
    )
