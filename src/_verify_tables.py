"""Readers for the MP4 sampling tables, with hostile-input bounds checks."""

from __future__ import annotations

import struct

from ._mp4_core import BoxRecord, Mp4Error


def _u32(data: bytes, offset: int) -> int:
    if offset < 0 or offset + 4 > len(data):
        raise Mp4Error("table_truncated", f"4-byte field at {offset} is past the file end", offset)
    return struct.unpack(">I", data[offset : offset + 4])[0]


def _bounded_count(data: bytes, offset: int, entry_size: int) -> int:
    """Read an entry count and refuse one the file cannot possibly hold."""
    count = _u32(data, offset)
    available = max(0, len(data) - (offset + 4))
    if count * entry_size > available:
        raise Mp4Error(
            "table_count_invalid",
            f"{count} entries of {entry_size} bytes do not fit in {available} bytes",
            offset,
        )
    return count


def _read_stts(data: bytes, box: BoxRecord) -> list[tuple[int, int]]:
    payload = box.payload_offset + 4  # FullBox version/flags
    count = _bounded_count(data, payload, 8)
    entries = []
    for index in range(count):
        at = payload + 4 + index * 8
        entries.append((_u32(data, at), _u32(data, at + 4)))
    return entries


def _read_stsz(data: bytes, box: BoxRecord) -> list[int]:
    payload = box.payload_offset + 4
    sample_size = _u32(data, payload)
    count = _u32(data, payload + 4)
    available = max(0, len(data) - (payload + 8))
    if sample_size:
        if count > 10_000_000:
            raise Mp4Error("table_count_invalid", f"stsz declares {count} samples", payload)
        return [sample_size] * count
    if count * 4 > available:
        raise Mp4Error(
            "table_count_invalid",
            f"stsz declares {count} sizes but only {available} bytes remain",
            payload,
        )
    start = payload + 8
    return [_u32(data, start + 4 * i) for i in range(count)]


def _read_stss(data: bytes, box: BoxRecord) -> list[int]:
    payload = box.payload_offset + 4
    count = _bounded_count(data, payload, 4)
    return [_u32(data, payload + 4 + 4 * i) for i in range(count)]


def _read_chunk_offsets(data: bytes, box: BoxRecord) -> list[int]:
    payload = box.payload_offset + 4
    count = _bounded_count(data, payload, 8 if box.type == b"co64" else 4)
    if box.type == b"co64":
        return [struct.unpack(">Q", data[payload + 4 + 8 * i : payload + 12 + 8 * i])[0] for i in range(count)]
    return [_u32(data, payload + 4 + 4 * i) for i in range(count)]


def _read_ctts(data: bytes, box: BoxRecord) -> tuple[int, list[tuple[int, int]]]:
    payload = box.payload_offset + 4
    count = _bounded_count(data, payload, 8)
    entries = []
    for index in range(count):
        at = payload + 4 + index * 8
        run = _u32(data, at)
        if box.version == 0:
            value = _u32(data, at + 4)
            if value >= 1 << 31:
                value -= 1 << 32
        else:
            value = struct.unpack(">i", data[at + 4 : at + 8])[0]
        entries.append((run, value))
    return box.version or 0, entries


__all__ = [
    "_bounded_count",
    "_read_chunk_offsets",
    "_read_ctts",
    "_read_stss",
    "_read_stsz",
    "_read_stts",
    "_u32",
]
