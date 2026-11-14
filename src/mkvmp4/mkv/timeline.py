"""Block parsing, lace expansion and exact sample-time reconstruction."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable, Sequence

from mkvmp4.ebml.vint import VINTError, decode_signed, decode_vint


class TimelineError(ValueError):
    pass


@dataclass(slots=True)
class BlockHeader:
    track_number: int
    relative_ticks: int
    flags: int
    lacing: int
    keyframe: bool
    payload_offset: int


def parse_block_header(data: bytes | bytearray | memoryview) -> BlockHeader:
    if len(data) < 4:
        raise TimelineError("truncated block")
    try:
        track, track_width, unknown = decode_vint(data)
    except VINTError as exc:
        raise TimelineError(str(exc)) from exc
    if unknown or track <= 0:
        raise TimelineError("invalid block track number")
    if len(data) < track_width + 3:
        raise TimelineError("truncated block header")
    relative = int.from_bytes(data[track_width:track_width + 2], "big", signed=True)
    flags = data[track_width + 2]
    return BlockHeader(track, relative, flags, (flags >> 1) & 0x03,
                       bool(flags & 0x80), track_width + 3)


def _frame_sizes(payload: memoryview, lacing: int, count: int) -> tuple[list[int], int]:
    if lacing == 0:
        return [len(payload)], 0
    if count < 1:
        raise TimelineError("invalid lace frame count")
    cursor = 1
    sizes: list[int] = []
    if lacing == 1:  # Xiph
        for _ in range(count - 1):
            size = 0
            while True:
                if cursor >= len(payload):
                    raise TimelineError("truncated Xiph lace")
                byte = payload[cursor]
                cursor += 1
                size += byte
                if byte != 255:
                    break
            sizes.append(size)
    elif lacing == 3:  # EBML
        if cursor >= len(payload):
            raise TimelineError("truncated EBML lace")
        first_size, width, unknown = decode_vint(payload, cursor)
        if unknown:
            raise TimelineError("unknown-size EBML lace")
        cursor += width
        sizes.append(first_size)
        for _ in range(count - 2):
            if cursor + width > len(payload):
                raise TimelineError("truncated EBML lace delta")
            delta = decode_signed(payload[cursor:cursor + width])
            cursor += width
            sizes.append(sizes[-1] + delta)
    elif lacing == 2:  # fixed size; handled by caller
        pass
    else:
        raise TimelineError("invalid lacing mode")
    used = cursor + sum(sizes)
    if used > len(payload):
        raise TimelineError("lace sizes exceed block payload")
    sizes.append(len(payload) - used)
    if any(size < 0 for size in sizes):
        raise TimelineError("negative lace frame size")
    return sizes, cursor


def expand_laces(data: bytes | bytearray | memoryview) -> tuple[BlockHeader, list[memoryview]]:
    """Expand no/Xiph/fixed/EBML lacing into individual frame views."""
    view = memoryview(data)
    header = parse_block_header(view)
    payload = view[header.payload_offset:]
    if header.lacing == 0:
        return header, [payload]
    if not payload:
        raise TimelineError("missing lace count")
    count = payload[0] + 1
    if header.lacing == 2:
        if count <= 0 or len(payload) - 1 < count:
            raise TimelineError("truncated fixed-size lace")
        frame_size = (len(payload) - 1) // count
        sizes = [frame_size] * (count - 1)
        sizes.append(len(payload) - 1 - frame_size * (count - 1))
        data_start = 1
    else:
        sizes, data_start = _frame_sizes(payload, header.lacing, count)
    cursor = data_start
    frames: list[memoryview] = []
    for size in sizes:
        frames.append(payload[cursor:cursor + size])
        cursor += size
    return header, frames


def ns_to_units(ns: int, timescale: int) -> int:
    """Round a cumulative nanosecond value to an output timescale once."""
    if timescale <= 0:
        raise TimelineError("timescale must be positive")
    magnitude = (abs(int(ns)) * timescale + 500_000_000) // 1_000_000_000
    return magnitude if ns >= 0 else -magnitude


@dataclass(slots=True)
class SampleTime:
    pts_ns: int
    duration_ns: int
    keyframe: bool
    cluster_index: int


def cumulative_sample_times(
    block_times_ns: Sequence[tuple[int, int, int]],
    counts: Sequence[int],
    nominal_durations_ns: Sequence[int],
    keyframes: Sequence[bool],
    cluster_indices: Sequence[int],
) -> list[SampleTime]:
    """Expand block starts to samples using cumulative, not per-frame, rounding.

    Each tuple is ``(start_ns, explicit_block_duration_ns, fallback_duration_ns)``.
    Explicit duration applies to a whole block and is divided among its frames.
    """
    if not (len(block_times_ns) == len(counts) == len(nominal_durations_ns)):
        raise TimelineError("timeline arrays have different lengths")
    result: list[SampleTime] = []
    for index, ((start, explicit, fallback), count, nominal) in enumerate(
            zip(block_times_ns, counts, nominal_durations_ns)):
        if count <= 0:
            raise TimelineError("block has no frames")
        total = explicit if explicit is not None and explicit > 0 else nominal
        if total is None or total <= 0:
            if index + 1 < len(block_times_ns):
                total = max(1, (block_times_ns[index + 1][0] - start) // count)
            else:
                total = max(1, fallback // count)
        frame_duration = max(1, int(round(total / count)))
        for frame in range(count):
            result.append(SampleTime(start + frame * frame_duration, frame_duration,
                                     keyframes[index], cluster_indices[index]))
    return result

