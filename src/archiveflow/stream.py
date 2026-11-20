"""Bounded queues, backpressure and stage cancellation.

Stages communicate through BoundedQueue instances with a hard item cap.
A slow downstream stage blocks the upstream producer (backpressure) so
in-flight memory stays bounded; peak occupancy is recorded for the
manifest. Cancellation or a write/error in any stage stops intake and
propagates so the pipeline keeps its last complete output instead of
registering half-finished results.
"""

from __future__ import annotations

import queue
import threading
from collections.abc import Callable, Iterable
from dataclasses import dataclass, field

from .errors import CancelledError

_SENTINEL = object()


class BoundedQueue:
    """A queue with a hard cap; put blocks while full (backpressure)."""

    def __init__(self, maxsize: int, cancel: threading.Event | None = None):
        if maxsize < 1:
            raise ValueError("queue maxsize must be >= 1")
        self.maxsize = maxsize
        self._q: queue.Queue = queue.Queue(maxsize=maxsize)
        self._cancel = cancel or threading.Event()
        self.peak_items = 0
        self._lock = threading.Lock()

    def put(self, item) -> None:
        while True:
            if self._cancel.is_set():
                raise CancelledError("queue intake stopped: pipeline cancelled")
            try:
                self._q.put(item, timeout=0.05)
            except queue.Full:
                continue  # downstream slow: keep applying backpressure
            with self._lock:
                self.peak_items = max(self.peak_items, self._q.qsize())
            return

    def get(self, timeout: float = 0.1):
        return self._q.get(timeout=timeout)

    def close(self) -> None:
        """Signal end-of-stream to consumers."""
        while True:
            try:
                self._q.put(_SENTINEL, timeout=0.05)
                return
            except queue.Full:
                if self._cancel.is_set():
                    return

    def __iter__(self):
        while True:
            item = self.get()
            if item is _SENTINEL:
                return
            yield item


@dataclass
class PipelineStats:
    peak_queue_items: int = 0
    stage_errors: list[str] = field(default_factory=list)
    cancelled: bool = False


class Pipeline:
    """Runs producer -> stages -> consumer threads over bounded queues."""

    def __init__(self, queue_size: int) -> None:
        self.cancel_event = threading.Event()
        self.queue_size = queue_size
        self.stats = PipelineStats()
        self._queues: list[BoundedQueue] = []

    def new_queue(self) -> BoundedQueue:
        q = BoundedQueue(self.queue_size, self.cancel_event)
        self._queues.append(q)
        return q

    def cancel(self) -> None:
        self.stats.cancelled = True
        self.cancel_event.set()

    def run(self, producer: Callable[[BoundedQueue], None],
            consumer: Callable[[Iterable], None]) -> PipelineStats:
        """Wire producer and consumer through one bounded queue.

        On any failure the pipeline is cancelled, intake stops, and the
        first error is re-raised after both threads are joined.
        """
        q = self.new_queue()
        errors: list[BaseException] = []

        def _produce() -> None:
            try:
                producer(q)
            except BaseException as exc:  # noqa: BLE001 - re-raised below
                errors.append(exc)
                self.cancel()
            finally:
                q.close()

        def _consume() -> None:
            try:
                consumer(iter(q))
            except BaseException as exc:  # noqa: BLE001
                errors.append(exc)
                self.cancel()

        threads = [threading.Thread(target=_produce, daemon=True),
                   threading.Thread(target=_consume, daemon=True)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.stats.peak_queue_items = max(
            (q.peak_items for q in self._queues), default=0)
        if errors:
            err = errors[0]
            self.stats.stage_errors.append(str(err))
            raise err
        return self.stats
