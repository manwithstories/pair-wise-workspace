"""EBML variable-length integer helpers.

A VINT encodes its own length in the leading bits of the first byte:
the first set bit marks the total length in bytes.  Element IDs keep
the marker bits, element sizes strip them.  A size whose data bits are
all 1 means "unknown length" (used by streaming writers for Segment
and Cluster).
"""

from ..errors import EbmlError

UNKNOWN_SIZE = None


def _vint_length(first_byte):
    mask = 0x80
    length = 1
    while not first_byte & mask:
        mask >>= 1
        length += 1
    return length, mask


def read_vint(fh, offset, max_len, strip_marker):
    """Read a VINT from a file object; returns ``(value, length)``."""
    first = fh.read(1)
    if len(first) != 1:
        raise EbmlError("unexpected end of file reading VINT", offset)
    if first[0] == 0:
        raise EbmlError("invalid VINT: zero first byte", offset)
    length, mask = _vint_length(first[0])
    if length > max_len:
        raise EbmlError(
            "VINT length %d exceeds %d" % (length, max_len), offset)
    value = first[0] & (mask - 1) if strip_marker else first[0]
    if length > 1:
        rest = fh.read(length - 1)
        if len(rest) != length - 1:
            raise EbmlError("truncated VINT", offset)
        value = (value << (8 * (length - 1))) | int.from_bytes(rest, "big")
    return value, length


def read_element_id(fh, offset):
    """Read an element ID (marker bits kept, 1-4 bytes)."""
    return read_vint(fh, offset, 4, False)


def read_element_size(fh, offset):
    """Read an element size; ``None`` means unknown length."""
    value, length = read_vint(fh, offset, 8, True)
    if value == (1 << (7 * length)) - 1:
        return UNKNOWN_SIZE, length
    return value, length


def vint_from_bytes(buf, pos, max_len=8, strip_marker=True):
    """Decode a VINT from a bytes buffer; returns ``(value, length)``."""
    if pos >= len(buf):
        raise EbmlError("truncated VINT in buffer")
    if buf[pos] == 0:
        raise EbmlError("invalid VINT: zero first byte")
    length, mask = _vint_length(buf[pos])
    if length > max_len:
        raise EbmlError("VINT length %d exceeds %d" % (length, max_len))
    if pos + length > len(buf):
        raise EbmlError("truncated VINT in buffer")
    value = buf[pos] & (mask - 1) if strip_marker else buf[pos]
    for k in range(1, length):
        value = (value << 8) | buf[pos + k]
    return value, length


def decode_signed(data):
    """Decode a big-endian two's-complement signed integer."""
    if not data:
        return 0
    value = int.from_bytes(data, "big")
    bits = len(data) * 8
    if value & (1 << (bits - 1)):
        value -= 1 << bits
    return value


def encode_id(value):
    """Encode an element ID (marker bits already part of the value)."""
    length = max(1, (value.bit_length() + 7) // 8)
    return value.to_bytes(length, "big")


def encode_size(value, length=None):
    """Encode an element size; ``value=None`` writes an unknown size."""
    if value is None:
        length = 1 if length is None else length
        return bytes([0xFF >> (length - 1)]) + b"\xff" * (length - 1)
    if length is None:
        length = 1
        while value >= (1 << (7 * length)) - 1:
            length += 1
    if value >= (1 << (7 * length)) - 1:
        raise ValueError("size %d does not fit in %d bytes" % (value, length))
    return (value | (1 << (7 * length))).to_bytes(length, "big")
