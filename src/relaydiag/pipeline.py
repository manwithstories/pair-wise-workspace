"""Bounded-queue streaming pipeline with backpressure and checkpointing.

Stages are plain callables connected by :class:`BoundedQueue` instances.  A
full queue blocks the upstream producer (backpressure), so a slow consumer
keeps live memory at ``capacity * item_size`` regardless of input length.
Each carried item includes its source byte offset, which lets the pipeline
persist an accurate resume position whenever:

* a queue saturates,
* the input stream is truncated (:class:`EOFError`),
* a downstream disk write fails (:class:`OSError`).

A cancellation event makes shutdown deadlock-free: when any stage fails, every
blocked producer/worker is woken and drains out, so a crashed run always
terminates after writing its last safe checkpoint.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Callable, Iterable

from .checkpoint import CheckpointState, save_checkpoint

#: Module-level stop marker; identity-compared so no payload can imitate it.
STOP = object()


class PipelineError(RuntimeError):
    """Wraps a stage failure together with the last safe checkpoint offset."""

    def __init__(self, message: str, last_offset: int, cause: Exception | None = None):
        super().__init__(message)
        self.last_offset = last_offset
        self.cause = cause


class BoundedQueue:
    """Blocking bounded queue supporting cancellation-aware shutdown."""

    def __init__(self, capacity: int) -> None:
        if capacity < 1:
            raise ValueError("queue capacity must be >= 1")
        self.capacity = capacity
        self._items: list = []
        self._lock = threading.Lock()
        self._not_full = threading.Condition(self._lock)
        self._not_empty = threading.Condition(self._lock)
        self.full_events = 0

    def put(self, item, cancel: threading.Event | None = None) -> bool:
        """Block while full; return False if cancelled before enqueueing."""
        with self._not_full:
            if len(self._items) >= self.capacity:
                self.full_events += 1
            while len(self._items) >= self.capacity:
                if cancel is not None and cancel.is_set():
                    return False
                self._not_full.wait(timeout=0.5)
                if cancel is not None and cancel.is_set():
                    return False
            self._items.append(item)
            self._not_empty.notify()
            return True

    def get(self, cancel: threading.Event | None = None):
        with self._not_empty:
            while not self._items:
                if cancel is not None and cancel.is_set():
                    return STOP
                self._not_empty.wait(timeout=0.5)
            item = self._items.pop(0)
            self._not_full.notify()
            return item

    def drain(self) -> None:
        with self._lock:
            self._items.clear()
            self._not_full.notify_all()
            self._not_empty.notify_all()

    def __len__(self) -> int:
        with self._lock:
            return len(self._items)


@dataclass(slots=True)
class StreamItem:
    offset: int
    payload: object


def run_pipeline(source: Iterable[StreamItem],
                 stages: list[Callable[[StreamItem], StreamItem | None]],
                 state: CheckpointState,
                 checkpoint_path: str,
                 capacity: int = 8,
                 checkpoint_every: int = 32) -> CheckpointState:
    """Push ``source`` through ``stages`` with bounded buffers and backpressure."""
    queues = [BoundedQueue(capacity) for _ in range(len(stages) + 1)]
    cancel = threading.Event()
    finalized = threading.Event()
    errors: list[BaseException] = []
    collected: list[StreamItem] = []

    def fail(exc: BaseException) -> None:
        errors.append(exc)
        finalized.set()
        cancel.set()
        for q in queues:
            q.drain()

    def collector() -> None:
        final_q = queues[-1]
        processed = 0
        while True:
            item = final_q.get(cancel)
            if item is STOP:
                state.packets_processed = processed
                state.collected_count = processed
                return
            collected.append(item)
            processed += 1
            # Once a failure is finalised, the failing stage owns the resume
            # offset; the collector must not advance or overwrite it.
            if finalized.is_set():
                continue
            state.next_offset = item.offset
            if processed % checkpoint_every == 0:
                _persist(state, checkpoint_path, item.offset, finalized)

    def producer() -> None:
        try:
            for item in source:
                if not queues[0].put(item, cancel):
                    return
                if queues[0].full_events:
                    _persist(state, checkpoint_path, item.offset, finalized)
        except EOFError as exc:
            fail(exc)
        except OSError as exc:
            fail(exc)
        finally:
            queues[0].put(STOP)

    def worker(index: int, stage: Callable) -> None:
        in_q, out_q = queues[index], queues[index + 1]
        while True:
            item = in_q.get(cancel)
            if item is STOP:
                out_q.put(STOP)
                return
            try:
                result = stage(item)
            except EOFError as exc:
                state.next_offset = item.offset
                fail(exc)
                _persist(state, checkpoint_path, item.offset, finalized)
                out_q.put(STOP)
                return
            except OSError as exc:
                state.next_offset = item.offset
                fail(PipelineError(
                    f"stage {index} disk/IO failure", item.offset, exc))
                _persist(state, checkpoint_path, item.offset, finalized)
                out_q.put(STOP)
                return
            if result is not None and not out_q.put(result, cancel):
                return

    threads = [threading.Thread(target=producer, name="producer")]
    threads += [
        threading.Thread(target=worker, args=(i, stage), name=f"stage-{i}")
        for i, stage in enumerate(stages)
    ]
    threads.append(threading.Thread(target=collector, name="collector"))
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=30)
        if t.is_alive():
            fail(PipelineError(f"stage {t.name} did not terminate",
                               state.next_offset))
            t.join(timeout=5)
    for exc in errors:
        if isinstance(exc, PipelineError):
            raise exc
    if errors:
        raise PipelineError(str(errors[0]), state.next_offset, errors[0])
    state.collected_count = len(collected)
    state.packets_processed = len(collected)
    _persist(state, checkpoint_path, state.next_offset)
    return state


def _persist(state: CheckpointState, path: str, offset: int,
             finalized: threading.Event | None = None) -> None:
    # Once a failure is finalised, only the failing item's offset may be
    # written; later collector/backpressure calls must not move the position.
    if finalized is not None and finalized.is_set() and offset != state.next_offset:
        return
    state.next_offset = offset
    save_checkpoint(path, state)
