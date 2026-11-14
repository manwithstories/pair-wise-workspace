"""Chunked RIFF/WAVE reader-writer for 16/24-bit PCM, mono/stereo."""

from __future__ import annotations

import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import UnsupportedFormatError

_PCM_FORMAT = 1
_SUPPORTED_BITS = (16, 24)
MIN_SAMPLE_RATE = 8000
MAX_SAMPLE_RATE = 96000


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    bits_per_sample: int
    data_offset: int
    data_size: int

    @property
    def bytes_per_sample(self) -> int:
        return self.bits_per_sample // 8

    @property
    def num_samples(self) -> int:
        return self.data_size // (self.bytes_per_sample * self.channels)


def read_info(path) -> WavInfo:
    """Parse RIFF chunks, skipping unknown ones (odd sizes padded)."""
    fmt = None
    data_offset = data_size = None
    with open(path, "rb") as fh:
        header = fh.read(12)
        if len(header) < 12 or header[:4] != b"RIFF" or header[8:12] != b"WAVE":
            raise UnsupportedFormatError(f"not a RIFF/WAVE file: {path}", path=str(path))
        while True:
            chunk_header = fh.read(8)
            if len(chunk_header) < 8:
                break
            chunk_id, chunk_size = chunk_header[:4], struct.unpack("<I", chunk_header[4:])[0]
            if chunk_id == b"fmt ":
                fmt = fh.read(chunk_size)
            elif chunk_id == b"data":
                data_offset, data_size = fh.tell(), chunk_size
                fh.seek(chunk_size, 1)
            else:
                fh.seek(chunk_size, 1)
            if chunk_size % 2:  # RIFF pads odd-sized chunks to even
                fh.seek(1, 1)
    if fmt is None or len(fmt) < 16:
        raise UnsupportedFormatError(f"missing fmt chunk: {path}", path=str(path))
    audio_format, channels, sample_rate, _br, _ba, bits = struct.unpack("<HHIIHH", fmt[:16])
    if audio_format != _PCM_FORMAT:
        raise UnsupportedFormatError(
            f"unsupported encoding (format {audio_format}, only PCM): {path}", path=str(path))
    if bits not in _SUPPORTED_BITS:
        raise UnsupportedFormatError(f"unsupported bit depth {bits}: {path}", path=str(path))
    if channels not in (1, 2):
        raise UnsupportedFormatError(f"unsupported channel count {channels}: {path}", path=str(path))
    if not (MIN_SAMPLE_RATE <= sample_rate <= MAX_SAMPLE_RATE):
        raise UnsupportedFormatError(f"sample rate {sample_rate} out of range: {path}", path=str(path))
    if data_offset is None:
        raise UnsupportedFormatError(f"missing data chunk: {path}", path=str(path))
    return WavInfo(sample_rate, channels, bits, data_offset, data_size)


def decode_pcm(raw: bytes, bits: int, channels: int) -> np.ndarray:
    """Decode interleaved PCM bytes to mono float64 in [-1, 1)."""
    if bits == 16:
        data = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32768.0
    else:
        triplets = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        wide = triplets[:, 0] | (triplets[:, 1] << 8) | (triplets[:, 2] << 16)
        wide = (wide << 8) >> 8  # sign-extend 24 -> 32 bit
        data = wide.astype(np.float64) / 8388608.0
    if channels == 2:
        data = data.reshape(-1, 2).mean(axis=1)
    return data


def iter_blocks(path, info: WavInfo | None = None, block_samples: int = 1 << 20):
    """Yield mono float64 blocks of at most ``block_samples`` samples."""
    info = info or read_info(path)
    frame_bytes = info.bytes_per_sample * info.channels
    with open(path, "rb") as fh:
        fh.seek(info.data_offset)
        remaining = info.data_size
        while remaining > 0:
            want = min(remaining, block_samples * frame_bytes)
            want -= want % frame_bytes
            if want <= 0:
                break
            raw = fh.read(want)
            if not raw:
                break
            remaining -= len(raw)
            yield decode_pcm(raw, info.bits_per_sample, info.channels)


def write_wav(path, samples: np.ndarray, sample_rate: int, bits: int = 16,
              channels: int = 1, extra_chunks: tuple = ()):
    """Write mono/stereo float samples ([-1,1]) as PCM WAV (test/fixture aid)."""
    samples = np.asarray(samples, dtype=np.float64)
    if channels == 2:
        if samples.ndim == 1:
            samples = np.column_stack([samples, samples])
        inter = samples.reshape(-1)
    else:
        inter = samples.reshape(-1)
    inter = np.clip(inter, -1.0, 1.0 - 1e-9)
    if bits == 16:
        payload = (inter * 32768.0).astype("<i2").tobytes()
    elif bits == 24:
        ints = (inter * 8388608.0).astype(np.int32)
        b = ints.view(np.uint8).reshape(-1, 4)[:, :3]
        payload = b.tobytes()
    else:
        raise ValueError(f"bits must be 16 or 24, got {bits}")
    block_align = channels * bits // 8
    fmt = struct.pack("<HHIIHH", _PCM_FORMAT, channels, sample_rate,
                      sample_rate * block_align, block_align, bits)
    chunks = [(b"fmt ", fmt)] + list(extra_chunks) + [(b"data", payload)]
    riff_size = 4 + sum(8 + len(body) + len(body) % 2 for _, body in chunks)
    with open(path, "wb") as fh:
        fh.write(b"RIFF" + struct.pack("<I", riff_size) + b"WAVE")
        for chunk_id, body in chunks:
            fh.write(chunk_id + struct.pack("<I", len(body)) + body)
            if len(body) % 2:
                fh.write(b"\x00")
    return Path(path)
