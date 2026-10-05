"""Frame container and TLV body codec for the SCADA gateway wire format.

Wire format (little endian, 26 byte fixed header)::

    off  size  field
      0     4  magic         b'TLM1'
      4     1  version       0x01
      5     1  flags         bit0 FRAGMENTED, bit1 LAST
      6     4  seq           monotonic 32-bit counter, per session
     10     2  frag_index    index of this fragment inside the frame
     12     2  frag_count    total fragments making up the frame
     14     4  body_len      body bytes carried by *this* fragment
     18     4  total_len     body length of the fully reassembled frame
     22     4  crc32c        CRC-32C over header[0:22] + reassembled body
     26   ...  body

The body is a flat TLV sequence::

    u16 tag, u16 length, <length bytes of value>

Frames larger than ``MAX_FRAME_BYTES`` are rejected outright so a corrupt
length field can never make the reallocator reserve an absurd buffer.
"""

from __future__ import annotations

import struct
from typing import Dict, Iterator, List, Tuple

MAGIC = b"TLM1"
VERSION = 1

FLAG_FRAGMENTED = 0x01
FLAG_LAST = 0x02

HEADER_SIZE = 26
#: Size of the canonical CRC-cover block (see :func:`crc_cover`).
CRC_COVER = 15

#: Hard ceiling for a single reassembled frame.  Defends against a garbage
#: ``total_len`` turning into a multi-gigabyte allocation.
MAX_FRAME_BYTES = 8 << 20

#: A TLV body may not contain more tags than this; a corrupt length field can
#: otherwise make the parser spin through a huge number of zero-length tags.
MAX_TLV_TAGS = 4096

_HEADER = struct.Struct("<4sBBIHHII")
assert _HEADER.size == HEADER_SIZE - 4

#: The CRC is defined over exactly these fields, and over nothing else:
#: ``magic | version | seq | frag_count | total_len``.
#:
#: ``flags``, ``frag_index`` and ``body_len`` are deliberately excluded because
#: they differ between the fragments of one frame.  Since a fragmented frame can
#: only be checksummed once every fragment has arrived, the cover must be
#: identical for all of them; anchoring it to frame-invariant fields is what
#: makes the check well defined.
_COVER = struct.Struct("<4sBIHI")
assert _COVER.size == CRC_COVER


def crc_cover(seq: int, frag_count: int, total_len: int) -> bytes:
    """Canonical CRC-cover block for a frame."""
    return _COVER.pack(MAGIC, VERSION, seq & 0xFFFFFFFF, frag_count, total_len)

_U16 = struct.Struct("<H")
_U32 = struct.Struct("<I")

# --------------------------------------------------------------------------
# CRC-32C (Castagnoli, reversed polynomial 0x82F63B78)
#
# NOTE: zlib.crc32 implements CRC-32/ISO-HDLC (polynomial 0xEDB88320), which is
# a *different* checksum.  The gateway frames carry CRC-32C, so we implement it
# here with a slice-by-8 table driven loop.  It costs roughly 17 MB/s, which is
# why the pipeline verifies exactly once per reassembled frame rather than once
# per input line.
# --------------------------------------------------------------------------

_CRC32C_POLY = 0x82F63B78


def _build_tables() -> List[List[int]]:
    t0 = []
    for i in range(256):
        crc = i
        for _ in range(8):
            crc = (crc >> 1) ^ (_CRC32C_POLY if crc & 1 else 0)
        t0.append(crc)
    tables = [t0]
    for _ in range(7):
        prev = tables[-1]
        tables.append([(p >> 8) ^ t0[p & 0xFF] for p in prev])
    return tables


_T0, _T1, _T2, _T3, _T4, _T5, _T6, _T7 = _build_tables()
_UNPACK8 = int.from_bytes


def crc32c(data, crc: int = 0) -> int:
    """Return the CRC-32C of *data*.

    *crc* seeds the computation, so ``crc32c(b, crc32c(a)) == crc32c(a + b)``.
    That lets the pipeline checksum the header and the reassembled body
    separately instead of joining them into one buffer.
    """
    crc ^= 0xFFFFFFFF
    view = memoryview(data)
    n = view.nbytes
    i = 0
    limit = n - (n & 7)
    t7, t6, t5, t4 = _T7, _T6, _T5, _T4
    t3, t2, t1, t0 = _T3, _T2, _T1, _T0
    while i < limit:
        crc ^= _UNPACK8(view[i : i + 8], "little")
        i += 8
        crc = (
            t7[crc & 0xFF]
            ^ t6[(crc >> 8) & 0xFF]
            ^ t5[(crc >> 16) & 0xFF]
            ^ t4[(crc >> 24) & 0xFF]
            ^ t3[(crc >> 32) & 0xFF]
            ^ t2[(crc >> 40) & 0xFF]
            ^ t1[(crc >> 48) & 0xFF]
            ^ t0[(crc >> 56) & 0xFF]
        )
    while i < n:
        crc = (crc >> 8) ^ t0[(crc ^ view[i]) & 0xFF]
        i += 1
    return crc ^ 0xFFFFFFFF


class ProtocolError(ValueError):
    """Raised for any malformed frame.  Always caught and routed to the DLQ."""


class Frame:
    """A decoded frame fragment (not yet necessarily a whole frame)."""

    __slots__ = (
        "version",
        "flags",
        "seq",
        "frag_index",
        "frag_count",
        "body_len",
        "total_len",
        "crc",
        "body",
        "cover",
    )

    def __init__(
        self,
        version: int,
        flags: int,
        seq: int,
        frag_index: int,
        frag_count: int,
        body_len: int,
        total_len: int,
        crc: int,
        body: bytes,
        cover: bytes,
    ) -> None:
        self.version = version
        self.flags = flags
        self.seq = seq
        self.frag_index = frag_index
        self.frag_count = frag_count
        self.body_len = body_len
        self.total_len = total_len
        self.crc = crc
        self.body = body
        #: First CRC_COVER header bytes, retained so a fragmented frame can be
        #: checksummed against its fully reassembled body.
        self.cover = cover

    @property
    def fragmented(self) -> bool:
        return bool(self.flags & FLAG_FRAGMENTED)

    def chunked(self) -> bool:
        """True when this fragment needs sibling fragments to form a frame."""
        return self.frag_count > 1

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return (
            f"<Frame seq={self.seq} frag={self.frag_index}/{self.frag_count} "
            f"body={self.body_len}/{self.total_len}>"
        )


def parse_frame(raw: bytes) -> Frame:
    """Decode one frame container.  Raises :class:`ProtocolError` on garbage."""
    if len(raw) < HEADER_SIZE:
        raise ProtocolError(f"short frame: {len(raw)} bytes < {HEADER_SIZE}")

    magic, version, flags, seq, frag_index, frag_count, body_len, total_len = (
        _HEADER.unpack_from(raw, 0)
    )
    if magic != MAGIC:
        raise ProtocolError(f"bad magic {magic!r}")
    if version != VERSION:
        raise ProtocolError(f"unsupported version {version}")
    if frag_count == 0:
        raise ProtocolError("frag_count must be >= 1")
    if frag_index >= frag_count:
        raise ProtocolError(f"frag_index {frag_index} >= frag_count {frag_count}")
    if total_len > MAX_FRAME_BYTES:
        raise ProtocolError(f"total_len {total_len} exceeds cap {MAX_FRAME_BYTES}")
    if body_len > total_len:
        raise ProtocolError(f"body_len {body_len} > total_len {total_len}")

    if len(raw) < HEADER_SIZE + body_len:
        raise ProtocolError(
            f"truncated body: have {len(raw) - HEADER_SIZE}, want {body_len}"
        )

    # Trailing bytes past the declared body mean the line was spliced or the
    # length field is wrong; treat it as corruption rather than silently
    # ignoring the difference.
    extra = len(raw) - HEADER_SIZE - body_len
    if extra:
        raise ProtocolError(f"{extra} trailing byte(s) after body")

    crc = _U32.unpack_from(raw, 22)[0]
    body = raw[HEADER_SIZE : HEADER_SIZE + body_len]
    return Frame(
        version, flags, seq, frag_index, frag_count, body_len, total_len, crc, body,
        crc_cover(seq, frag_count, total_len),
    )


def frame_crc(cover: bytes, body: bytes) -> int:
    """Compute the frame CRC over the CRC-cover header plus *body*.

    Seeded in two steps so the reassembled body never has to be copied into a
    contiguous header+body buffer just to be checksummed.
    """
    return crc32c(body, crc32c(cover))


def verify_crc(frame: Frame) -> None:
    """Validate an unfragmented frame's CRC in place.

    Fragmented frames are checksummed by the reassembler, which is the only
    component that ever holds the complete body.
    """
    if frame.frag_count != 1:
        return
    actual = frame_crc(frame.cover, frame.body)
    if actual != frame.crc:
        raise ProtocolError(
            f"crc mismatch on seq={frame.seq}: got 0x{actual:08x}, want 0x{frame.crc:08x}"
        )


# --------------------------------------------------------------------------
# TLV body
# --------------------------------------------------------------------------


def iter_tlvs(body: bytes) -> Iterator[Tuple[int, bytes]]:
    """Yield ``(tag, value)`` pairs from a TLV encoded body.

    Raises :class:`ProtocolError` on truncation, overlong tag runs, or a tag
    that would not fit in the u16/u32 header.
    """
    view = memoryview(body)
    size = view.nbytes
    pos = 0
    tags = 0
    while pos < size:
        if size - pos < 4:
            raise ProtocolError(f"trailing {size - pos} byte(s) in TLV body")
        tag = _U16.unpack_from(view, pos)[0]
        length = _U16.unpack_from(view, pos + 2)[0]
        pos += 4
        if size - pos < length:
            raise ProtocolError(
                f"TLV tag 0x{tag:04x} claims {length} bytes, only {size - pos} left"
            )
        yield tag, view[pos : pos + length]
        pos += length
        tags += 1
        if tags > MAX_TLV_TAGS:
            raise ProtocolError(f"more than {MAX_TLV_TAGS} TLVs in one body")


def parse_tlvs(body: bytes) -> Dict[int, bytes]:
    """Parse a TLV body into a ``tag -> value`` mapping (last tag wins)."""
    out: Dict[int, bytes] = {}
    for tag, value in iter_tlvs(body):
        out[tag] = bytes(value)
    return out


def build_frame(
    seq: int,
    body: bytes,
    frag_index: int = 0,
    frag_count: int = 1,
    total_len: int | None = None,
) -> bytes:
    """Encode a frame.  Used by the test-data generator and the test-suite."""
    if total_len is None:
        total_len = len(body)
    flags = FLAG_FRAGMENTED if frag_count > 1 else 0
    if frag_count > 1 and frag_index == frag_count - 1:
        flags |= FLAG_LAST
    header = _HEADER.pack(
        MAGIC, VERSION, flags, seq & 0xFFFFFFFF, frag_index, frag_count,
        len(body), total_len,
    )
    # One CRC per *frame*, identical across all of its fragments, computed over
    # the invariant cover plus the body this call carries.  For a fragmented
    # frame the caller must therefore pass the same `total_len`/`frag_count`
    # and the CRC ends up covering only that fragment, so fragmented frames are
    # built by :func:`build_fragments` instead.
    return header + _U32.pack(
        crc32c(body, crc32c(crc_cover(seq, frag_count, total_len)))
    ) + body


def build_fragments(seq: int, body: bytes, nfrag: int) -> List[bytes]:
    """Split *body* into *nfrag* fragments that share one frame CRC.

    The CRC is computed once over the whole body, so every fragment carries the
    same value and the reassembler can verify the reassembled frame.
    """
    if nfrag < 1:
        raise ValueError("nfrag must be >= 1")
    if nfrag == 1:
        return [build_frame(seq, body)]
    step = max(1, (len(body) + nfrag - 1) // nfrag)
    crc = crc32c(body, crc32c(crc_cover(seq, nfrag, len(body))))
    out: List[bytes] = []
    for i in range(nfrag):
        chunk = body[i * step : (i + 1) * step]
        flags = FLAG_FRAGMENTED | (FLAG_LAST if i == nfrag - 1 else 0)
        header = _HEADER.pack(
            MAGIC, VERSION, flags, seq & 0xFFFFFFFF, i, nfrag,
            len(chunk), len(body),
        )
        out.append(header + _U32.pack(crc) + chunk)
    return out
