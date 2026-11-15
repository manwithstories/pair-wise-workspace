"""Strict WAV parsing, streaming mono reads, and atomic PCM writes."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import os
from pathlib import Path
import struct
import tempfile

import numpy as np

from .errors import AudioError, FileSystemError


@dataclass(frozen=True)
class WavHeader:
    path: Path
    sample_rate: int
    channels: int
    bits_per_sample: int
    block_align: int
    total_samples: int
    data_offset: int
    data_size: int
    available_data_size: int


def _u16(data: bytes, offset: int) -> int:
    return struct.unpack_from("<H", data, offset)[0]


def _u32(data: bytes, offset: int) -> int:
    return struct.unpack_from("<I", data, offset)[0]


class WavReader:
    """A small PCM WAV reader whose public iteration is always mono float32."""

    def __init__(self, path: str | os.PathLike[str]):
        self.path = Path(path)
        self._fp = None
        try:
            self._fp = self.path.open("rb")
        except OSError as exc:
            raise FileSystemError("filesystem_error", str(exc), path=str(self.path)) from exc
        try:
            self.file_size = os.fstat(self._fp.fileno()).st_size
            self.header = self._parse_header()
        except Exception:
            if self._fp is not None:
                self._fp.close()
            raise

    def __enter__(self) -> "WavReader":
        return self

    def __exit__(self, *exc_info) -> None:
        self.close()

    def close(self) -> None:
        if self._fp is not None:
            self._fp.close()
            self._fp = None

    def _error(self, code: str, message: str) -> AudioError:
        return AudioError(code, message, path=str(self.path))

    def _parse_header(self) -> WavHeader:
        fp = self._fp
        fp.seek(0)
        head = fp.read(12)
        if len(head) < 12 or head[:4] != b"RIFF" or head[8:12] != b"WAVE":
            raise self._error("truncated_header", "not a complete RIFF/WAVE header")
        riff_end = 8 + _u32(head, 4)
        scan_end = min(self.file_size, riff_end)
        fmt = None
        data_info = None
        position = 12
        while position + 8 <= scan_end:
            fp.seek(position)
            chunk_head = fp.read(8)
            if len(chunk_head) < 8:
                raise self._error("truncated_header", "truncated chunk header")
            chunk_id = chunk_head[:4]
            chunk_size = _u32(chunk_head, 4)
            payload = position + 8
            if chunk_id == b"fmt ":
                if chunk_size < 16 or payload + chunk_size > scan_end:
                    raise self._error("truncated_header", "truncated fmt chunk")
                fmt = fp.read(chunk_size)
            elif chunk_id == b"data":
                available = max(0, scan_end - payload)
                data_info = (payload, chunk_size, min(chunk_size, available))
                break
            next_position = payload + chunk_size + (chunk_size & 1)
            if next_position <= position:
                raise self._error("invalid_header", "invalid chunk size")
            position = next_position
        if fmt is None:
            if self.file_size < riff_end:
                raise self._error("truncated_header", "file ends before fmt chunk")
            raise self._error("invalid_header", "missing fmt chunk")
        if data_info is None:
            if self.file_size < riff_end:
                raise self._error("truncated_header", "file ends before data chunk")
            raise self._error("invalid_header", "missing data chunk")

        format_tag = _u16(fmt, 0)
        channels = _u16(fmt, 2)
        sample_rate = _u32(fmt, 4)
        byte_rate = _u32(fmt, 8)
        block_align = _u16(fmt, 12)
        bits = _u16(fmt, 14)
        if format_tag != 1:
            raise self._error("non_pcm", f"unsupported WAV format tag {format_tag}")
        if bits not in (8, 16, 24, 32):
            raise self._error("invalid_bit_depth", f"unsupported bit depth {bits}")
        if channels < 1:
            raise self._error("invalid_header", "channel count must be positive")
        expected_align = channels * (bits // 8)
        if block_align != expected_align:
            raise self._error("invalid_header", "block alignment does not match channels")
        if sample_rate <= 0 or byte_rate != sample_rate * block_align:
            raise self._error(
                "sample_rate_mismatch", "byte rate does not match declared sample rate"
            )
        offset, declared_size, available_size = data_info
        if declared_size % block_align:
            raise self._error("invalid_header", "data chunk ends inside a sample frame")
        if available_size == 0:
            raise self._error("empty_audio", "WAV contains no sample data")
        return WavHeader(
            path=self.path,
            sample_rate=sample_rate,
            channels=channels,
            bits_per_sample=bits,
            block_align=block_align,
            total_samples=available_size // block_align,
            data_offset=offset,
            data_size=declared_size,
            available_data_size=available_size,
        )

    def _ensure_open(self) -> None:
        if self._fp is None:
            raise self._error("file_closed", "WAV reader is closed")

    def _read_bytes(self, start: int, count: int) -> bytes:
        self._ensure_open()
        if start < 0 or count < 0:
            raise self._error("invalid_range", "negative sample range")
        byte_start = start * self.header.block_align
        byte_count = count * self.header.block_align
        if byte_start + byte_count > self.header.available_data_size:
            raise self._error("unexpected_eof", "unexpected end of WAV sample data")
        self._fp.seek(self.header.data_offset + byte_start)
        payload = self._fp.read(byte_count)
        if len(payload) != byte_count:
            raise self._error("unexpected_eof", "unexpected end of WAV sample data")
        return payload

    def _decode(self, payload: bytes) -> np.ndarray:
        bits = self.header.bits_per_sample
        channels = self.header.channels
        if bits == 8:
            raw = np.frombuffer(payload, dtype=np.uint8).astype(np.float32)
            mono = (raw - 128.0) / 128.0
        elif bits == 16:
            mono = np.frombuffer(payload, dtype="<i2").astype(np.float32) / 32768.0
        elif bits == 24:
            raw = np.frombuffer(payload, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
            values = raw[:, 0] | (raw[:, 1] << 8) | (raw[:, 2] << 16)
            values = (values ^ 0x800000) - 0x800000
            mono = values.astype(np.float32) / 8388608.0
        else:
            mono = np.frombuffer(payload, dtype="<i4").astype(np.float32) / 2147483648.0
        if channels > 1:
            mono = mono.reshape(-1, channels).mean(axis=1, dtype=np.float32)
        return np.ascontiguousarray(mono, dtype=np.float32)

    def iter_mono(self, block_frames: int = 65536) -> "object":
        self._ensure_open()
        if block_frames < 1:
            raise ValueError("block_frames must be positive")
        remaining = self.header.data_size // self.header.block_align
        position = 0
        while remaining:
            count = min(block_frames, remaining)
            payload = self._read_bytes(position, count)
            yield self._decode(payload)
            position += count
            remaining -= count

    def read_range(self, start: int, end: int) -> np.ndarray:
        if start < 0 or end < start or end > self.header.total_samples:
            raise self._error("invalid_range", "sample range is outside the audio")
        return self._decode(self._read_bytes(start, end - start))

    def iter_range(self, start: int, end: int, block_frames: int = 65536):
        if start < 0 or end < start or end > self.header.total_samples:
            raise self._error("invalid_range", "sample range is outside the audio")
        position = start
        while position < end:
            count = min(block_frames, end - position)
            yield self._decode(self._read_bytes(position, count))
            position += count


def read_header(path: str | os.PathLike[str]) -> WavHeader:
    with WavReader(path) as reader:
        return reader.header


def write_wav_atomic(path: str | os.PathLike[str], sample_rate: int, samples: np.ndarray) -> None:
    target = Path(path)
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        pcm = np.asarray(samples)
        if pcm.dtype != np.int16:
            pcm = np.clip(np.rint(pcm * 32767.0), -32768, 32767).astype("<i2")
        else:
            pcm = pcm.astype("<i2", copy=False)
        payload = np.ascontiguousarray(pcm).tobytes()
        header = struct.pack(
            "<4sI4s4sIHHIIHH4sI",
            b"RIFF", 36 + len(payload), b"WAVE", b"fmt ", 16, 1, 1,
            sample_rate, sample_rate * 2, 2, 16, b"data", len(payload),
        )
        fd, temp_name = tempfile.mkstemp(prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
        try:
            with os.fdopen(fd, "wb") as fp:
                fp.write(header)
                fp.write(payload)
                fp.flush()
                os.fsync(fp.fileno())
            os.replace(temp_name, target)
        except Exception:
            try:
                os.unlink(temp_name)
            except OSError:
                pass
            raise
    except OSError as exc:
        raise FileSystemError("filesystem_error", str(exc), path=str(target)) from exc


def write_wav_range_atomic(
    path: str | os.PathLike[str],
    sample_rate: int,
    reader: WavReader,
    start: int,
    end: int,
    block_frames: int = 65536,
) -> None:
    target = Path(path)
    frame_count = end - start
    if frame_count < 0:
        raise AudioError("invalid_range", "negative segment length", path=str(target))
    payload_size = frame_count * 2
    header = struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF", 36 + payload_size, b"WAVE", b"fmt ", 16, 1, 1,
        sample_rate, sample_rate * 2, 2, 16, b"data", payload_size,
    )
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        fd, temp_name = tempfile.mkstemp(prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
        try:
            with os.fdopen(fd, "wb") as fp:
                fp.write(header)
                for chunk in reader.iter_range(start, end, block_frames):
                    pcm = np.clip(np.rint(chunk * 32767.0), -32768, 32767).astype("<i2")
                    fp.write(pcm.tobytes())
                fp.flush()
                os.fsync(fp.fileno())
            os.replace(temp_name, target)
        except Exception:
            try:
                os.unlink(temp_name)
            except OSError:
                pass
            raise
    except OSError as exc:
        raise FileSystemError("filesystem_error", str(exc), path=str(target)) from exc


def sha256_file(path: str | os.PathLike[str], block_size: int = 1024 * 1024) -> str:
    digest = hashlib.sha256()
    try:
        with open(path, "rb") as fp:
            while True:
                chunk = fp.read(block_size)
                if not chunk:
                    break
                digest.update(chunk)
    except OSError as exc:
        raise FileSystemError("filesystem_error", str(exc), path=str(path)) from exc
    return digest.hexdigest()
