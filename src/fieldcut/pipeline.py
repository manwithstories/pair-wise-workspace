"""Bounded-queue pipeline orchestration with backpressure and cancellation.

Stages are connected by fixed-capacity queues whose size is independent of
media duration: a slow downstream stage fills its input queue and the upstream
producer then blocks on ``put``, which is the backpressure signal. A
cancellation event propagates to every stage, and a failure in one stage is
surfaced through an error future rather than silently killing a worker thread.

The media work itself (PyAV decode/encode) releases the GIL around native
calls, so threaded stages genuinely overlap I/O and encoding while sharing
the :class:`~fieldcut.errors.ErrorLog` for recovery state.
"""

from __future__ import annotations

import queue
import threading
from dataclasses import dataclass, field

from .errors import ErrorLog, FieldcutError, Recovery, Stage

# Module-level sentinels. QUEUE_CLOSED marks end-of-input on a bounded queue.
QUEUE_CLOSED = object()


class PipelineCancelled(FieldcutError):
    """Raised when an external cancellation reaches a running stage."""


class BoundedQueue:
    """Fixed-capacity queue; ``put`` blocks (backpressure) when full.

    Capacity is a frame/packet count, never a duration-derived value, so memory
    stays bounded regardless of recording length.
    """

    def __init__(self, capacity: int):
        if capacity < 1:
            raise ValueError("queue capacity must be >= 1")
        self._q: queue.Queue = queue.Queue(maxsize=capacity)
        self.capacity = capacity
        self.peak_size = 0

    def put(self, item, cancel: threading.Event) -> None:
        while True:
            if cancel.is_set():
                raise PipelineCancelled("pipeline cancelled during put")
            try:
                self._q.put(item, timeout=0.05)
                self.peak_size = max(self.peak_size, self._q.qsize())
                return
            except queue.Full:
                continue  # backpressure: retry until space or cancellation

    def get(self, cancel: threading.Event):
        while True:
            if cancel.is_set():
                # Cancellation winds the whole chain down through the same
                # closed-marker path as normal end-of-input.
                return QUEUE_CLOSED
            try:
                item = self._q.get(timeout=0.05)
                return item
            except queue.Empty:
                continue

    def close(self, cancel: threading.Event) -> None:
        """Signal end-of-input; never blocks, even on a full queue."""
        try:
            self._q.put_nowait(QUEUE_CLOSED)
            self.peak_size = max(self.peak_size, self._q.qsize())
        except queue.Full:
            # Make room by dropping one buffered item; cancellation/error
            # means the in-flight data is being discarded anyway.
            try:
                self._q.get_nowait()
            except queue.Empty:
                pass
            self._q.put_nowait(QUEUE_CLOSED)

    def drain(self) -> list:
        out = []
        while not self._q.empty():
            try:
                out.append(self._q.get_nowait())
            except queue.Empty:
                break
        return out


@dataclass
class StageStatus:
    name: str
    processed: int = 0
    recovered: int = 0
    failed: int = 0
    peak_queue: int = 0
    error: str | None = None


@dataclass
class PipelineState:
    cancel: threading.Event
    log: ErrorLog
    statuses: dict[str, StageStatus] = field(default_factory=dict)
    threads: dict[str, threading.Thread] = field(default_factory=dict)
    lock: threading.Lock = field(default_factory=threading.Lock)

    def register_thread(self, name: str, thread: threading.Thread) -> None:
        with self.lock:
            self.threads[name] = thread

    def shutdown(self, wait: float = 5.0) -> None:
        """Signal cancellation and join every worker within ``wait`` seconds."""
        self.cancel.set()
        for thread in list(self.threads.values()):
            thread.join(timeout=wait)

    @property
    def any_failed(self) -> bool:
        return any(s.failed > 0 for s in self.statuses.values())

    def record_recovery(self, stage: Stage, camera: str, message: str,
                        media_time_ticks: int | None = None) -> None:
        with self.lock:
            self.log.add(stage, camera, message,
                         media_time_ticks=media_time_ticks,
                         recovery=Recovery.RETRIED)

    def fail_stage(self, name: str, exc: BaseException) -> None:
        with self.lock:
            status = self.statuses.setdefault(name, StageStatus(name))
            status.failed += 1
            status.error = f"{type(exc).__name__}: {exc}"
        self.cancel.set()


def run_stage(name: str, process_item, in_q: BoundedQueue | None,
              out_q: BoundedQueue | None, state: PipelineState) -> threading.Thread:
    """Wire one worker thread between an input and an output bounded queue."""
    state.statuses.setdefault(name, StageStatus(name))

    def _worker() -> None:
        try:
            while True:
                if in_q is None:
                    item = QUEUE_CLOSED
                else:
                    item = in_q.get(state.cancel)
                if item is QUEUE_CLOSED:
                    break
                try:
                    results = process_item(item, state) or []
                except PipelineCancelled:
                    raise
                except Exception as exc:  # stage-level recovery boundary
                    # Record and surface via state; never let a traceback
                    # escape the worker (the driver inspects any_failed).
                    state.fail_stage(name, exc)
                    state.log.add(Stage.PIPELINE, name,
                                  f"stage item failed: {exc}",
                                  recovery=Recovery.UNRECOVERABLE)
                    break
                for result in results:
                    if out_q is not None:
                        out_q.put(result, state.cancel)
                with state.lock:
                    state.statuses[name].processed += 1
        except PipelineCancelled:
            pass
        finally:
            # Always release the next stage, even on cancel or failure.
            if out_q is not None:
                out_q.close(state.cancel)

    thread = threading.Thread(target=_worker, name=f"fieldcut-{name}",
                              daemon=False)
    thread.start()
    state.register_thread(name, thread)
    return thread
