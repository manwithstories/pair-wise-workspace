"""Bounded queues, frame/audio caches and a lightweight memory budget.

Back pressure is structural, not advisory: producers call :meth:`BoundedQueue.put`
which blocks once the queue is full, and consumers take items one at a time.
Every stage that caches anything (scanned access units, decoded frames, audio
blocks) goes through :class:`ByteBudget`, which tracks *bytes*, not item counts,
so a long fixture cannot grow memory without bound even when each item is small.

:class:`FrameCache` holds decoded pictures up to a byte limit and evicts the
oldest entries; :class:`AudioBlockCache` does the same for PCM blocks.  Both
report eviction counts so the manifest can show the pipeline actually stayed
within its budget rather than getting lucky.
"""

from __future__ import annotations

import threading
from collections import OrderedDict
from dataclasses import dataclass
from typing import Any, Generic, Iterator, TypeVar

from .errors import MediaInputError, MEMORY_LIMIT_BYTES

T = TypeVar("T")


class BoundedQueue(Generic[T]):
    """A thread-safe FIFO with a hard item limit.

    ``put`` blocks while full (the back-pressure signal), ``put_nowait`` raises
    instead.  ``closed`` lets a producer wake blocked consumers on shutdown.
    """

    def __init__(self, max_items: int = 64, *, name: str = "queue") -> None:
        if max_items <= 0:
            raise MediaInputError(f"{name} max_items must be positive", code="queue_config")
        self.max_items = max_items
        self.name = name
        self._items: list[T] = []
        self._condition = threading.Condition()
        self._closed = False
        self._high_water = 0
        self._blocked_puts = 0
        self._pushed = 0
        self._popped = 0

    def put(self, item: T) -> None:
        with self._condition:
            while len(self._items) >= self.max_items and not self._closed:
                self._blocked_puts += 1
                self._condition.wait()
            if self._closed:
                raise MediaInputError(f"{self.name} is closed", code="queue_closed")
            self._items.append(item)
            self._pushed += 1
            self._high_water = max(self._high_water, len(self._items))
            self._condition.notify_all()

    def put_nowait(self, item: T) -> None:
        with self._condition:
            if self._closed:
                raise MediaInputError(f"{self.name} is closed", code="queue_closed")
            if len(self._items) >= self.max_items:
                self._blocked_puts += 1
                raise MediaInputError(
                    f"{self.name} is full ({self.max_items} items)", code="queue_full")
            self._items.append(item)
            self._pushed += 1
            self._high_water = max(self._high_water, len(self._items))
            self._condition.notify_all()

    def get(self, timeout: float | None = None) -> T | None:
        with self._condition:
            if not self._items and self._closed:
                return None
            if not self._items:
                self._condition.wait(timeout)
                if not self._items:
                    if self._closed:
                        return None
                    raise TimeoutError(f"{self.name} get timed out")
            item = self._items.pop(0)
            self._popped += 1
            self._condition.notify_all()
            return item

    def close(self) -> None:
        with self._condition:
            self._closed = True
            self._condition.notify_all()

    @property
    def closed(self) -> bool:
        return self._closed

    def __len__(self) -> int:
        return len(self._items)

    def drain(self) -> Iterator[T]:
        while True:
            item = self.get()
            if item is None:
                return
            yield item

    def stats(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "max_items": self.max_items,
            "high_water": self._high_water,
            "pushed": self._pushed,
            "popped": self._popped,
            "blocked_puts": self._blocked_puts,
            "resident": len(self._items),
        }


class ByteBudget:
    """Tracks allocated bytes against a hard ceiling."""

    def __init__(self, limit: int = MEMORY_LIMIT_BYTES) -> None:
        if limit <= 0:
            raise MediaInputError("memory limit must be positive", code="budget_config")
        self.limit = limit
        self.used = 0
        self.peak = 0
        self.denied = 0

    def try_reserve(self, nbytes: int) -> bool:
        if nbytes < 0:
            raise ValueError("cannot reserve negative bytes")
        if self.used + nbytes > self.limit:
            self.denied += 1
            return False
        self.used += nbytes
        self.peak = max(self.peak, self.used)
        return True

    def reserve(self, nbytes: int, *, what: str = "buffer") -> None:
        if not self.try_reserve(nbytes):
            raise MediaInputError(
                f"{what} would exceed the {self.limit} byte memory limit",
                code="memory_limit")

    def release(self, nbytes: int) -> None:
        self.used = max(0, self.used - nbytes)

    def stats(self) -> dict[str, Any]:
        return {
            "limit": self.limit,
            "used": self.used,
            "peak": self.peak,
            "denied": self.denied,
        }


@dataclass
class CacheStats:
    inserted: int = 0
    evicted: int = 0
    hits: int = 0
    misses: int = 0
    bytes_peak: int = 0
    bytes_current: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "inserted": self.inserted,
            "evicted": self.evicted,
            "hits": self.hits,
            "misses": self.misses,
            "bytes_peak": self.bytes_peak,
            "bytes_current": self.bytes_current,
        }


class FrameCache(Generic[T]):
    """LRU cache of decoded frames limited by estimated bytes."""

    def __init__(self, limit_bytes: int, *, name: str = "frames") -> None:
        if limit_bytes <= 0:
            raise MediaInputError("frame cache limit must be positive", code="cache_config")
        self.limit_bytes = limit_bytes
        self.name = name
        self._items: OrderedDict[int, tuple[T, int]] = OrderedDict()
        self._bytes = 0
        self.stats = CacheStats()

    def _size_of(self, item: T) -> int:
        size = getattr(item, "nbytes", None)
        if callable(size):
            size = size()
        if size is None:
            shape = getattr(item, "shape", None)
            size = int(shape[0] * shape[1]) if shape is not None else 1024
        return int(size)

    def put(self, key: int, item: T) -> None:
        if key in self._items:
            old, old_size = self._items.pop(key)
            self._bytes -= old_size
        size = self._size_of(item)
        if size > self.limit_bytes:
            # A single oversized frame is kept transiently but not cached.
            self.stats.evicted += 1
            return
        while self._items and self._bytes + size > self.limit_bytes:
            _, (_, evicted_size) = self._items.popitem(last=False)
            self._bytes -= evicted_size
            self.stats.evicted += 1
        self._items[key] = (item, size)
        self._bytes += size
        self.stats.inserted += 1
        self.stats.bytes_peak = max(self.stats.bytes_peak, self._bytes)
        self.stats.bytes_current = self._bytes

    def get(self, key: int) -> T | None:
        entry = self._items.get(key)
        if entry is None:
            self.stats.misses += 1
            return None
        self.stats.hits += 1
        self._items.move_to_end(key)
        return entry[0]

    def clear(self) -> None:
        self._items.clear()
        self._bytes = 0
        self.stats.bytes_current = 0

    def __len__(self) -> int:
        return len(self._items)


class AudioBlockCache(Generic[T]):
    """LRU cache of PCM blocks limited by bytes (same policy as FrameCache)."""

    def __init__(self, limit_bytes: int, *, name: str = "audio") -> None:
        self._cache: FrameCache[T] = FrameCache(limit_bytes, name=name)

    @property
    def limit_bytes(self) -> int:
        return self._cache.limit_bytes

    @property
    def stats(self) -> CacheStats:
        return self._cache.stats

    def put(self, key: int, block: T) -> None:
        self._cache.put(key, block)

    def get(self, key: int) -> T | None:
        return self._cache.get(key)

    def clear(self) -> None:
        self._cache.clear()

    def __len__(self) -> int:
        return len(self._cache)
