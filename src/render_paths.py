"""Offline (whole-array) and streaming (block-by-block) render paths.

The two paths are deliberately built on the *same* operations:

* both instantiate one :class:`~src.block_convolver.BlockConvolver`;
* both feed it blocks no larger than ``block_size`` and call the same
  ``process_block`` / ``flush`` methods — no second convolution code exists;
* the only difference is who slices the blocks. The offline path pre-slices
  the complete array, the streaming path iterates a block iterator, so their
  arithmetic is identical by construction (the measured path error is 0).

Every emitted chunk produces a :class:`BlockRecord` with timing, absolute
input/output sample ranges and peak, which is what makes block boundaries,
tail padding and peak anomalies diagnosable.
"""
from __future__ import annotations

import time
from dataclasses import dataclass, field

import numpy as np

from .block_convolver import BlockConvolver
from .config import StreamBlockError


@dataclass
class BlockRecord:
    index: int
    kind: str                 # "block" for source blocks, "tail" for flush
    input_start: int
    input_end: int            # half-open; tail chunks carry no new input
    output_start: int
    output_end: int
    samples: int
    peak: float
    has_nan: bool
    has_inf: bool
    peak_over_one: bool
    time_ms: float


@dataclass
class RenderResult:
    output: np.ndarray
    records: list = field(default_factory=list)

    @property
    def block_count(self) -> int:
        return len(self.records)


def _make_record(index, kind, in_start, in_end, out_start, chunk, t0, t1):
    flat = np.asarray(chunk, dtype=np.float64)
    peak = float(np.max(np.abs(flat))) if flat.size else 0.0
    return BlockRecord(
        index=index,
        kind=kind,
        input_start=in_start,
        input_end=in_end,
        output_start=out_start,
        output_end=out_start + flat.shape[0],
        samples=flat.shape[0],
        peak=peak,
        has_nan=bool(np.any(np.isnan(flat))),
        has_inf=bool(np.any(np.isinf(flat))),
        peak_over_one=bool(peak > 1.0),
        time_ms=(t1 - t0) * 1000.0,
    )


def _drive(ir, block_iter, block_size, on_error=None):
    """Shared engine: run one convolver over an iterable of input blocks."""
    core = BlockConvolver(ir, block_size)
    chunks, records = [], []
    out_pos = 0
    in_pos = 0
    index = 0

    for block in block_iter:
        block = np.asarray(block, dtype=np.float64)
        length = block.shape[0]
        try:
            t0 = time.perf_counter()
            chunk = core.process_block(block)
            t1 = time.perf_counter()
        except Exception as exc:  # structured failure: stop writing at once
            details = {
                "block_index": index,
                "input_start": in_pos,
                "input_end": in_pos + length,
                "samples_written": out_pos,
                "error_type": type(exc).__name__,
                "message": str(exc),
            }
            if on_error is not None:
                on_error(details)
            raise StreamBlockError(
                f"streaming block {index} failed; stopped after "
                f"{out_pos} output samples", details
            ) from exc
        records.append(_make_record(
            index, "block", in_pos, in_pos + length, out_pos, chunk, t0, t1
        ))
        chunks.append(chunk)
        out_pos += chunk.shape[0]
        in_pos += length
        index += 1

    t0 = time.perf_counter()
    tail = core.flush()
    t1 = time.perf_counter()
    # Split the IR tail into block-sized chunks so record boundaries stay
    # aligned with the configured block size all the way to the last sample.
    for offset in range(0, tail.shape[0], block_size):
        piece = tail[offset:offset + block_size]
        records.append(_make_record(
            index, "tail", in_pos, in_pos, out_pos, piece, t0, t1
        ))
        chunks.append(piece)
        out_pos += piece.shape[0]
        index += 1

    output = np.concatenate(chunks, axis=0) if chunks else tail
    return RenderResult(output=output, records=records)


def render_offline(source: np.ndarray, ir: np.ndarray, block_size: int):
    """Whole-array path: pre-slice the full input, then run the shared core."""
    source = np.asarray(source, dtype=np.float64)
    slices = (source[i:i + block_size]
              for i in range(0, source.shape[0], block_size))
    return _drive(ir, slices, block_size)


def render_streaming(source: np.ndarray, ir: np.ndarray, block_size: int,
                     on_error=None):
    """Streaming path: lazily iterate blocks, one at a time, through the core.

    A block failure raises :class:`StreamBlockError` with structured details
    after no further chunks are emitted, giving a non-zero CLI exit path.
    """
    source = np.asarray(source, dtype=np.float64)

    def block_iter():
        for i in range(0, source.shape[0], block_size):
            yield source[i:i + block_size]

    return _drive(ir, block_iter(), block_size, on_error=on_error)
