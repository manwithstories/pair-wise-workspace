"""Streaming RIFF/WAVE parser with chunked mono reads and PCM writer."""

from __future__ import annotations

import hashlib
import struct
import warnings
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import AudioReadError

WAVE_FORMAT_PCM = 0x0001
_SUPPORTED_BITS = (16, 24, 32)
_HASH_CHUNK = 1 << 20


@dataclass(frozen=True)
class WavInfo:
    path: Path
    channels: int
    sample_rate: int
    bits_per_sample: int
    total_frames: int
    data_offset: int
    block_align: int
    sha256: str
    truncated: bool


def _decode_pcm(raw: bytes, bits: int, channels: int) -> np.ndarray:
    """Decode little-endian integer PCM into mono float64 in [-1, 1]."""
    if bits == 16:
        samples = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32768.0
    elif bits == 32:
        samples = np.frombuffer(raw, dtype="<i4").astype(np.float64) / 2147483648.0
    else:
        bytes_in = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int64)
        words = bytes_in[:, 0] | (bytes_in[:, 1] << 8) | (bytes_in[:, 2] << 16)
        words = np.where(words & 0x800000, words - 0x1000000, words)
        samples = words.astype(np.float64) / 8388608.0
    frames = samples.reshape(-1, channels)
    if channels == 1:
        return frames[:, 0].copy()
    return frames.mean(axis=1)


class WavReader:
    """Opens a WAV once and serves repeated streaming block/range reads."""

    def __init__(self, path: str | Path):
        self.path = Path(path)
        if not self.path.is_file():
            raise AudioReadError(f"not a file: {self.path}")
        self._fh = self.path.open("rb")
        try:
            self.info = self._parse_header()
        except AudioReadError:
            self._fh.close()
            raise
        self._frame_bytes = self.info.block_align

    def _read_exact(self, size: int) -> bytes:
        data = self._fh.read(size)
        if len(data) != size:
            raise AudioReadError(f"unexpected EOF in {self.path}")
        return data

    def _parse_header(self) -> WavInfo:
        if self._fh.read(4) != b"RIFF":
            raise AudioReadError(f"not a RIFF container: {self.path}")
        self._fh.read(4)  # RIFF payload size, unused
        if self._fh.read(4) != b"WAVE":
            raise AudioReadError(f"not a WAVE container: {self.path}")
        fmt = None
        data_offset = data_size = -1
        while True:
            header = self._fh.read(8)
            if len(header) < 8:
                break
            chunk_id, chunk_size = struct.unpack("<4sI", header)
            if chunk_id == b"fmt ":
                fmt = self._read_exact(chunk_size)
            elif chunk_id == b"data":
                data_offset = self._fh.tell()
                data_size = chunk_size
                break
            else:
                self._fh.seek(chunk_size + (chunk_size & 1), 1)
        if fmt is None or data_offset < 0:
            raise AudioReadError(f"missing fmt/data chunk: {self.path}")
        tag, channels, rate, _, block_align, bits = struct.unpack("<HHIIHH", fmt[:16])
        if tag != WAVE_FORMAT_PCM:
            raise AudioReadError(f"non-PCM format tag {tag}: {self.path}")
        if channels not in (1, 2):
            raise AudioReadError(f"unsupported channel count {channels}: {self.path}")
        if bits not in _SUPPORTED_BITS:
            raise AudioReadError(f"unsupported bit depth {bits}: {self.path}")
        if block_align != channels * bits // 8:
            raise AudioReadError(f"inconsistent block align: {self.path}")
        actual = self.path.stat().st_size - data_offset
        truncated = data_size > actual
        if truncated:
            warnings.warn(
                f"{self.path.name}: data chunk declares {data_size} bytes, "
                f"only {max(actual, 0)} available; truncating",
                stacklevel=2,
            )
        usable = max(min(data_size, actual), 0)
        total_frames = usable // block_align
        digest = hashlib.sha256()
        self._fh.seek(0)
        while True:
            block = self._fh.read(_HASH_CHUNK)
            if not block:
                break
            digest.update(block)
        self._fh.seek(data_offset)
        return WavInfo(self.path, channels, rate, bits, total_frames,
                       data_offset, block_align, digest.hexdigest(), truncated)

    def iter_mono_blocks(self, block_frames: int):
        """Yield down-mixed float64 blocks; a trailing partial block is skipped."""
        self._fh.seek(self.info.data_offset)
        remaining = self.info.total_frames
        while remaining > 0:
            take = min(block_frames, remaining)
            raw = self._fh.read(take * self._frame_bytes)
            if not raw:
                break
            usable = (len(raw) // self._frame_bytes) * self._frame_bytes
            if usable == 0:
                break
            yield _decode_pcm(raw[:usable], self.info.bits_per_sample,
                              self.info.channels)
            remaining -= usable // self._frame_bytes

    def read_mono_range(self, start_frame: int, end_frame: int) -> np.ndarray:
        start_frame = max(0, start_frame)
        end_frame = min(self.info.total_frames, end_frame)
        if end_frame <= start_frame:
            return np.empty(0, dtype=np.float64)
        self._fh.seek(self.info.data_offset + start_frame * self._frame_bytes)
        count = end_frame - start_frame
        raw = self._fh.read(count * self._frame_bytes)
        usable = (len(raw) // self._frame_bytes) * self._frame_bytes
        if usable == 0:
            return np.empty(0, dtype=np.float64)
        return _decode_pcm(raw[:usable], self.info.bits_per_sample,
                           self.info.channels)

    def close(self) -> None:
        self._fh.close()

    def __enter__(self) -> "WavReader":
        return self

    def __exit__(self, *exc) -> None:
        self.close()


def _encode_pcm(samples: np.ndarray, bits: int) -> bytes:
    clipped = np.clip(samples, -1.0, 1.0)
    if bits == 16:
        quantized = np.rint(clipped * 32767.0).astype("<i2")
        return quantized.tobytes()
    if bits == 32:
        quantized = np.rint(clipped * 2147483647.0).astype("<i4")
        return quantized.tobytes()
    quantized = np.rint(clipped * 8388607.0).astype(np.int64) & 0xFFFFFF
    out = np.empty((quantized.size, 3), dtype=np.uint8)
    out[:, 0] = quantized & 0xFF
    out[:, 1] = (quantized >> 8) & 0xFF
    out[:, 2] = (quantized >> 16) & 0xFF
    return out.tobytes()


def write_pcm_wav(path: str | Path, samples: np.ndarray, sample_rate: int,
                  bits: int = 16, channels: int = 1) -> None:
    """Write an interleaved PCM WAV (mono or stereo) for synth and export."""
    if bits not in _SUPPORTED_BITS:
        raise AudioReadError(f"unsupported export bit depth {bits}")
    if channels not in (1, 2):
        raise AudioReadError(f"unsupported channel count {channels}")
    payload = _encode_pcm(np.asarray(samples, dtype=np.float64), bits)
    block_align = channels * bits // 8
    header = struct.pack("<4sI4s4sIHHIIHH4sI",
                         b"RIFF", 36 + len(payload), b"WAVE", b"fmt ",
                         16, WAVE_FORMAT_PCM, channels, sample_rate,
                         sample_rate * block_align, block_align, bits,
                         b"data", len(payload))
    with Path(path).open("wb") as fh:
        fh.write(header)
        fh.write(payload)
