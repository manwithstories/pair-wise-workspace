"""Pipeline: bounded producer/consumer stages with backpressure and cancel.

Producer: parse (source) + transform (demux/mapping lookup).  Consumer:
mux.  The hand-off queue is bounded in both item count and total payload
bytes, so a slow consumer (e.g. a slow archive disk) applies backpressure
instead of piling up memory.  Cancelling — or an unrecoverable write
failure — drains and releases buffered frames while completed segments
stay intact in the muxer's sidecar index.
"""
from __future__ import annotations

import threading
import time
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path

from .demux import Demuxer
from .mapping import MapPlan
from .model import (
    MAX_QUEUE_BYTES,
    DemuxEvent,
    FieldPackError,
    FrameReady,
    Quarantined,
    SegmentStart,
    Stage,
)
from .mux import Muxer
from .source import iter_records


class BoundedByteQueue:
    """Blocking queue bounded by item count and total payload bytes."""

    def __init__(self, max_items: int, max_bytes: int = MAX_QUEUE_BYTES) -> None:
        self._items: deque = deque()
        self._max_items = max_items
        self._max_bytes = max_bytes
        self._bytes = 0
        self._cond = threading.Condition()
        self.peak_items = 0
        self.peak_bytes = 0
        self.producer_wait_ns = 0
        self.consumer_wait_ns = 0

    @staticmethod
    def _size_of(item) -> int:
        if isinstance(item, FrameReady):
            return len(item.record.payload)
        return 0

    def put(self, item, cancel: threading.Event) -> bool:
        """Block until there is room; returns False if cancelled first."""
        wait_start = time.monotonic_ns()
        size = self._size_of(item)
        with self._cond:
            while (
                len(self._items) >= self._max_items
                or self._bytes + size > self._max_bytes
            ):
                if cancel.is_set():
                    self.producer_wait_ns += time.monotonic_ns() - wait_start
                    return False
                self._cond.wait(timeout=0.01)
            self._items.append(item)
            self._bytes += size
            self.peak_items = max(self.peak_items, len(self._items))
            self.peak_bytes = max(self.peak_bytes, self._bytes)
            self.producer_wait_ns += time.monotonic_ns() - wait_start
            self._cond.notify_all()
        return True

    def get(self, cancel: threading.Event):
        wait_start = time.monotonic_ns()
        with self._cond:
            while not self._items:
                if cancel.is_set():
                    self.consumer_wait_ns += time.monotonic_ns() - wait_start
                    return None
                self._cond.wait(timeout=0.01)
            item = self._items.popleft()
            self._bytes -= self._size_of(item)
            self.consumer_wait_ns += time.monotonic_ns() - wait_start
            self._cond.notify_all()
        return item

    def drain(self) -> None:
        with self._cond:
            self._items.clear()
            self._bytes = 0
            self._cond.notify_all()


@dataclass
class PipelineStats:
    frames_written: int = 0
    records_read: int = 0
    corrupt_records: int = 0
    recovered_records: int = 0  # valid records accepted after the first error
    quarantined: int = 0
    queue_peak_items: int = 0
    queue_peak_bytes: int = 0
    producer_wait_ns: int = 0
    consumer_wait_ns: int = 0
    cancelled: bool = False
    errors: list[FieldPackError] = field(default_factory=list)

    @property
    def first_error_offset(self) -> int | None:
        offsets = [e.offset for e in self.errors if e.offset is not None]
        return min(offsets) if offsets else None


class Pipeline:
    def __init__(self, queue_size: int = 8, slow_consumer_ms: int = 0,
                 chunk_records: int = 64) -> None:
        self.queue_size = max(1, queue_size)
        self.slow_consumer_ms = max(0, slow_consumer_ms)
        self.chunk_records = max(1, chunk_records)
        self._cancel = threading.Event()

    def cancel(self) -> None:
        self._cancel.set()

    def run(self, source_path: str | Path, plan: MapPlan, muxer: Muxer) -> PipelineStats:
        stats = PipelineStats()
        queue: BoundedByteQueue = BoundedByteQueue(self.queue_size)
        demuxer = Demuxer()
        seen_error = False

        def produce() -> None:
            nonlocal seen_error
            chunk_left = self.chunk_records
            try:
                for item in iter_records(source_path):
                    if self._cancel.is_set():
                        return
                    chunk_left -= 1
                    if chunk_left <= 0:
                        chunk_left = self.chunk_records
                        time.sleep(0)  # yield to the consumer between chunks
                    if isinstance(item, FieldPackError):
                        stats.errors.append(item)
                        stats.corrupt_records += 1
                        seen_error = True
                        continue
                    stats.records_read += 1
                    if seen_error:
                        stats.recovered_records += 1
                    for event in demuxer.handle(item):
                        if isinstance(event, Quarantined):
                            stats.quarantined += 1
                            stats.errors.append(
                                FieldPackError(
                                    code="quarantined",
                                    message=event.reason,
                                    stage=Stage.TRANSFORM,
                                    offset=event.record.offset,
                                )
                            )
                            continue
                        if isinstance(event, FrameReady) and plan.device_for(
                            event.record.device_id
                        ) is None:
                            continue  # device not mapped: dropped by plan, not an error
                        if not queue.put(event, self._cancel):
                            return
            finally:
                queue.put(None, threading.Event())  # sentinel always fits

        def consume() -> None:
            while True:
                event = queue.get(self._cancel)
                if event is None:
                    return
                if isinstance(event, (SegmentStart,)):
                    continue
                if self.slow_consumer_ms:
                    time.sleep(self.slow_consumer_ms / 1000.0)
                try:
                    muxer.write_frame(event.record, event.pts_ns, event.segment)
                    stats.frames_written += 1
                except FieldPackError as exc:
                    stats.errors.append(exc)
                    if not exc.recoverable:
                        self._cancel.set()
                        queue.drain()
                        return

        producer = threading.Thread(target=produce, name="fieldpack-producer", daemon=True)
        producer.start()
        consume()
        producer.join(timeout=5)
        if self._cancel.is_set():
            stats.cancelled = True
            queue.drain()
        stats.queue_peak_items = queue.peak_items
        stats.queue_peak_bytes = queue.peak_bytes
        stats.producer_wait_ns = queue.producer_wait_ns
        stats.consumer_wait_ns = queue.consumer_wait_ns
        return stats
