"""Minimal streaming RIFF/WAVE reader for the formats used by the project."""

from __future__ import annotations

import os
import struct
from dataclasses import dataclass
from typing import Iterator

import numpy as np


class RiffError(ValueError):
    """Raised when a WAV container is malformed or unsupported."""


@dataclass(frozen=True)
class WavInfo:
    path: str
    sample_rate: int
    channels: int
    bits_per_sample: int
    encoding: str
    block_align: int
    data_offset: int
    data_size: int
    total_frames: int


_ENCODINGS = {
    (1, 16): "pcm16",
    (1, 24): "pcm24",
    (1, 32): "pcm32",
    (3, 32): "float32",
}


def _u16(data: bytes, offset: int) -> int:
    if offset + 2 > len(data):
        raise RiffError("truncated format chunk")
    return struct.unpack_from("<H", data, offset)[0]


def _u32(data: bytes, offset: int) -> int:
    if offset + 4 > len(data):
        raise RiffError("truncated format chunk")
    return struct.unpack_from("<I", data, offset)[0]


def _parse_format(payload: bytes) -> tuple[int, int, int, int, int, str]:
    if len(payload) < 16:
        raise RiffError("short fmt chunk")
    format_tag = _u16(payload, 0)
    channels = _u16(payload, 2)
    sample_rate = _u32(payload, 4)
    block_align = _u16(payload, 12)
    bits = _u16(payload, 14)
    if format_tag == 0xFFFE:
        if len(payload) < 40 or _u16(payload, 16) < 22:
            raise RiffError("invalid extensible format")
        format_tag = _u16(payload, 24)
    if channels not in (1, 2):
        raise RiffError("unsupported channel count")
    if not 22050 <= sample_rate <= 96000:
        raise RiffError("unsupported sample rate")
    try:
        encoding = _ENCODINGS[(format_tag, bits)]
    except KeyError as exc:
        raise RiffError("unsupported sample format") from exc
    bytes_per_sample = bits // 8
    if block_align != channels * bytes_per_sample:
        raise RiffError("invalid block alignment")
    return sample_rate, channels, bits, block_align, format_tag, encoding


def _parse_header(fh, path: str) -> WavInfo:
    file_size = os.fstat(fh.fileno()).st_size
    header = fh.read(12)
    if len(header) != 12 or header[:4] != b"RIFF" or header[8:12] != b"WAVE":
        raise RiffError("not a RIFF/WAVE file")
    riff_size = struct.unpack_from("<I", header, 4)[0]
    if riff_size < 4 or 8 + riff_size > file_size:
        raise RiffError("invalid RIFF size")

    fmt_payload: bytes | None = None
    data_offset: int | None = None
    data_size: int | None = None
    pos = 12
    while pos + 8 <= file_size:
        fh.seek(pos)
        chunk_header = fh.read(8)
        if len(chunk_header) != 8:
            raise RiffError("truncated chunk header")
        chunk_id, chunk_size = struct.unpack("<4sI", chunk_header)
        payload_pos = pos + 8
        padded_end = payload_pos + chunk_size + (chunk_size & 1)
        if padded_end > file_size:
            raise RiffError("chunk extends beyond file")
        if chunk_id == b"fmt ":
            fh.seek(payload_pos)
            fmt_payload = fh.read(chunk_size)
            if len(fmt_payload) != chunk_size:
                raise RiffError("truncated format chunk")
        elif chunk_id == b"data":
            data_offset = payload_pos
            data_size = chunk_size
        pos = padded_end

    if fmt_payload is None or data_offset is None or data_size is None:
        raise RiffError("missing fmt or data chunk")
    sample_rate, channels, bits, block_align, _, encoding = _parse_format(fmt_payload)
    if data_size % block_align:
        raise RiffError("data size is not frame aligned")
    return WavInfo(
        path=path,
        sample_rate=sample_rate,
        channels=channels,
        bits_per_sample=bits,
        encoding=encoding,
        block_align=block_align,
        data_offset=data_offset,
        data_size=data_size,
        total_frames=data_size // block_align,
    )


def _decode(raw: bytes, info: WavInfo) -> np.ndarray:
    if info.encoding == "pcm16":
        values = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
    elif info.encoding == "pcm24":
        triples = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3)
        ints = (
            triples[:, 0].astype(np.int32)
            | (triples[:, 1].astype(np.int32) << 8)
            | (triples[:, 2].astype(np.int32) << 16)
        )
        ints = np.where(ints >= 0x800000, ints - 0x1000000, ints)
        values = ints.astype(np.float32) / 8388608.0
    elif info.encoding == "pcm32":
        values = np.frombuffer(raw, dtype="<i4").astype(np.float32) / 2147483648.0
    else:
        values = np.frombuffer(raw, dtype="<f4").astype(np.float32)
        values = np.nan_to_num(values, nan=0.0, posinf=1.0, neginf=-1.0)
        np.clip(values, -1.0, 1.0, out=values)
    return values.reshape(-1, info.channels)


class WavReader:
    """Open a WAV once and decode arbitrary frame ranges on demand."""

    def __init__(self, path: str | os.PathLike[str]):
        self.path = os.fspath(path)
        self._fh = None
        self.info: WavInfo | None = None

    def __enter__(self) -> "WavReader":
        self._fh = open(self.path, "rb")
        try:
            self.info = _parse_header(self._fh, self.path)
        except Exception:
            self._fh.close()
            self._fh = None
            raise
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        if self._fh is not None:
            self._fh.close()
            self._fh = None

    @property
    def ready(self) -> bool:
        return self._fh is not None and self.info is not None

    def iter_frames(self, block_frames: int = 65536) -> Iterator[np.ndarray]:
        if not self.ready or self.info is None or self._fh is None:
            raise RuntimeError("reader is not open")
        if block_frames <= 0:
            raise ValueError("block_frames must be positive")
        info = self.info
        self._fh.seek(info.data_offset)
        remaining = info.total_frames
        while remaining:
            count = min(block_frames, remaining)
            byte_count = count * info.block_align
            raw = self._fh.read(byte_count)
            if len(raw) != byte_count:
                raise RiffError("truncated data chunk")
            yield _decode(raw, info)
            remaining -= count

    def read_frames(self, start: int, count: int) -> np.ndarray:
        if not self.ready or self.info is None or self._fh is None:
            raise RuntimeError("reader is not open")
        info = self.info
        start = max(0, min(int(start), info.total_frames))
        count = max(0, min(int(count), info.total_frames - start))
        if count == 0:
            return np.empty((0, info.channels), dtype=np.float32)
        self._fh.seek(info.data_offset + start * info.block_align)
        raw = self._fh.read(count * info.block_align)
        if len(raw) != count * info.block_align:
            raise RiffError("truncated data chunk")
        return _decode(raw, info)
