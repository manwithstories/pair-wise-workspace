"""Byte cursor and ISO-BMFF box header parsing (standard library only)."""
from __future__ import annotations

import struct
from dataclasses import dataclass


class StructureError(Exception):
    """Raised when the byte layout violates ISO-BMFF structure."""

    def __init__(self, message: str, offset: int):
        super().__init__(f"{message} (file offset 0x{offset:x})")
        self.offset = offset


@dataclass
class BoxHeader:
    """Parsed box header: size/type plus largesize and uuid variants."""

    type: str
    offset: int        # absolute file offset of the box start
    size: int          # total box size including header
    header_size: int   # 8, 16 (largesize), or 24/32 (uuid)
    usertype: bytes | None = None

    @property
    def payload_offset(self) -> int:
        return self.offset + self.header_size

    @property
    def end(self) -> int:
        return self.offset + self.size


class ByteCursor:
    """Seekable cursor over a binary file object; reads on demand."""

    def __init__(self, fh, size: int):
        self._fh = fh
        self.size = size

    @classmethod
    def open(cls, path) -> "ByteCursor":
        fh = open(path, "rb")
        fh.seek(0, 2)
        return cls(fh, fh.tell())

    def read(self, offset: int, length: int) -> bytes:
        if offset < 0 or length < 0 or offset + length > self.size:
            raise StructureError("read past end of file", max(offset, 0))
        self._fh.seek(offset)
        data = self._fh.read(length)
        if len(data) != length:
            raise StructureError("short read", offset)
        return data

    def u8(self, offset: int) -> int:
        return self.read(offset, 1)[0]

    def u16(self, offset: int) -> int:
        return struct.unpack(">H", self.read(offset, 2))[0]

    def u24(self, offset: int) -> int:
        return int.from_bytes(self.read(offset, 3), "big")

    def u32(self, offset: int) -> int:
        return struct.unpack(">I", self.read(offset, 4))[0]

    def i32(self, offset: int) -> int:
        return struct.unpack(">i", self.read(offset, 4))[0]

    def u64(self, offset: int) -> int:
        return struct.unpack(">Q", self.read(offset, 8))[0]

    def i64(self, offset: int) -> int:
        return struct.unpack(">q", self.read(offset, 8))[0]

    def parse_header(self, offset: int, parent_end: int | None = None) -> BoxHeader:
        """Parse one box header; raises StructureError on truncation."""
        limit = self.size if parent_end is None else min(parent_end, self.size)
        if offset + 8 > limit:
            raise StructureError("truncated box header", offset)
        size, raw = struct.unpack(">I4s", self.read(offset, 8))
        try:
            box_type = raw.decode("ascii")
        except UnicodeDecodeError:
            raise StructureError(f"non-ascii box type {raw!r}", offset)
        header_size = 8
        usertype = None
        if size == 1:
            if offset + 16 > limit:
                raise StructureError("truncated largesize", offset)
            size = self.u64(offset + 8)
            header_size = 16
        elif size == 0:
            size = limit - offset  # box extends to end of parent/file
        if box_type == "uuid":
            if offset + header_size + 16 > limit:
                raise StructureError("truncated uuid usertype", offset)
            usertype = self.read(offset + header_size, 16)
            header_size += 16
        if size < header_size:
            raise StructureError(f"box size {size} smaller than header", offset)
        if offset + size > limit:
            raise StructureError(f"box '{box_type}' overruns parent/file", offset)
        return BoxHeader(box_type, offset, size, header_size, usertype)
