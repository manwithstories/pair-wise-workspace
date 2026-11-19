"""Bounded, cancellable queue primitives.

Python's :class:`queue.Queue` already blocks producers when full, but it cannot
be woken by a cancellation flag: a producer parked in ``put()`` waits for a
consumer that may itself be shutting down.  :class:`BoundedQueue` adds that
wake-up path and keeps an explicit high-water mark so the acceptance report can
prove the memory ceiling held (``peak_items <= capacity``).

The queue tracks byte size as well: a small number of huge packets must not
exceed the intended memory budget even when the item count is within bounds.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Generic, TypeVar

from .timebase import CancelledError, QueueOverflowError

T = TypeVar("T")


@dataclass
class QueueStats:
    """Observable behaviour of one queue, used for acceptance reporting."""

    capacity_items: int
    capacity_bytes: int
    peak_items: int = 0
    peak_bytes: int = 0
    puts: int = 0
    gets: int = 0
    blocked_puts: int = 0
    total_wait_seconds: float = 0.0

    def to_dict(self) -> dict[str, object]:
        return {
            "capacity_items": self.capacity_items,
            "capacity_bytes": self.capacity_bytes,
            "peak_items": self.peak_items,
            "peak_bytes": self.peak_bytes,
            "puts": self.puts,
            "gets": self.gets,
            "blocked_puts": self.blocked_puts,
            "total_wait_seconds": round(self.total_wait_seconds, 6),
        }


class BoundedQueue(Generic[T]):
    """A bounded queue with cancellation-aware ``put``/``get``.

    ``item_size`` maps an item to its byte footprint for the byte ceiling.
    ``cancel`` is a :class:`threading.Event` (or any object with ``is_set``);
    once set, a blocked producer/consumer raises :class:`CancelledError`
    promptly instead of waiting for the other side.
    """

    def __init__(
        self,
        capacity: int,
        *,
        name: str = "queue",
        capacity_bytes: int | None = None,
        item_size=None,
        cancel: threading.Event | None = None,
        poll_interval: float = 0.01,
    ) -> None:
        if capacity <= 0:
            raise ValueError("queue capacity must be positive")
        self.capacity = capacity
        self.name = name
        self.cancel = cancel
        self.poll_interval = poll_interval
        self._capacity_bytes = capacity_bytes if capacity_bytes is not None else 0
        self._item_size = item_size
        self._items: list[T] = []
        self._bytes = 0
        self._closed = False
        self._not_empty = threading.Condition()
        self._not_full = threading.Condition()
        self.stats = QueueStats(
            capacity_items=capacity,
            capacity_bytes=self._capacity_bytes,
        )

    # -- introspection ---------------------------------------------------- #

    def qsize(self) -> int:
        with self._not_empty:
            return len(self._items)

    @property
    def bytes_held(self) -> int:
        with self._not_empty:
            return self._bytes

    @property
    def closed(self) -> bool:
        return self._closed

    # -- lifecycle -------------------------------------------------------- #

    def close(self) -> None:
        """Stop accepting new items and wake every waiter."""
        with self._not_empty:
            self._closed = True
            self._not_empty.notify_all()
        with self._not_full:
            self._not_full.notify_all()

    # -- item transfer ---------------------------------------------------- #

    def put(self, item: T) -> None:
        size = self._item_size(item) if self._item_size is not None else 0
        waited = False
        with self._not_full:
            while self._is_full(len(self._items), self._bytes, size):
                if self._cancelled():
                    raise CancelledError(
                        "queue put cancelled while waiting for capacity",
                        queue=self.name,
                        depth=len(self._items),
                        capacity=self.capacity,
                    )
                if self._closed:
                    raise QueueOverflowError(
                        "queue closed while producer was blocked",
                        queue=self.name,
                        depth=len(self._items),
                    )
                if not waited:
                    self.stats.blocked_puts += 1
                    waited = True
                start = _now()
                self._not_full.wait(self.poll_interval)
                self.stats.total_wait_seconds += _now() - start
            if self._cancelled():
                raise CancelledError(
                    "queue put cancelled", queue=self.name, depth=len(self._items)
                )
            self._items.append(item)
            self._bytes += size
            self.stats.puts += 1
            if len(self._items) > self.stats.peak_items:
                self.stats.peak_items = len(self._items)
            if self._bytes > self.stats.peak_bytes:
                self.stats.peak_bytes = self._bytes
        with self._not_empty:
            self._not_empty.notify()

    def get(self) -> T:
        with self._not_empty:
            while not self._items:
                if self._cancelled():
                    raise CancelledError("queue get cancelled", queue=self.name)
                if self._closed:
                    raise QueueOverflowError(
                        "queue closed with no items remaining", queue=self.name
                    )
                self._not_empty.wait(self.poll_interval)
            item = self._items.pop(0)
            self._bytes -= self._item_size(item) if self._item_size else 0
            self.stats.gets += 1
        with self._not_full:
            self._not_full.notify()
        return item

    def drain(self) -> list[T]:
        """Remove and return everything queued (used on cancellation)."""
        with self._not_empty:
            items = self._items
            self._items = []
            self._bytes = 0
        with self._not_full:
            self._not_full.notify_all()
        return items

    # -- internals -------------------------------------------------------- #

    def _is_full(self, count: int, held_bytes: int, incoming: int) -> bool:
        if count >= self.capacity:
            return True
        if self._capacity_bytes and held_bytes + incoming > self._capacity_bytes:
            # Always admit a single item even if it alone exceeds the byte
            # budget, otherwise an oversized packet would deadlock forever.
            return count > 0
        return False

    def _cancelled(self) -> bool:
        return self.cancel is not None and self.cancel.is_set()


def _now() -> float:
    import time

    return time.monotonic()


__all__ = ["BoundedQueue", "QueueStats"]
