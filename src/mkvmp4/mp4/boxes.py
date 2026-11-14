"""Small ISO BMFF box writer with nested size backfill."""

from __future__ import annotations

import io
import struct
from typing import Iterable


class BoxError(ValueError):
    pass


def u8(value: int) -> bytes:
    return struct.pack(">B", value)


def u16(value: int) -> bytes:
    return struct.pack(">H", value)


def u24(value: int) -> bytes:
    return value.to_bytes(3, "big")


def u32(value: int) -> bytes:
    return struct.pack(">I", value)


def u64(value: int) -> bytes:
    return struct.pack(">Q", value)


def i16(value: int) -> bytes:
    return struct.pack(">h", value)


def i32(value: int) -> bytes:
    return struct.pack(">i", value)


def box(box_type: str, payload: bytes = b"") -> bytes:
    if len(box_type) != 4:
        raise BoxError("box type must be four ASCII characters")
    size = 8 + len(payload)
    if size > 0xFFFFFFFF:
        return u32(1) + box_type.encode("ascii") + u64(size + 8) + payload
    return u32(size) + box_type.encode("ascii") + payload


def full_box(box_type: str, version: int, flags: int, payload: bytes = b"") -> bytes:
    if not 0 <= version <= 255 or not 0 <= flags <= 0xFFFFFF:
        raise BoxError("invalid full-box version/flags")
    return box(box_type, u8(version) + u24(flags) + payload)


class BoxBuffer:
    """Build nested boxes and backfill each container size when it closes."""

    def __init__(self) -> None:
        self._buffer = io.BytesIO()
        self._stack: list[int] = []

    def tell(self) -> int:
        return self._buffer.tell()

    def getvalue(self) -> bytes:
        if self._stack:
            raise BoxError("cannot finish with open boxes")
        return self._buffer.getvalue()

    def write(self, data: bytes) -> None:
        self._buffer.write(data)

    def start(self, box_type: str, *, version: int | None = None,
              flags: int = 0) -> None:
        if len(box_type) != 4:
            raise BoxError("box type must be four ASCII characters")
        self._stack.append(self._buffer.tell())
        self._buffer.write(b"\x00\x00\x00\x00" + box_type.encode("ascii"))
        if version is not None:
            if not 0 <= version <= 255 or not 0 <= flags <= 0xFFFFFF:
                raise BoxError("invalid full-box version/flags")
            self._buffer.write(u8(version) + u24(flags))

    def end(self) -> None:
        if not self._stack:
            raise BoxError("no open box")
        start = self._stack.pop()
        end = self._buffer.tell()
        size = end - start
        if size > 0xFFFFFFFF:
            raise BoxError("metadata box exceeds 32-bit size")
        self._buffer.seek(start)
        self._buffer.write(u32(size))
        self._buffer.seek(end)

    def __enter__(self) -> "BoxBuffer":
        return self

    def __exit__(self, exc_type: object, *_: object) -> None:
        if exc_type is None:
            self.end()


def join_boxes(parts: Iterable[bytes]) -> bytes:
    return b"".join(parts)


def visual_sample_entry(width: int, height: int, codec_private: bytes,
                        *, compressor: bytes = b"") -> bytes:
    name = compressor[:31].ljust(32, b"\x00")
    payload = bytearray()
    payload += b"\x00" * 6 + u16(1)
    payload += u16(0) + u16(0) + b"\x00" * 12
    payload += u16(max(1, min(width, 0xFFFF))) + u16(max(1, min(height, 0xFFFF)))
    payload += u32(0x00480000) + u32(0x00480000) + u32(0) + u16(1)
    payload += name + u16(0x0018) + i16(-1)
    payload += box("avcC", codec_private)
    return box("avc1", bytes(payload))


def audio_sample_entry(sample_rate: int, channels: int, codec_private: bytes) -> bytes:
    payload = bytearray()
    payload += b"\x00" * 6 + u16(1)
    payload += b"\x00" * 8
    payload += u16(max(1, min(channels, 0xFFFF))) + u16(16) + u16(0) + u16(0)
    payload += u32(max(0, min(sample_rate, 0xFFFF)) << 16)
    payload += full_box("esds", 0, 0, codec_private)
    return box("mp4a", bytes(payload))


def text_sample_entry() -> bytes:
    payload = bytearray()
    payload += b"\x00" * 6 + u16(1)
    payload += u32(0) + u8(0) + u8(0)
    payload += b"\x00\x00\x00\x00"
    payload += u16(0) + u16(0) + u16(0) + u16(0)
    payload += u16(1) + u16(1) + u16(1) + u8(0) + u8(12) + b"\x00" * 4
    return box("tx3g", bytes(payload))


def sample_description(entry: bytes) -> bytes:
    payload = bytes(4) + u32(1) + entry
    return full_box("stsd", 0, 0, payload)


def matrix_identity() -> bytes:
    return b"".join(u32(value) for value in (0x00010000, 0, 0, 0, 0x00010000,
                                             0, 0, 0, 0x40000000))


def language_code(language: str) -> int:
    code = (language or "und").split("-")[0].lower()
    if len(code) != 3 or not code.isalpha():
        code = "und"
    value = 0
    for char in code:
        value = (value << 5) | (ord(char) - 0x60)
    return value


def write_mdat_header(stream: io.BufferedWriter, payload_size: int) -> int:
    total = payload_size + 8
    if total <= 0xFFFFFFFF:
        stream.write(u32(total) + b"mdat")
        return 8
    stream.write(u32(1) + b"mdat" + u64(payload_size + 16))
    return 16

