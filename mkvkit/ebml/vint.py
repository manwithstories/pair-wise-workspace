"""EBML variable-length integer (VINT) primitives.

A VINT stores its own length in the leading zero bits of the first byte:
the first set bit is the length marker.  Element IDs keep the marker bit
as part of the value, element sizes strip it.  A size whose value bits are
all ones is the reserved "unknown length" marker.
"""
from __future__ import annotations


class VintError(Exception):
    """Base class for VINT decoding failures."""


class VintTruncated(VintError):
    """Not enough bytes available to decode the VINT."""


class VintInvalid(VintError):
    """The VINT byte pattern is invalid (zero first byte)."""


def read_vint(buf, pos: int = 0, *, strip_marker: bool = True):
    """Decode one VINT from *buf* starting at *pos*.

    Returns ``(value, length, is_unknown)``.  With ``strip_marker`` the
    length-marker bit is removed from the value (element sizes); otherwise
    it is kept (element IDs).  ``is_unknown`` is only set for sizes whose
    value bits are all ones.
    """
    if pos >= len(buf):
        raise VintTruncated("no data available for VINT")
    first = buf[pos]
    if first == 0:
        raise VintInvalid("VINT first byte is zero")
    length = 9 - first.bit_length()
    end = pos + length
    if end > len(buf):
        raise VintTruncated(
            f"VINT needs {length} bytes, only {len(buf) - pos} available"
        )
    value = first & (0xFF >> length) if strip_marker else first
    for idx in range(pos + 1, end):
        value = (value << 8) | buf[idx]
    unknown = False
    if strip_marker:
        unknown = value == (1 << (7 * length)) - 1
    return value, length, unknown


def encode_size(value: int, length: int | None = None) -> bytes:
    """Encode *value* as an EBML element size with the marker bit set."""
    if value < 0:
        raise ValueError("negative size")
    if length is None:
        length = 1
        while value > (1 << (7 * length)) - 2:
            length += 1
            if length > 8:
                raise ValueError("size too large for VINT")
    if value > (1 << (7 * length)) - 2:
        raise ValueError("value does not fit requested VINT length")
    return ((1 << (7 * length)) | value).to_bytes(length, "big")


def encode_unknown(length: int = 8) -> bytes:
    """Encode the reserved all-ones "unknown size" VINT of *length* bytes."""
    if not 1 <= length <= 8:
        raise ValueError("VINT length out of range")
    return ((1 << (7 * length + 1)) - 1).to_bytes(length, "big")


def encode_id(id_value: int) -> bytes:
    """Encode an element ID (marker bit retained) on its canonical width."""
    for length in (1, 2, 3, 4):
        if id_value <= (1 << (8 * length)) - 1:
            return id_value.to_bytes(length, "big")
    raise ValueError("element ID too large")


def decode_signed(value: int, length: int) -> int:
    """Interpret a VINT value as a signed integer (EBML lacing deltas)."""
    return value - ((1 << (7 * length - 1)) - 1)


def encode_signed(value: int) -> bytes:
    """Encode a signed integer as the shortest signed VINT (lacing deltas)."""
    for length in range(1, 9):
        bias = (1 << (7 * length - 1)) - 1
        if -bias <= value <= bias:
            return encode_size(value + bias, length)
    raise ValueError("signed value does not fit a VINT")
