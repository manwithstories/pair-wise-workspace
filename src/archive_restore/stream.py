"""Bounded, back-pressured in-process pipeline.

Stages communicate through queues capped by *both* an item count and a byte
budget.  A slow consumer fills its input queue; the upstream stage then blocks
on ``put`` instead of buffering without limit.  Cancellation and failure travel
through the stage boundaries via explicit sentinels so every worker drains and
stops rather than losing work or hanging.
"""

from __future__ import annotations

import enum
import queue
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from typing import Any, Callable

from .errors import ErrorStage, PipelineError, retryable


class Control(enum.Enum):
    END = "end"          # orderly shutdown: no more items will arrive
    CANCEL = "cancel"    # external cancellation request
    FAIL = "fail"        # upstream fatal failure


@dataclass
class StreamItem:
    """Unit of work flowing between stages; byte_size drives the byte budget."""

    key: str
    payload: Any
    byte_size: int
    enqueued_at: float = field(default_factory=time.perf_counter)
    stage: str = ""
    attempt: int = 0


class PipelineCancelled(Exception):
    """Raised inside a stage when a cancellation sentinel reaches it."""


class BoundedQueue:
    """Queue bounded by item count and cumulative payload bytes."""

    def __init__(self, max_items: int, max_bytes: int, name: str = "") -> None:
        self._q: queue.Queue[StreamItem | Control] = queue.Queue()
        self.max_items = max_items
        self.max_bytes = max_bytes
        self.name = name
        self._bytes = 0
        self.peak_bytes = 0
        self._lock = threading.Lock()
        self._writable = threading.Condition(self._lock)

    def put(self, item: StreamItem, token: "CancellationToken") -> None:
        with self._writable:
            while (self._q.qsize() >= self.max_items or
                   self._bytes + item.byte_size > self.max_bytes):
                if token.cancelled:
                    raise PipelineCancelled(f"queue {self.name} put cancelled")
                self._writable.wait(timeout=0.1)
            self._q.put(item)
            self._bytes += item.byte_size
            self.peak_bytes = max(self.peak_bytes, self._bytes)

    def get(self, token: "CancellationToken") -> StreamItem | Control:
        while True:
            try:
                item = self._q.get(timeout=0.1)
            except queue.Empty:
                if token.cancelled:
                    return Control.CANCEL
                continue
            if isinstance(item, StreamItem):
                with self._lock:
                    self._bytes -= item.byte_size
                    self._writable.notify_all()
            return item

    def signal(self, control: Control) -> None:
        self._q.put(control)

    def qsize(self) -> int:
        return self._q.qsize()


class CancellationToken:
    """Thread-safe cancellation flag observed by every blocking call."""

    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()


StageFn = Callable[[StreamItem, CancellationToken], StreamItem | None]


class Pipeline:
    """Linear sequence of bounded queues with per-stage worker threads."""

    def __init__(self, max_items: int = 64, max_bytes: int = 64 * 1024 * 1024) -> None:
        self.max_items = max_items
        self.max_bytes = max_bytes
        self.queues: list[BoundedQueue] = []
        self.stages: list[tuple[str, StageFn]] = []
        self.token = CancellationToken()
        self.latencies_ms: deque[float] = deque()
        self.peak_queue_bytes = 0
        self._threads: list[threading.Thread] = []
        self._error: PipelineError | None = None
        self._error_lock = threading.Lock()

    def add_stage(self, name: str, fn: StageFn) -> "Pipeline":
        self.stages.append((name, fn))
        self.queues.append(BoundedQueue(self.max_items, self.max_bytes, name))
        return self

    @property
    def input_queue(self) -> BoundedQueue:
        if not self.queues:
            raise RuntimeError("pipeline has no stages")
        return self.queues[0]

    def submit(self, item: StreamItem) -> None:
        if self.token.cancelled:
            raise PipelineCancelled("pipeline already cancelled")
        item.stage = self.stages[0][0]
        self.input_queue.put(item, self.token)

    def _record_error(self, exc: BaseException, asset: str) -> None:
        with self._error_lock:
            if self._error is None:
                if isinstance(exc, PipelineError):
                    self._error = exc
                else:
                    self._error = PipelineError(retryable(
                        ErrorStage.STREAM, asset, f"{type(exc).__name__}: {exc}"))
        self.token.cancel()

    def _worker(self, index: int) -> None:
        name, fn = self.stages[index]
        in_q = self.queues[index]
        out_q = self.queues[index + 1] if index + 1 < len(self.queues) else None
        terminator = Control.END
        while True:
            item = in_q.get(self.token)
            if isinstance(item, Control):
                terminator = Control.CANCEL if (item is Control.CANCEL or self.token.cancelled) else item
                if item is Control.FAIL:
                    terminator = Control.FAIL
                break
            started = time.perf_counter()
            try:
                result = fn(item, self.token)
            except PipelineCancelled:
                terminator = Control.CANCEL
                break
            except Exception as exc:  # stage boundary: failure propagates downstream
                self._record_error(exc, item.key)
                terminator = Control.FAIL
                break
            self.latencies_ms.append((time.perf_counter() - started) * 1000.0)
            if result is not None and out_q is not None:
                result.stage = self.stages[index + 1][0]
                try:
                    out_q.put(result, self.token)
                except PipelineCancelled:
                    terminator = Control.CANCEL
                    break
        if out_q is not None:
            out_q.signal(Control.FAIL if terminator is Control.FAIL
                         else Control.CANCEL if terminator is Control.CANCEL else Control.END)

    def run(self) -> None:
        """Start all workers; the caller feeds :meth:`submit` from this thread."""
        self._threads = [threading.Thread(target=self._worker, args=(i,), name=name, daemon=True)
                         for i, (name, _) in enumerate(self.stages)]
        for t in self._threads:
            t.start()

    def finish(self) -> None:
        """Close the input and wait for every worker to drain."""
        self.input_queue.signal(Control.CANCEL if self.token.cancelled else Control.END)
        for t in self._threads:
            t.join()
        self.peak_queue_bytes = max(q.peak_bytes for q in self.queues)
        if self._error is not None:
            raise self._error

    def cancel(self) -> None:
        self.token.cancel()
