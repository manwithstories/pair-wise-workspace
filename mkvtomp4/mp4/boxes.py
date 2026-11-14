"""Minimal ISO BMFF box encoding and streaming header parsing."""

from __future__ import annotations

import io
import os
import struct
from dataclasses import dataclass
from typing import BinaryIO


class BoxError(ValueError):
    """Raised for malformed or unsupported ISO BMFF boxes."""


def u8(value: int) -> bytes:
    return struct.pack(">B", value & 0xFF)


def u16(value: int) -> bytes:
    return struct.pack(">H", value & 0xFFFF)


def u32(value: int) -> bytes:
    return struct.pack(">I", value & 0xFFFFFFFF)


def i32(value: int) -> bytes:
    return struct.pack(">i", value)


def u64(value: int) -> bytes:
    return struct.pack(">Q", value & 0xFFFFFFFFFFFFFFFF)


def box(box_type: str, payload: bytes) -> bytes:
    if len(box_type) != 4:
        raise BoxError(f"box type must be four characters: {box_type!r}")
    size = 8 + len(payload)
    if size <= 0xFFFFFFFF:
        return u32(size) + box_type.encode("ascii") + payload
    return u32(1) + box_type.encode("ascii") + u64(size) + payload


def full_box(box_type: str, version: int, flags: int, payload: bytes = b"") -> bytes:
    return box(box_type, bytes([version]) + flags.to_bytes(3, "big") + payload)


def container(box_type: str, *children: bytes) -> bytes:
    return box(box_type, b"".join(children))


@dataclass(frozen=True)
class BoxHeader:
    box_type: str
    offset: int
    size: int
    header_size: int
    data_offset: int

    @property
    def end(self) -> int:
        return self.offset + self.size


def read_box_header(stream: BinaryIO, offset: int, limit: int | None = None) -> BoxHeader:
    if limit is None:
        current = stream.tell()
        stream.seek(0, os.SEEK_END)
        limit = stream.tell()
        stream.seek(current)
    if offset + 8 > limit:
        raise BoxError(f"truncated box header at {offset}")
    stream.seek(offset)
    raw = stream.read(8)
    size, kind = struct.unpack(">I4s", raw)
    header_size = 8
    if size == 1:
        if offset + 16 > limit:
            raise BoxError(f"truncated 64-bit box header at {offset}")
        size = struct.unpack(">Q", stream.read(8))[0]
        header_size = 16
    elif size == 0:
        size = limit - offset
    if size < header_size or offset + size > limit:
        raise BoxError(f"invalid box size {size} for {kind!r} at {offset}")
    return BoxHeader(kind.decode("ascii", "replace"), offset, size, header_size, offset + header_size)


def iter_box_headers(stream: BinaryIO, start: int = 0, end: int | None = None):
    if end is None:
        current = stream.tell()
        stream.seek(0, os.SEEK_END)
        end = stream.tell()
        stream.seek(current)
    offset = start
    while offset < end:
        if end - offset < 8:
            break
        header = read_box_header(stream, offset, end)
        yield header
        offset = header.end


def read_payload(stream: BinaryIO, header: BoxHeader) -> bytes:
    stream.seek(header.data_offset)
    data = stream.read(header.size - header.header_size)
    if len(data) != header.size - header.header_size:
        raise BoxError(f"truncated payload for {header.box_type}")
    return data


def child_headers(payload: bytes) -> list[BoxHeader]:
    stream = io.BytesIO(payload)
    return list(iter_box_headers(stream, 0, len(payload)))


def find_header(headers: list[BoxHeader], box_type: str) -> BoxHeader | None:
    return next((header for header in headers if header.box_type == box_type), None)


def descriptor(tag: int, payload: bytes) -> bytes:
    size = len(payload) + 1
    encoded = bytearray()
    while True:
        encoded.insert(0, size & 0x7F)
        size >>= 7
        if not size:
            break
    encoded[-1] |= 0x80
    return bytes([tag]) + bytes(encoded) + payload
