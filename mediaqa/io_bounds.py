"""Bounded binary reader: endian helpers, EBML varints, truncation guards."""

import os
import struct


class MediaError(Exception):
    """Container parse error carrying a byte offset (never a bare traceback)."""

    def __init__(self, message, offset=None, path=None):
        self.message = message
        self.offset = offset
        self.path = path
        where = []
        if path is not None:
            where.append(str(path))
        if offset is not None:
            where.append("offset 0x%x" % offset)
        super().__init__("%s: %s" % (":".join(where), message) if where else message)


class BoundedReader:
    """Random-access reader confined to [base, base + size) of a file."""

    __slots__ = ("fh", "base", "size", "pos", "path")

    def __init__(self, fh, size, base=0, path=None):
        self.fh = fh
        self.base = base
        self.size = size
        self.pos = 0
        self.path = path

    @classmethod
    def open(cls, path):
        fh = open(path, "rb")
        return cls(fh, os.fstat(fh.fileno()).st_size, 0, path)

    def close(self):
        self.fh.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False

    def tell(self):
        return self.pos

    def remaining(self):
        return self.size - self.pos

    def eof(self):
        return self.pos >= self.size

    def _need(self, count):
        if count < 0 or count > self.remaining():
            raise MediaError(
                "need %d bytes, only %d left (truncated or out-of-bounds)"
                % (count, self.remaining()),
                self.base + self.pos,
                self.path,
            )

    def read(self, count):
        self._need(count)
        self.fh.seek(self.base + self.pos)
        data = self.fh.read(count)
        if len(data) != count:
            raise MediaError("short read", self.base + self.pos, self.path)
        self.pos += count
        return data

    def skip(self, count):
        self._need(count)
        self.pos += count

    def seek(self, pos):
        if pos < 0 or pos > self.size:
            raise MediaError("seek out of bounds", self.base + pos, self.path)
        self.pos = pos

    def sub(self, count):
        """Return a child reader over the next `count` bytes and consume them."""
        self._need(count)
        child = BoundedReader(self.fh, count, self.base + self.pos, self.path)
        self.pos += count
        return child

    def _unpack(self, fmt):
        return struct.unpack(fmt, self.read(struct.calcsize(fmt)))[0]

    def u8(self):
        return self._unpack(">B")

    def u16be(self):
        return self._unpack(">H")

    def u16le(self):
        return self._unpack("<H")

    def u32be(self):
        return self._unpack(">I")

    def u32le(self):
        return self._unpack("<I")

    def u64be(self):
        return self._unpack(">Q")

    def u64le(self):
        return self._unpack("<Q")

    def ebml_id(self):
        """Read an EBML element id (length-descriptor bit kept)."""
        first = self.u8()
        length = 1
        while length <= 4 and not first & (0x80 >> (length - 1)):
            length += 1
        if length > 4:
            raise MediaError("invalid EBML id", self.base + self.pos - 1, self.path)
        value = first
        for _ in range(length - 1):
            value = (value << 8) | self.u8()
        return value

    def ebml_size(self):
        """Read an EBML element size; None means unknown (all-ones) length."""
        first = self.u8()
        length = 1
        while length <= 8 and not first & (0x80 >> (length - 1)):
            length += 1
        if length > 8:
            raise MediaError("invalid EBML size", self.base + self.pos - 1, self.path)
        value = first & (0xFF >> length)
        for _ in range(length - 1):
            value = (value << 8) | self.u8()
        if value == (1 << (7 * length)) - 1:
            return None
        return value
