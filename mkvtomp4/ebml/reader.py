"""Streaming EBML VINT, element-header and byte-range primitives."""

from __future__ import annotations

import io
import os
import zlib
from dataclasses import dataclass
from typing import BinaryIO


class EBMLReadError(ValueError):
    """Raised when the byte stream cannot be interpreted as EBML."""


class TruncatedFileError(EBMLReadError):
    """Raised when an element header or payload runs past end-of-file."""


class DepthLimitError(EBMLReadError):
    """Raised when recursive master-element nesting exceeds the limit."""


@dataclass(frozen=True)
class ElementHeader:
    eid: int
    size: int | None
    unknown_size: bool
    offset: int
    header_size: int
    data_offset: int

    @property
    def end(self) -> int | None:
        return None if self.size is None else self.data_offset + self.size


def decode_vint(data: bytes | bytearray | memoryview, offset: int, *, keep_marker: bool):
    """Decode one EBML VINT from *data* and return ``(value, length)``."""
    if offset >= len(data):
        raise TruncatedFileError("truncated VINT")
    first = data[offset]
    if first == 0:
        raise EBMLReadError("VINT has a zero first byte")
    marker = 0x80
    length = 1
    while length <= 8 and not (first & marker):
        marker >>= 1
        length += 1
    if length > 8 or offset + length > len(data):
        raise TruncatedFileError("truncated VINT")
    value = first if keep_marker else first & (marker - 1)
    for index in range(1, length):
        value = (value << 8) | data[offset + index]
    return value, length


class EBMLReader:
    """Cursor over a seekable byte stream with EBML-aware reads."""

    def __init__(self, stream: BinaryIO, *, path: str | None = None):
        self.stream = stream
        self.path = path
        self.size = self._stream_size()
        self.warnings: list[dict[str, object]] = []

    @classmethod
    def from_path(cls, path: os.PathLike[str] | str) -> "EBMLReader":
        return cls(open(path, "rb"), path=os.fspath(path))

    @classmethod
    def from_bytes(cls, data: bytes, *, path: str | None = None) -> "EBMLReader":
        return cls(io.BytesIO(data), path=path)

    def _stream_size(self) -> int:
        current = self.stream.tell()
        self.stream.seek(0, os.SEEK_END)
        size = self.stream.tell()
        self.stream.seek(current)
        return size

    def tell(self) -> int:
        return self.stream.tell()

    def seek(self, position: int) -> None:
        if position < 0 or position > self.size:
            raise TruncatedFileError(f"seek outside file: {position}")
        self.stream.seek(position)

    def read(self, count: int) -> bytes:
        if count < 0:
            raise ValueError("negative read length")
        data = self.stream.read(count)
        if len(data) != count:
            raise TruncatedFileError(
                f"truncated payload at {self.tell() - len(data)}: "
                f"wanted {count}, got {len(data)}"
            )
        return data

    def read_range(self, offset: int, count: int) -> bytes:
        current = self.tell()
        self.seek(offset)
        try:
            return self.read(count)
        finally:
            self.seek(current)

    def read_vint_id(self) -> int:
        """Read one 1-8 byte element ID, preserving its marker bit."""
        offset = self.tell()
        first = self.read(1)[0]
        if first == 0:
            raise EBMLReadError(f"invalid element ID at offset {offset}")
        marker = 0x80
        length = 1
        while length <= 8 and not (first & marker):
            marker >>= 1
            length += 1
        if length > 8:
            raise EBMLReadError(f"element ID is wider than 8 bytes at {offset}")
        encoded = bytes([first]) + self.read(length - 1)
        return int.from_bytes(encoded, "big")

    def read_vint_size(self) -> tuple[int, int, bool]:
        """Read a size VINT and return ``(value, byte_length, unknown)``."""
        offset = self.tell()
        first = self.read(1)[0]
        if first == 0:
            raise EBMLReadError(f"invalid element size at offset {offset}")
        marker = 0x80
        length = 1
        while length <= 8 and not (first & marker):
            marker >>= 1
            length += 1
        if length > 8:
            raise EBMLReadError(f"element size is wider than 8 bytes at {offset}")
        value = first & (marker - 1)
        for _ in range(length - 1):
            value = (value << 8) | self.read(1)[0]
        unknown = value == (1 << (7 * length)) - 1
        return value, length, unknown

    def read_header(self) -> ElementHeader:
        offset = self.tell()
        first = self.read(1)[0]
        marker = 0x80
        id_length = 1
        while id_length <= 8 and not (first & marker):
            marker >>= 1
            id_length += 1
        if id_length > 8 or first == 0:
            raise EBMLReadError(f"invalid element ID at offset {offset}")
        id_bytes = bytes([first]) + self.read(id_length - 1)
        eid = int.from_bytes(id_bytes, "big")

        first_size = self.read(1)[0]
        if first_size == 0:
            raise EBMLReadError(f"invalid element size at offset {self.tell() - 1}")
        size_marker = 0x80
        size_length = 1
        while size_length <= 8 and not (first_size & size_marker):
            size_marker >>= 1
            size_length += 1
        if size_length > 8:
            raise EBMLReadError(f"element size is wider than 8 bytes at {offset}")
        size_value = first_size & (size_marker - 1)
        for _ in range(size_length - 1):
            size_value = (size_value << 8) | self.read(1)[0]
        unknown = size_value == (1 << (7 * size_length)) - 1
        header_size = id_length + size_length
        data_offset = offset + header_size
        size = None if unknown else size_value
        if size is not None and data_offset + size > self.size:
            raise TruncatedFileError(
                f"element 0x{eid:X} at {offset} extends beyond end-of-file"
            )
        return ElementHeader(eid, size, unknown, offset, header_size, data_offset)

    def peek_id(self) -> int | None:
        position = self.tell()
        if position >= self.size:
            return None
        try:
            return self.read_header().eid
        finally:
            self.seek(position)

    def verify_crc(
        self,
        *,
        crc_element_offset: int,
        crc_data_offset: int,
        master_end: int,
        parent_name: str,
    ) -> bool:
        """Validate a CRC-32 child against bytes following the CRC element."""
        if crc_data_offset + 4 > master_end:
            self.warnings.append(
                {
                    "kind": "crc",
                    "parent": parent_name,
                    "offset": crc_element_offset,
                    "message": "truncated CRC-32 element",
                }
            )
            return False
        stored = int.from_bytes(self.read_range(crc_data_offset, 4), "big")
        payload = self.read_range(crc_data_offset + 4, master_end - crc_data_offset - 4)
        actual = zlib.crc32(payload) & 0xFFFFFFFF
        if stored != actual:
            self.warnings.append(
                {
                    "kind": "crc",
                    "parent": parent_name,
                    "offset": crc_element_offset,
                    "expected": f"0x{stored:08X}",
                    "actual": f"0x{actual:08X}",
                    "message": "CRC-32 mismatch; continuing",
                }
            )
            return False
        return True
