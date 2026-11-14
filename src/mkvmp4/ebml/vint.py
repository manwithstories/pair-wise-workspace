"""EBML variable-length integers.

Element IDs keep their marker bit.  Data sizes discard it.  A size whose value
bits are all ones is the reserved unknown-size sentinel.
"""

from __future__ import annotations

from typing import BinaryIO


class VINTError(ValueError):
    pass


def vint_length(first: int) -> int:
    if first == 0:
        raise VINTError("VINT marker bit is missing")
    return 9 - first.bit_length()


def vint_capacity(length: int) -> int:
    if not 1 <= length <= 8:
        raise VINTError("VINT width must be 1..8")
    return (1 << (7 * length)) - 1


def decode_vint(data: bytes | bytearray | memoryview, offset: int = 0) -> tuple[int, int, bool]:
    """Decode an EBML data size, returning ``(value, width, unknown)``."""
    if offset < 0 or offset >= len(data):
        raise VINTError("truncated VINT")
    first = data[offset]
    if first == 0:
        raise VINTError("invalid VINT first byte 0x00")
    width = vint_length(first)
    if offset + width > len(data):
        raise VINTError("truncated VINT")
    value = first & ((1 << (8 - width)) - 1)
    for byte in data[offset + 1 : offset + width]:
        value = (value << 8) | byte
    return value, width, value == vint_capacity(width)


def read_vint(stream: BinaryIO) -> tuple[int, int, bool]:
    first = stream.read(1)
    if not first:
        raise VINTError("unexpected EOF while reading VINT")
    width = vint_length(first[0])
    rest = stream.read(width - 1)
    if len(rest) != width - 1:
        raise VINTError("truncated VINT")
    return decode_vint(first + rest)


def decode_element_id(data: bytes | bytearray | memoryview, offset: int = 0) -> tuple[int, int]:
    if offset < 0 or offset >= len(data):
        raise VINTError("truncated element ID")
    width = vint_length(data[offset])
    if offset + width > len(data):
        raise VINTError("truncated element ID")
    element_id = int.from_bytes(data[offset : offset + width], "big")
    return element_id, width


def read_element_id(stream: BinaryIO) -> tuple[int, int]:
    first = stream.read(1)
    if not first:
        raise VINTError("unexpected EOF while reading element ID")
    width = vint_length(first[0])
    rest = stream.read(width - 1)
    if len(rest) != width - 1:
        raise VINTError("truncated element ID")
    return int.from_bytes(first + rest, "big"), width


def encode_element_id(element_id: int) -> bytes:
    if element_id <= 0:
        raise VINTError("element ID must be positive")
    width = (element_id.bit_length() + 7) // 8
    if width > 4:
        raise VINTError("element ID exceeds 4 bytes")
    if vint_length(element_id >> (8 * (width - 1))) != width:
        raise VINTError("element ID has an invalid marker bit")
    return element_id.to_bytes(width, "big")


def encode_vint(value: int, width: int | None = None, *, unknown: bool = False) -> bytes:
    """Encode an EBML data size.

    Values requiring a marker width set to the unknown sentinel are rejected;
    callers must ask for a wider representation explicitly.
    """
    if value < 0:
        raise VINTError("VINT value cannot be negative")
    if width is None:
        for candidate in range(1, 9):
            if value < vint_capacity(candidate):
                width = candidate
                break
        else:
            raise VINTError("VINT value is too large")
    if not 1 <= width <= 8:
        raise VINTError("VINT width must be 1..8")
    marker = 1 << (7 * width)
    if unknown:
        encoded = marker | (marker - 1)
    else:
        if value >= vint_capacity(width):
            raise VINTError("value does not fit requested VINT width")
        encoded = marker | value
    return encoded.to_bytes(width, "big")


def encode_unknown_size(width: int = 1) -> bytes:
    return encode_vint(0, width, unknown=True)


def is_unknown_size(value: int, width: int) -> bool:
    return value == vint_capacity(width)


def decode_signed(data: bytes | bytearray | memoryview, offset: int = 0, width: int | None = None) -> int:
    """Decode a fixed-width two's-complement EBML signed integer."""
    if width is None:
        width = len(data) - offset
    if width == 0:
        return 0
    if width < 0 or offset + width > len(data) or offset < 0:
        raise VINTError("invalid signed integer width")
    return int.from_bytes(data[offset : offset + width], "big", signed=True)


def encode_signed(value: int, width: int | None = None) -> bytes:
    if width is None:
        width = max(1, (value.bit_length() + 8) // 8)
        while not -(1 << (width * 8 - 1)) <= value < (1 << (width * 8 - 1)):
            width += 1
    if not 1 <= width <= 8:
        raise VINTError("signed integer width must be 1..8")
    try:
        return value.to_bytes(width, "big", signed=True)
    except OverflowError as exc:
        raise VINTError("signed value does not fit requested width") from exc

