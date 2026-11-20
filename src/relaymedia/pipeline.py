"""Bounded, back-pressured, block-streaming stage orchestration.

Stages communicate through :class:`BoundedQueue` instances with both an item
capacity and a byte budget. Decoded blocks are split until every queued chunk
is smaller than the budget, so no single message can exceed the limit and the
pipeline can always make progress. A slow sink fills its queue, the full queue
blocks the stage feeding it, and that stage's own queue then fills upstream —
back-pressure propagates all the way to the decoder instead of buffering the
whole run in memory. A shared counter records live and peak queued bytes, so
the run summary can prove the budget was never exceeded.

Decoding errors carry stage, input byte offset and media time; they travel as
ordinary zero-byte messages and processing continues with the next record.
Checkpoints bind input and parameter digests, so resume reuses only still-
valid work and any drift is refused upstream.
"""

from __future__ import annotations

import threading
from collections import deque
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import numpy as np

from . import audio as audio_mod
from . import checkpoint as cp_mod
from .audio import AudioBlock
from .errors import ErrorRecord


@dataclass(frozen=True, slots=True)
class PipelineConfig:
    out_rate: int = 48000
    out_channels: int = 2
    window_ms: float = 50.0
    silence_threshold: float = 0.01
    fp_window_ms: float = 20.0
    queue_items: int = 8
    queue_bytes: int = 1 << 20
    timeline_offset: Fraction = Fraction(0)


class BufferAccountant:
    """Live and peak byte counts shared across every bounded queue."""

    def __init__(self, limit_bytes: int) -> None:
        self.limit_bytes = max(1, limit_bytes)
        self.current = 0
        self.peak = 0
        # One condition for every queue so all stages wake on budget changes.
        self.condition = threading.Condition()


class BoundedQueue:
    """Item + byte bounded queue; put blocks while either budget is full."""

    def __init__(self, max_items: int, accountant: BufferAccountant) -> None:
        self._items: deque = deque()
        self._max_items = max(1, max_items)
        self._accountant = accountant
        self._condition = accountant.condition

    def put(self, item: object, nbytes: int) -> None:
        nbytes = min(nbytes, self._accountant.limit_bytes)
        cond = self._condition
        with cond:
            while (
                len(self._items) >= self._max_items
                or self._accountant.current + nbytes > self._accountant.limit_bytes
            ):
                cond.wait()
            self._items.append((item, nbytes))
            self._accountant.current += nbytes
            self._accountant.peak = max(self._accountant.peak, self._accountant.current)
            cond.notify_all()

    def get(self):
        cond = self._condition
        with cond:
            while not self._items:
                cond.wait()
            item, nbytes = self._items.popleft()
            self._accountant.current -= nbytes
            cond.notify_all()
            return item


@dataclass(slots=True)
class RunResult:
    work_dir: str
    segment_count: int = 0
    duplicate_count: int = 0
    bad_record_count: int = 0
    first_error_offset_bytes: int = -1
    peak_buffer_bytes: int = 0
    buffer_limit_bytes: int = 0
    resumed: bool = False
    output_sha256: str = ""
    errors: list[ErrorRecord] = field(default_factory=list)
    channel_maps: list = field(default_factory=list)
    color_reports: list = field(default_factory=list)
    silence_intervals: list = field(default_factory=list)
    segments: list = field(default_factory=list)
    duplicates: list = field(default_factory=list)
    output_files: list[str] = field(default_factory=list)


def split_block(block: AudioBlock, max_bytes: int) -> list[AudioBlock]:
    """Split an audio block on sample rows until each chunk fits max_bytes."""
    row_bytes = int(block.data.strides[0]) if block.data.ndim == 2 else block.data.itemsize
    rows_per = max(1, max_bytes // max(1, row_bytes))
    if block.data.nbytes <= max_bytes:
        return [block]
    out: list[AudioBlock] = []
    total = block.data.shape[0]
    for start in range(0, total, rows_per):
        end = min(total, start + rows_per)
        out.append(AudioBlock(block.data[start:end].copy(), block.source,
                              block.src_start + start, block.src_start + end))
    return out


def run_pipeline(
    *,
    sources: list[tuple[str, str]],
    subtitle_path: str,
    video_paths: list[str],
    work_dir: str,
    config: PipelineConfig,
    input_digest: str,
    params_digest: str,
    probe_color_fn=None,
    open_video_fn=None,
) -> RunResult:
    """Stream decode -> chunked forward -> streaming sink, fully back-pressured."""
    import av

    from .archive import StreamSink, finalize
    from .color import probe_color as default_probe

    work = Path(work_dir)
    (work / "segments").mkdir(parents=True, exist_ok=True)
    result = RunResult(work_dir=str(work), buffer_limit_bytes=config.queue_bytes)
    accountant = BufferAccountant(config.queue_bytes)
    q_decode = BoundedQueue(config.queue_items, accountant)
    q_sink = BoundedQueue(config.queue_items, accountant)
    SENTINEL = object()

    checkpoint = cp_mod.load_checkpoint(work)
    if checkpoint is not None:
        checkpoint = cp_mod.resume_checkpoint(
            work, input_digest=input_digest, params_digest=params_digest
        )
        result.resumed = True
    else:
        checkpoint = cp_mod.new_checkpoint(work, input_digest, params_digest)

    def decoder():
        for node_id, path in sources:
            decoded = audio_mod.decode_source(path, config.out_rate, config.out_channels)
            result.channel_maps.extend(decoded.channel_maps)
            for block in decoded.blocks:
                for chunk in split_block(block, config.queue_bytes):
                    q_decode.put((node_id, chunk), int(chunk.data.nbytes))
            for err in decoded.errors:
                q_decode.put(("error", err), 0)
        q_decode.put(SENTINEL, 0)

    def forwarder():
        # Pure pass-through stage: a slow sink stalls here, which stalls decode.
        while True:
            item = q_decode.get()
            if item is SENTINEL:
                q_sink.put(SENTINEL, 0)
                break
            if isinstance(item, tuple) and item[0] == "error":
                q_sink.put(item, 0)
                continue
            node_id, block = item
            q_sink.put(item, int(block.data.nbytes))

    sink = StreamSink(work, config)

    def sink_thread():
        while True:
            item = q_sink.get()
            if item is SENTINEL:
                sink.finish_node()
                break
            if isinstance(item, tuple) and item[0] == "error":
                sink.errors.append(item[1])
                continue
            node_id, block = item
            sink.push(node_id, block)

    threads = [
        threading.Thread(target=decoder, name="decode", daemon=True),
        threading.Thread(target=forwarder, name="forward", daemon=True),
        threading.Thread(target=sink_thread, name="sink", daemon=True),
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    result.peak_buffer_bytes = accountant.peak
    finalize(
        result, sink, checkpoint, config, sources, subtitle_path, video_paths,
        probe_color_fn or default_probe, open_video_fn or av.open,
    )
    return result
