"""Byte-level ISO BMFF box reading with offset tracking.

Streaming cursor design: only box headers and metadata payloads are
touched; mdat sample data is never materialised in memory.
"""
from __future__ import annotations

import mmap
import struct
from dataclasses import dataclass

__all__ = ["Box", "BoxError", "TruncatedError", "BoxReader", "parse_header"]


class BoxError(Exception):
    """Structural problem in an ISO BMFF file."""


class TruncatedError(BoxError):
    """A box extends beyond the available bytes."""

    def __init__(self, offset, box_type, needed, available):
        self.offset = offset
        self.box_type = box_type
        self.needed = needed
        self.available = max(available, 0)
        super().__init__(
            "truncated box %r at byte %d: needs %d bytes, only %d available"
            % (box_type, offset, needed, self.available)
        )


@dataclass
class Box:
    type: str
    start: int
    size: int
    header_size: int
    usertype: bytes | None = None

    @property
    def end(self):
        return self.start + self.size

    @property
    def data_offset(self):
        return self.start + self.header_size

    @property
    def data_size(self):
        return self.size - self.header_size

    def __repr__(self):
        return "Box(%r, start=%d, size=%d)" % (self.type, self.start, self.size)


def parse_header(buf, offset, limit):
    """Parse one box header at ``offset``; ``limit`` is the parent end."""
    available = limit - offset
    if available < 8:
        raise TruncatedError(offset, "<header>", 8, available)
    size32, raw = struct.unpack_from(">I4s", buf, offset)
    box_type = raw.decode("latin-1")
    header = 8
    if size32 == 1:  # 64-bit largesize
        if available < 16:
            raise TruncatedError(offset, box_type, 16, available)
        size = struct.unpack_from(">Q", buf, offset + 8)[0]
        header = 16
    elif size32 == 0:  # box extends to end of parent/file
        size = available
    else:
        size = size32
    usertype = None
    if box_type == "uuid":
        if available < header + 16:
            raise TruncatedError(offset, box_type, header + 16, available)
        usertype = bytes(buf[offset + header: offset + header + 16])
        header += 16
    if size < header:
        raise BoxError(
            "invalid size %d for box %r at byte %d" % (size, box_type, offset))
    if size > available:
        raise TruncatedError(offset, box_type, size, available)
    return Box(box_type, offset, size, header, usertype)


CONTAINER_TYPES = frozenset({
    "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf",
    "moof", "traf", "mvex", "udta", "mfra", "skip",
})


class BoxReader:
    """Read-only mmap cursor over one ISO BMFF file."""

    def __init__(self, path):
        self.path = path
        self._fh = open(path, "rb")
        try:
            self._mm = mmap.mmap(self._fh.fileno(), 0, access=mmap.ACCESS_READ)
        except ValueError:
            self._fh.close()
            raise BoxError("empty file: %s" % path)
        self.size = self._mm.size()

    def close(self):
        self._mm.close()
        self._fh.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False

    def boxes(self, start=0, end=None):
        """Yield sibling boxes in [start, end); unknown types are skipped
        by offset without inspecting their payload."""
        end = self.size if end is None else min(end, self.size)
        pos = start
        while pos < end:
            box = parse_header(self._mm, pos, end)
            yield box
            pos = box.end

    def children(self, box):
        start = box.data_offset + (4 if box.type == "meta" else 0)
        return self.boxes(start, box.end)

    def box_at(self, offset):
        return parse_header(self._mm, offset, self.size)

    def read(self, offset, length):
        return self._mm[offset:offset + length]

    def unpack(self, fmt, offset):
        return struct.unpack_from(fmt, self._mm, offset)

    def fullbox(self, box):
        """Return (version, flags, body_offset) for a FullBox."""
        if box.data_size < 4:
            raise BoxError(
                "box %r at %d too small for fullbox header" % (box.type, box.start))
        raw = self._mm[box.data_offset:box.data_offset + 4]
        return raw[0], int.from_bytes(raw[1:4], "big"), box.data_offset + 4

    def copy_range(self, out, start, length, chunk_size=1 << 20):
        """Stream [start, start+length) to a writable file object."""
        pos = start
        remaining = length
        while remaining > 0:
            n = min(chunk_size, remaining)
            out.write(self._mm[pos:pos + n])
            pos += n
            remaining -= n

