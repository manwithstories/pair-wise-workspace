"""Bounded queues, back-pressure, cancellation and slow-consumer timeouts.

The pipeline is a producer/consumer pair connected by a queue with a fixed
capacity in *blocks*.  Capacity is what bounds memory: however long the source
material is, at most ``capacity`` decoded blocks are alive at once.

Back-pressure works by blocking the producer when the queue is full rather
than buffering without limit.  A consumer that stops draining for longer than
``slow_consumer_timeout`` causes a :class:`ResourceError` instead of the queue
silently growing, which is the failure mode the old scripts had.

Cancellation is cooperative and thread-safe.  When a stage raises, the runner
cancels the token, closes the queue and drains it, so producer threads always
terminate and resources are closed even on failure.
"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass
from typing import Any, Callable, Generic, Iterable, Iterator, TypeVar

from .errors import ErrorCategory, CancelledError, ResourceError, make_error

__all__ = [
    "CancellationToken",
    "BoundedQueue",
    "QueueStats",
    "pipeline",
]

T = TypeVar("T")


class CancellationToken:
    """A cooperative, thread-safe cancellation flag."""

    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def raise_if_cancelled(self) -> None:
        if self._event.is_set():
            raise CancelledError(ErrorCategory.CANCELLED, "operation cancelled")


@dataclass
class QueueStats:
    capacity: int
    peak_depth: int = 0
    total_put: int = 0
    total_get: int = 0
    blocked_puts: int = 0
    blocked_seconds: float = 0.0
    max_block_seconds: float = 0.0
    timed_out: bool = False

    def to_json(self) -> dict[str, Any]:
        return {
            "capacity": self.capacity,
            "peak_depth": self.peak_depth,
            "total_put": self.total_put,
            "total_get": self.total_get,
            "blocked_puts": self.blocked_puts,
            "blocked_ms": round(self.blocked_seconds * 1000, 3),
            "max_block_ms": round(self.max_block_seconds * 1000, 3),
            "timed_out": self.timed_out,
        }


class BoundedQueue(Generic[T]):
    """A blocking FIFO with a hard capacity.

    ``put`` blocks while full.  If it stays full for longer than
    ``slow_consumer_timeout`` seconds, it raises ``runtime.consumer_timeout``.
    ``close`` marks the queue finished; pending and future ``get`` calls drain
    remaining items and then raise :class:`StopIteration` via :meth:`drain`.
    """

    def __init__(
        self,
        capacity: int,
        *,
        slow_consumer_timeout: float | None = None,
        token: CancellationToken | None = None,
        name: str = "queue",
    ) -> None:
        if capacity <= 0:
            raise ValueError("queue capacity must be positive")
        self.capacity = capacity
        self.slow_consumer_timeout = slow_consumer_timeout
        self.token = token or CancellationToken()
        self.name = name
        self._items: list[T] = []
        self._cond = threading.Condition()
        self._closed = False
        self._aborted: BaseException | None = None
        self.stats = QueueStats(capacity=capacity)

    # -- producer -------------------------------------------------------
    def put(self, item: T) -> None:
        self.token.raise_if_cancelled()
        with self._cond:
            if self._aborted is not None:
                raise self._aborted
            if len(self._items) >= self.capacity:
                self.stats.blocked_puts += 1
                started = time.monotonic()
                while len(self._items) >= self.capacity and not self._closed and self._aborted is None:
                    if self.token.cancelled:
                        raise CancelledError(ErrorCategory.CANCELLED, "cancelled while waiting for queue space")
                    # compute the timeout against the *total* block time
                    if self.slow_consumer_timeout is not None:
                        elapsed = time.monotonic() - started
                        remaining = self.slow_consumer_timeout - elapsed
                        if remaining <= 0:
                            self.stats.timed_out = True
                            raise ResourceError(
                                ErrorCategory.QUEUE_TIMEOUT,
                                f"consumer stalled for {self.slow_consumer_timeout:.3f}s on {self.name}",
                                details={"queue": self.name, "capacity": self.capacity, "depth": len(self._items)},
                            )
                    else:
                        remaining = 0.2
                    self._cond.wait(remaining)
                blocked = time.monotonic() - started
                self.stats.blocked_seconds += blocked
                self.stats.max_block_seconds = max(self.stats.max_block_seconds, blocked)
            if self._closed:
                raise ResourceError(ErrorCategory.CANCELLED, f"queue {self.name} closed before put")
            if self._aborted is not None:
                raise self._aborted
            self._items.append(item)
            self.stats.total_put += 1
            self.stats.peak_depth = max(self.stats.peak_depth, len(self._items))
            self._cond.notify_all()

    # -- consumer -------------------------------------------------------
    def get(self, timeout: float | None = None) -> T:
        with self._cond:
            if not self._items and not self._closed and self._aborted is None:
                self._cond.wait(timeout)
            if self._aborted is not None:
                raise self._aborted
            if not self._items:
                if self._closed:
                    raise StopIteration
                raise ResourceError(ErrorCategory.QUEUE_TIMEOUT, f"no item available on {self.name}")
            item = self._items.pop(0)
            self.stats.total_get += 1
            self._cond.notify_all()
            return item

    def try_get(self) -> T | None:
        with self._cond:
            if not self._items:
                return None
            item = self._items.pop(0)
            self.stats.total_get += 1
            self._cond.notify_all()
            return item

    def __iter__(self) -> Iterator[T]:
        while True:
            try:
                yield self.get()
            except StopIteration:
                return

    def __len__(self) -> int:
        with self._cond:
            return len(self._items)

    # -- lifecycle ------------------------------------------------------
    def close(self) -> None:
        with self._cond:
            self._closed = True
            self._cond.notify_all()

    def abort(self, exc: BaseException) -> None:
        """Wake every waiter and make them re-raise ``exc``."""
        with self._cond:
            self._aborted = exc
            self._closed = True
            self._cond.notify_all()

    def drain(self) -> list[T]:
        with self._cond:
            items = list(self._items)
            self._items.clear()
            self._cond.notify_all()
        return items

    @property
    def closed(self) -> bool:
        return self._closed


def pipeline(
    produce: Callable[[CancellationToken], Iterable[T]],
    consume: Callable[[Iterator[T]], Any],
    *,
    capacity: int,
    slow_consumer_timeout: float | None = None,
    name: str = "pipeline",
    token: CancellationToken | None = None,
) -> tuple[Any, QueueStats]:
    """Run ``produce`` in a worker thread feeding ``consume`` on the caller.

    Returns ``(consumer_result, queue_stats)``.  Any error raised by either
    side aborts the queue, cancels the token, joins the producer and is
    re-raised unchanged, so the caller sees the original diagnostic error
    (with its category, details and exit code) rather than a generic failure.
    """
    token = token or CancellationToken()
    queue: BoundedQueue[T] = BoundedQueue(capacity, slow_consumer_timeout=slow_consumer_timeout, token=token, name=name)
    producer_error: list[BaseException] = []

    def _run() -> None:
        try:
            for item in produce(token):
                if token.cancelled:
                    break
                queue.put(item)
        except BaseException as exc:  # noqa: BLE001 - forwarded to the consumer
            producer_error.append(exc)
        finally:
            queue.close()

    thread = threading.Thread(target=_run, name=f"{name}-producer", daemon=True)
    thread.start()
    try:
        result = consume(iter(queue))
    except BaseException as exc:
        token.cancel()
        queue.abort(exc)
        raise
    finally:
        token.cancel()
        queue.close()
        thread.join(timeout=10.0)
        if thread.is_alive():
            # a producer stuck in a non-interruptible call is a resource bug,
            # but we must still report it deterministically
            queue.abort(
                make_error(
                    ErrorCategory.RESOURCE_EXHAUSTED,
                    f"producer thread of {name} did not stop within 10s",
                    queue=name,
                )
            )
        queue.drain()
    if producer_error:
        raise producer_error[0]
    return result, queue.stats
