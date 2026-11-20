"""Bounded-queue pipeline, recovery policy and backpressure accounting.

Four stages -- read, demux, process, write -- are connected by fixed-capacity
:class:`BoundedQueue` instances.  A slow consumer blocks the upstream
``put`` instead of buffering without limit, so peak memory is bounded by
``queue_size`` items regardless of programme duration.  Every queue records
its peak occupancy and the cumulative time producers spent blocked, which the
CLI surfaces as ``queue_peak_items`` and a backpressure duration.

Errors travel through the same queues as data: a stage wraps a failure in a
:class:`StageError` and pushes it downstream; the policy at the other end
decides skip / silence-gap / stop, and a stop propagates a sentinel back
upstream so every worker drains and exits.
"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from enum import Enum
from queue import Full, Queue
from typing import Any, Callable

from .errors import Code, Diagnostic, FatalDamageError


class Policy(str, Enum):
    """What to do with a damaged segment."""

    SKIP = "skip"          # drop the segment, record a zero-length placeholder
    GAP = "gap"            # keep its duration as a silent time hole
    STOP = "stop"          # abort the run (this is what --strict selects)


# Damage codes each policy treats as fatal for the current run.
FATAL_UNDER_POLICY: dict[Policy, frozenset[Code]] = {
    Policy.SKIP: frozenset(),
    Policy.GAP: frozenset(),
    Policy.STOP: frozenset(
        {
            Code.SEGMENT_TRUNCATED,
            Code.REFERENCE_MISSING,
            Code.PACKET_CORRUPT,
            Code.DECODE_FAILURE,
        }
    ),
}


@dataclass
class QueueMetrics:
    name: str
    capacity: int
    peak_items: int = 0
    blocked_seconds: float = 0.0
    puts: int = 0
    gets: int = 0

    def to_json(self) -> dict[str, object]:
        return {
            "name": self.name,
            "capacity": self.capacity,
            "peak_items": self.peak_items,
            "blocked_seconds": round(self.blocked_seconds, 6),
            "puts": self.puts,
            "gets": self.gets,
        }


class BoundedQueue:
    """Fixed-capacity queue with explicit backpressure instrumentation."""

    def __init__(self, capacity: int, name: str) -> None:
        if capacity < 1:
            raise ValueError("queue capacity must be >= 1")
        self._queue: Queue[Any] = Queue(maxsize=capacity)
        self.metrics = QueueMetrics(name=name, capacity=capacity)
        self._lock = threading.Lock()

    def put(self, item: Any, *, timeout: float | None = None) -> None:
        start = time.monotonic()
        self._queue.put(item, block=True, timeout=timeout)
        waited = time.monotonic() - start
        with self._lock:
            self.metrics.blocked_seconds += waited
            self.metrics.puts += 1
            size = self._queue.qsize()
            if size > self.metrics.peak_items:
                self.metrics.peak_items = size

    def get(self, *, timeout: float | None = None) -> Any:
        item = self._queue.get(block=True, timeout=timeout)
        with self._lock:
            self.metrics.gets += 1
        return item

    def qsize(self) -> int:
        return self._queue.qsize()


SENTINEL = object()


@dataclass
class StageError:
    """A failure envelope transported downstream like any other item."""

    stage: str
    diagnostic: Diagnostic
    fatal: bool = False


@dataclass
class PipelineMetrics:
    queues: list[QueueMetrics] = field(default_factory=list)
    recovered_segments: int = 0
    skipped_segments: int = 0
    gap_segments: int = 0
    stopped_early: bool = False

    @property
    def queue_peak_items(self) -> int:
        return max((q.peak_items for q in self.queues), default=0)

    @property
    def backpressure_seconds(self) -> float:
        return round(sum(q.blocked_seconds for q in self.queues), 6)

    def to_json(self) -> dict[str, object]:
        return {
            "queue_peak_items": self.queue_peak_items,
            "backpressure_seconds": self.backpressure_seconds,
            "recovered_segments": self.recovered_segments,
            "skipped_segments": self.skipped_segments,
            "gap_segments": self.gap_segments,
            "stopped_early": self.stopped_early,
            "queues": [q.to_json() for q in self.queues],
        }


def run_stage(
    name: str,
    source: BoundedQueue | None,
    sink: BoundedQueue | None,
    handle: Callable[[Any], Any | None],
    *,
    stop_event: threading.Event,
) -> None:
    """Generic worker: pull items, call ``handle``, forward non-None results.

    A :class:`StageError` is forwarded untouched so the policy enforcement
    point is always the final stage.  Any unexpected exception from
    ``handle`` is wrapped as a fatal :class:`StageError` and forwarded; the
    worker then drains the source so the pipeline can shut down cleanly.
    """
    while True:
        if stop_event.is_set() and source is not None and source.qsize() == 0:
            if sink is not None:
                sink.put(SENTINEL)
            return
        assert source is not None
        item = source.get()
        if item is SENTINEL:
            if sink is not None:
                sink.put(SENTINEL)
            return
        if isinstance(item, StageError):
            if sink is not None:
                sink.put(item)
            continue
        try:
            result = handle(item)
        except FatalDamageError as exc:
            stop_event.set()
            if sink is not None:
                sink.put(StageError(name, exc.diagnostic(), fatal=True))
            continue
        except Exception as exc:  # defensive: keep the pipeline alive
            stop_event.set()
            diag = Diagnostic(Code.DECODE_FAILURE, f"{name} failed: {exc!r}")
            if sink is not None:
                sink.put(StageError(name, diag, fatal=True))
            continue
        if sink is not None and result is not None:
            sink.put(result)


__all__ = [
    "BoundedQueue",
    "FATAL_UNDER_POLICY",
    "PipelineMetrics",
    "Policy",
    "SENTINEL",
    "StageError",
    "run_stage",
]
