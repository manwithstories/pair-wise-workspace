"""Threading primitives and decoded-item data types.

Holds the cooperative cancellation token, the bounded backpressure queue
and the video/audio item dataclasses shared by the decoder worker and the
pipeline consumer.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from fractions import Fraction
from queue import Full, Queue

import numpy as np

from .errors import ErrorCode, RecoverableError

VIDEO = "video"
AUDIO = "audio"


class CancellationToken:
    """Cooperative cancellation shared by every pipeline stage."""

    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def raise_if_cancelled(self) -> None:
        if self._event.is_set():
            raise RecoverableError(ErrorCode.CANCELLED, "pipeline cancelled")


@dataclass(slots=True)
class VideoItem:
    rgb: np.ndarray
    pts_seconds: Fraction
    raw_pts: int
    kind: str               # advance / duplicate / backward / initial
    segment: str
    sequence: int
    frame_index: int
    wrapped: bool = False   # a 33-bit modulus wrap happened at this frame


@dataclass(slots=True)
class AudioItem:
    pcm: np.ndarray         # mono int16
    pts_seconds: Fraction
    nominal_seconds: Fraction  # undrifted reference position on the grid
    sample_rate: int
    segment: str
    sequence: int


@dataclass(slots=True)
class SegmentBoundary:
    sequence: int
    segment: str
    declared_duration: Fraction
    first_pts: Fraction | None = None
    last_pts: Fraction | None = None
    truncated: bool = False


@dataclass(slots=True)
class BoundedQueue:
    """Capacity-bounded queue with explicit slow-consumer accounting."""

    capacity: int
    token: CancellationToken
    slow_consumer_events: int = 0
    high_watermark: int = 0
    _q: Queue = field(init=False)
    _lock: threading.Lock = field(default_factory=threading.Lock)

    def __post_init__(self) -> None:
        self._q = Queue(maxsize=self.capacity)

    def push(self, item: object, timeout: float = 2.0) -> None:
        while True:
            self.token.raise_if_cancelled()
            try:
                self._q.put(item, timeout=timeout)
                with self._lock:
                    self.high_watermark = max(self.high_watermark, self._q.qsize())
                return
            except Full:
                with self._lock:
                    self.slow_consumer_events += 1

    def pop(self, timeout: float = 5.0) -> object:
        return self._q.get(timeout=timeout)

    def close(self) -> None:
        self._q.put(None)

    def qsize(self) -> int:
        return self._q.qsize()
