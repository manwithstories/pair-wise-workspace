"""EBML primitives: element IDs, VINT encoding, element types, parse state.

This private module holds the wire-level pieces shared by
:mod:`src.ebml_schema` (the public schema surface) and the reader. Nothing here
knows about Matroska semantics beyond a handful of element-ID constants.

Wire rules implemented here (RFC 8794):
  * Element IDs keep their length-marker bits and compare as raw bytes.
  * A VINT length occupies 1..8 octets; the marker bit of the first octet sets
    the width. An all-ones value payload means "unknown size".
  * First octet 0x00 is illegal (no marker bit); ID widths above 4 octets and
    size widths above 8 are rejected.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum


MAX_VINT_WIDTH = 8

#: Sentinel returned when an element length is "unknown" (all value bits 1).
UNKNOWN_SIZE = -1

#: Element types that can appear in the schema table.
ELEMENT_TYPES = frozenset(
    {"master", "uint", "int", "float", "string", "utf8", "binary", "date", "unknown"}
)


class SchemaError(ValueError):
    """Raised when EBML metadata is malformed (bad ID, reserved bits, ...)."""

    def __init__(self, code: str, message: str, offset: int | None = None) -> None:
        self.code = code
        self.offset = offset
        super().__init__(message)

    def as_dict(self) -> dict[str, object]:
        return {"code": self.code, "message": str(self), "offset": self.offset}


@dataclass(frozen=True)
class ElementSpec:
    """Static description of one EBML element."""

    name: str
    element_id: bytes
    type: str
    level: int
    parents: tuple[str, ...] = ()
    multiple: bool = False

    def __post_init__(self) -> None:
        if self.type not in ELEMENT_TYPES:
            raise SchemaError(
                "invalid_element_type", f"{self.name}: unknown element type {self.type!r}"
            )
        if not self.element_id:
            raise SchemaError("invalid_element_id", f"{self.name}: empty element id")
        if len(self.element_id) > 4:
            raise SchemaError(
                "invalid_element_id",
                f"{self.name}: element id longer than 4 octets",
            )

    @property
    def key(self) -> str:
        return self.element_id.hex()

    def accepts_parent(self, parent_name: str | None) -> bool:
        return not self.parents or (parent_name in self.parents)


class Strictness(Enum):
    """Parsing posture. STRICT rejects anything the spec forbids."""

    STRICT = "strict"
    LENIENT = "lenient"


class ParseState(Enum):
    """Lifecycle of a single element read by the reader."""

    START = "start"
    HEADER = "header"
    CHILDREN = "children"
    PAYLOAD = "payload"
    DONE = "done"
    ERROR = "error"


# ── VINT primitives ────────────────────────────────────────────────────────


def vint_width(first_byte: int) -> int:
    """Return 1..8 for a leading VINT octet, or raise on 0x00 / >8 octets."""
    if first_byte == 0:
        raise SchemaError(
            "vint_no_marker", "VINT first octet 0x00 has no length marker bit"
        )
    width = 9 - first_byte.bit_length()
    if width < 1 or width > MAX_VINT_WIDTH:
        raise SchemaError("vint_too_wide", f"VINT width {width} exceeds 8 octets")
    return width


def vint_mask(width: int) -> int:
    """Value-of-ones mask for a VINT of ``width`` octets (the unknown sentinel)."""
    if not 1 <= width <= MAX_VINT_WIDTH:
        raise SchemaError("vint_width", f"VINT width {width} out of range")
    return (1 << (7 * width)) - 1


def decode_vint(data: bytes, *, keep_marker: bool, offset: int | None = None) -> tuple[int, int]:
    """Decode an EBML VINT from ``data`` and return ``(value, width)``.

    ``keep_marker=True`` preserves the marker bits (element IDs). With
    ``keep_marker=False`` the value is the plain size with the marker stripped;
    an all-ones payload is still returned as the raw value of ones, and callers
    detect unknown size by comparing against :func:`vint_mask`.
    """
    if not data:
        raise SchemaError("vint_truncated", "empty VINT buffer", offset)
    first = data[0]
    width = vint_width(first)
    if len(data) < width:
        raise SchemaError(
            "vint_truncated",
            f"VINT needs {width} octets, only {len(data)} available",
            offset,
        )
    raw = int.from_bytes(data[:width], "big")
    if keep_marker:
        return raw, width
    value = raw & vint_mask(width)
    return value, width


def encode_vint(value: int, width: int | None = None, *, keep_marker: bool = False) -> bytes:
    """Encode ``value`` as an EBML VINT.

    When ``width`` is None the shortest legal width is chosen. ``keep_marker``
    is for element IDs, where ``value`` already includes its marker bits.
    """
    if keep_marker:
        if value <= 0:
            raise SchemaError("id_invalid", "element id must be positive")
        width = (value.bit_length() + 7) // 8
        return value.to_bytes(width, "big")
    if value < 0:
        raise SchemaError("vint_negative", "VINT value must be non-negative")
    width = width or _min_vint_width(value)
    if not 1 <= width <= MAX_VINT_WIDTH:
        raise SchemaError("vint_width", f"VINT width {width} out of range")
    limit = vint_mask(width) - 1
    if value > limit:
        raise SchemaError(
            "vint_overflow",
            f"value {value} does not fit in {width}-octet VINT without reading as unknown size",
        )
    return (value | (1 << (7 * width))).to_bytes(width, "big")


def _min_vint_width(value: int) -> int:
    # An all-ones payload is the reserved "unknown size" sentinel, so a known
    # size needs one width wider to stay unambiguous.
    for width in range(1, MAX_VINT_WIDTH + 1):
        if value < vint_mask(width):
            return width
    raise SchemaError("vint_overflow", f"value {value} needs more than 8 octets")


def encode_unknown_size(width: int = 8) -> bytes:
    """Encode an unknown-size VINT (marker bit followed by all value bits set)."""
    if not 1 <= width <= MAX_VINT_WIDTH:
        raise SchemaError("vint_width", f"VINT width {width} out of range")
    return ((1 << (7 * width)) | vint_mask(width)).to_bytes(width, "big")


def is_unknown_size(value: int, width: int) -> bool:
    """True when a decoded size equals the all-ones sentinel for its width."""
    return value == vint_mask(width)


# ── Primitive payload codecs ──────────────────────────────────────────────


def decode_uint(data: bytes) -> int:
    if not data:
        return 0
    if len(data) > 8:
        raise SchemaError("uint_too_wide", f"unsigned int payload {len(data)} octets")
    return int.from_bytes(data, "big")


def encode_uint(value: int, width: int | None = None) -> bytes:
    if value < 0:
        raise SchemaError("uint_negative", "unsigned int cannot be negative")
    width = width or max(1, (value.bit_length() + 7) // 8)
    return value.to_bytes(width, "big")


def decode_int(data: bytes) -> int:
    if not data:
        return 0
    if len(data) > 8:
        raise SchemaError("int_too_wide", f"signed int payload {len(data)} octets")
    return int.from_bytes(data, "big", signed=True)


def encode_int(value: int, width: int | None = None) -> bytes:
    if width is not None:
        return value.to_bytes(width, "big", signed=True)
    width = 1
    while not -(1 << (8 * width - 1)) <= value < (1 << (8 * width - 1)):
        width += 1
    return value.to_bytes(width, "big", signed=True)


def decode_float(data: bytes) -> float:
    import struct as _struct

    if len(data) == 4:
        return _struct.unpack(">f", data)[0]
    if len(data) == 8:
        return _struct.unpack(">d", data)[0]
    if len(data) == 0:
        return 0.0
    raise SchemaError("float_width", f"float payload must be 4 or 8 octets, got {len(data)}")


def encode_float(value: float, width: int = 8) -> bytes:
    import struct as _struct

    if width == 4:
        return _struct.pack(">f", value)
    if width == 8:
        return _struct.pack(">d", value)
    raise SchemaError("float_width", f"float width must be 4 or 8, got {width}")


def decode_string(data: bytes) -> str:
    return data.split(b"\x00", 1)[0].decode("ascii", errors="replace")


def decode_utf8(data: bytes) -> str:
    return data.split(b"\x00", 1)[0].decode("utf-8", errors="replace")


def decode_date(data: bytes) -> int:
    """EBML date: signed nanoseconds since 2001-01-01T00:00:00 UTC."""
    if len(data) not in (0, 8):
        raise SchemaError("date_width", f"date payload must be 8 octets, got {len(data)}")
    return decode_int(data) if data else 0


def decode_payload(element_type: str, data: bytes):
    """Dispatch primitive decoding by declared element type."""
    if element_type in ("uint",):
        return decode_uint(data)
    if element_type == "int":
        return decode_int(data)
    if element_type == "float":
        return decode_float(data)
    if element_type == "string":
        return decode_string(data)
    if element_type == "utf8":
        return decode_utf8(data)
    if element_type == "date":
        return decode_date(data)
    if element_type == "binary":
        return bytes(data)
    raise SchemaError(
        "not_primitive", f"element type {element_type!r} has no primitive payload"
    )


