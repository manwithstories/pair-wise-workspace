"""Buffered sequential byte source with absolute-offset tracking."""

from __future__ import annotations

from typing import BinaryIO

from ._ebml_types import ReaderError


MAX_ID_WIDTH = 4

class ByteReader:
    """Buffered sequential reader over a file-like object with absolute offsets."""

    def __init__(self, fp: BinaryIO, *, size: int | None = None) -> None:
        self._fp = fp
        self._pos = 0
        self._size = size
        if size is None:
            cur = fp.tell()
            fp.seek(0, 2)
            self._size = fp.tell()
            fp.seek(cur)

    @property
    def position(self) -> int:
        return self._pos

    @property
    def size(self) -> int:
        return int(self._size or 0)

    def remaining(self) -> int:
        return self.size - self._pos

    def read(self, count: int) -> bytes:
        if count < 0:
            raise ReaderError("read_negative", f"negative read of {count} bytes", self._pos)
        if count == 0:
            return b""
        if self._pos + count > self.size:
            raise ReaderError(
                "ebml_truncated",
                f"need {count} bytes at offset {self._pos}, only {self.remaining()} remain",
                self._pos,
            )
        data = self._fp.read(count)
        if len(data) != count:
            raise ReaderError(
                "ebml_truncated",
                f"short read at offset {self._pos}: wanted {count}, got {len(data)}",
                self._pos,
            )
        self._pos += count
        return data

    def skip(self, count: int) -> None:
        if count < 0:
            raise ReaderError("skip_negative", f"negative skip of {count}", self._pos)
        if self._pos + count > self.size:
            raise ReaderError(
                "ebml_truncated",
                f"skip of {count} at offset {self._pos} runs past EOF ({self.size})",
                self._pos,
            )
        self._fp.seek(count, 1)
        self._pos += count

    def seek(self, position: int) -> None:
        if position < 0 or position > self.size:
            raise ReaderError("seek_out_of_range", f"seek to {position} outside file", position)
        self._fp.seek(position)
        self._pos = position

    def peek(self, count: int) -> bytes:
        start = self._pos
        data = self._fp.read(min(count, self.remaining()))
        self._fp.seek(start)
        return data

    def read_exact_into(self, sink: BinaryIO, count: int) -> int:
        """Stream ``count`` bytes into ``sink`` without buffering them here."""
        left = count
        while left:
            chunk = self.read(min(left, 1 << 20))
            sink.write(chunk)
            left -= len(chunk)
        return count


