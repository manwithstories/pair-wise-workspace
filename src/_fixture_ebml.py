"""Low-level EBML element and Block writers used by the fixture builder."""

from __future__ import annotations

import struct

from . import ebml_schema as ids
from .ebml_schema import encode_float, encode_uint, encode_unknown_size, encode_vint


def elem(eid: bytes, payload: bytes) -> bytes:
    """Encode one known-size EBML element."""
    return eid + encode_vint(len(payload)) + payload


def elem_unknown(eid: bytes, payload: bytes, *, width: int = 8) -> bytes:
    """Encode one unknown-size EBML element (marker + all-ones size)."""
    return eid + encode_unknown_size(width) + payload


def uint_elem(eid: bytes, value: int) -> bytes:
    return elem(eid, encode_uint(value))


def float_elem(eid: bytes, value: float) -> bytes:
    return elem(eid, encode_float(value))


def string_elem(eid: bytes, value: str, *, utf8: bool = False) -> bytes:
    return elem(eid, value.encode("utf-8" if utf8 else "ascii"))


def binary_elem(eid: bytes, payload: bytes) -> bytes:
    return elem(eid, payload)


def simple_block(
    track_number: int,
    relative: int,
    data: bytes,
    *,
    keyframe: bool = True,
    invisible: bool = False,
    discardable: bool = False,
) -> bytes:
    flags = 0
    if keyframe:
        flags |= 0x80
    if invisible:
        flags |= 0x08
    if discardable:
        flags |= 0x01
    payload = encode_vint(track_number) + struct.pack(">h", relative) + bytes([flags]) + data
    return elem(ids.SIMPLE_BLOCK, payload)


def block_group(
    track_number: int,
    relative: int,
    data: bytes,
    *,
    duration: int | None = None,
    references: tuple[int, ...] = (),
) -> bytes:
    payload = encode_vint(track_number) + struct.pack(">h", relative) + b"\x00" + data
    inner = binary_elem(ids.BLOCK, payload)
    if duration is not None:
        inner += uint_elem(ids.BLOCK_DURATION, duration)
    for ref in references:
        inner += elem(ids.REFERENCE_BLOCK, ref.to_bytes(2, "big", signed=True))
    return elem(ids.BLOCK_GROUP, inner)
