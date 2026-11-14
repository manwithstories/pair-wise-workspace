"""EBML variable-length integers and element headers.

Element IDs retain their marker bit (EBML IDs are compared verbatim), while
sizes discard it.  A size whose payload bits are all ones is the EBML unknown
length marker.
"""

from __future__ import annotations

from dataclasses import dataclass

from mkvtool.errors import InputParseError

MAX_ID_LENGTH = 4
MAX_SIZE_LENGTH = 8


@dataclass(frozen=True)
class ElementHeader:
    element_id: int
    size: int | None
    id_length: int
    size_length: int

    @property
    def unknown_size(self) -> bool:
        return self.size is None

    @property
    def header_length(self) -> int:
        return self.id_length + self.size_length


def vint_length(first_byte: int) -> int:
    """Return the encoded width implied by a VINT marker byte."""
    if first_byte == 0:
        raise InputParseError("invalid VINT: marker byte is zero")
    return 9 - first_byte.bit_length()


def read_vint(
    data: bytes | bytearray | memoryview,
    offset: int,
    *,
    keep_marker: bool = False,
    max_length: int = MAX_SIZE_LENGTH,
) -> tuple[int, int, bool]:
    """Read one VINT and return ``(value, width, is_unknown)``.

    ``is_unknown`` is only meaningful for sizes; IDs never use the all-ones
    marker as an encoded value in practice.
    """
    if offset < 0 or offset >= len(data):
        raise InputParseError(f"truncated VINT at offset {offset}")
    width = vint_length(data[offset])
    if width > max_length:
        raise InputParseError(
            f"VINT width {width} exceeds maximum {max_length} at offset {offset}"
        )
    if offset + width > len(data):
        raise InputParseError(f"truncated {width}-byte VINT at offset {offset}")

    if keep_marker:
        value = int.from_bytes(data[offset : offset + width], "big")
        return value, width, False

    marker = 1 << (8 - width)
    value = data[offset] & (marker - 1)
    for byte in data[offset + 1 : offset + width]:
        value = (value << 8) | byte
    unknown = value == (1 << (7 * width)) - 1
    return value, width, unknown


def read_element_id(data: bytes | bytearray | memoryview, offset: int) -> tuple[int, int]:
    value, width, _ = read_vint(data, offset, keep_marker=True, max_length=MAX_ID_LENGTH)
    return value, width


def parse_element_header(
    data: bytes | bytearray | memoryview, offset: int
) -> ElementHeader:
    """Parse an element ID and size VINT at ``offset``."""
    element_id, id_length = read_element_id(data, offset)
    size_offset = offset + id_length
    size, size_length, unknown = read_vint(data, size_offset, max_length=MAX_SIZE_LENGTH)
    return ElementHeader(
        element_id=element_id,
        size=None if unknown else size,
        id_length=id_length,
        size_length=size_length,
    )


def encode_vint(value: int, *, length: int | None = None) -> bytes:
    """Encode a non-negative integer as an EBML size VINT."""
    if value < 0:
        raise ValueError("VINT value must be non-negative")
    if length is None:
        for candidate in range(1, MAX_SIZE_LENGTH + 1):
            if value < (1 << (7 * candidate)) - 1:
                length = candidate
                break
        else:
            raise ValueError(f"value {value} does not fit in an 8-byte VINT")
    if not 1 <= length <= MAX_SIZE_LENGTH:
        raise ValueError("VINT length must be between 1 and 8")
    if value >= (1 << (7 * length)) - 1:
        raise OverflowError(f"value {value} does not fit in a {length}-byte VINT")
    marker = 1 << (7 * length)
    return (marker | value).to_bytes(length, "big")


def encode_element_id(element_id: int) -> bytes:
    """Encode an EBML ID, preserving its leading marker bit."""
    if not 1 <= element_id <= 0xFFFFFFFF:
        raise ValueError(f"invalid EBML element ID: {element_id:#x}")
    width = (element_id.bit_length() + 7) // 8
    if width > MAX_ID_LENGTH:
        raise ValueError("EBML element IDs are limited to four bytes")
    return element_id.to_bytes(width, "big")


def encode_unknown_size(length: int = 1) -> bytes:
    if not 1 <= length <= MAX_SIZE_LENGTH:
        raise ValueError("unknown-size VINT length must be between 1 and 8")
    marker = 1 << (8 - length)
    first = marker | (marker - 1)
    return bytes([first]) + b"\xff" * (length - 1)


def decode_signed(data: bytes) -> int:
    if not data:
        raise InputParseError("empty signed integer payload")
    return int.from_bytes(data, "big", signed=True)


def encode_signed(value: int, length: int = 2) -> bytes:
    return value.to_bytes(length, "big", signed=True)
