"""Minimal RIFF/WAVE reader without third-party audio frameworks.

Walks RIFF chunks, skipping unknown ones (LIST, fact, ...) and odd-size
padding bytes. Decodes PCM16/24/32 and float32, downmixes multi-channel
to mono float32. Truncated or out-of-range chunks raise dedicated
errors so the CLI can map them to exit code 3.
"""
from __future__ import annotations

import os
import struct
from dataclasses import dataclass

import numpy as np

PCM_FORMAT = 0x0001
IEEE_FLOAT_FORMAT = 0x0003


class WavError(Exception):
    """Base class for WAV input errors."""


class WavStructureError(WavError):
    """Malformed RIFF/WAVE container."""


class WavTruncatedError(WavStructureError):
    """A chunk header or payload runs past the end of the file."""


class WavUnsupportedError(WavError):
    """Well-formed WAV using an encoding we do not decode."""


@dataclass
class WavInfo:
    path: str
    audio_format: int
    channels: int
    sample_rate: int
    bits_per_sample: int
    block_align: int
    data_offset: int
    data_size: int

    @property
    def frames(self) -> int:
        return self.data_size // self.block_align


def _iter_chunks(path):
    """Yield (chunk_id, payload_offset, payload_size) for each RIFF chunk."""
    file_size = os.path.getsize(path)
    with open(path, "rb") as fh:
        header = fh.read(12)
        if len(header) < 12:
            raise WavTruncatedError(f"{path}: file too short for a RIFF header")
        if header[0:4] != b"RIFF" or header[8:12] != b"WAVE":
            raise WavStructureError(f"{path}: not a RIFF/WAVE file")
        riff_size = struct.unpack("<I", header[4:8])[0]
        limit = 8 + riff_size
        if limit > file_size:
            raise WavTruncatedError(
                f"{path}: RIFF declares {riff_size} bytes but file is {file_size}"
            )
        pos = 12
        while pos < limit:
            fh.seek(pos)
            hdr = fh.read(8)
            if len(hdr) < 8:
                raise WavTruncatedError(
                    f"{path}: truncated chunk header at offset {pos}"
                )
            cid = hdr[0:4]
            size = struct.unpack("<I", hdr[4:8])[0]
            data_off = pos + 8
            if data_off + size > limit:
                raise WavTruncatedError(
                    f"{path}: chunk {cid.decode('latin1')!r} at {pos} declares "
                    f"{size} bytes running past end of file"
                )
            yield cid, data_off, size
            pos = data_off + size + (size & 1)  # odd sizes are padded


def read_info(path: str) -> WavInfo:
    """Parse fmt/data chunk locations; skip LIST, fact and unknown chunks."""
    fmt_payload = None
    data_off = data_size = None
    for cid, off, size in _iter_chunks(path):
        if cid == b"fmt " and fmt_payload is None:
            with open(path, "rb") as fh:
                fh.seek(off)
                fmt_payload = fh.read(size)
        elif cid == b"data" and data_off is None:
            data_off, data_size = off, size
    if fmt_payload is None:
        raise WavStructureError(f"{path}: missing fmt chunk")
    if data_off is None:
        raise WavStructureError(f"{path}: missing data chunk")
    if len(fmt_payload) < 16:
        raise WavTruncatedError(f"{path}: fmt chunk too short")
    (audio_format, channels, sample_rate, _byte_rate,
     block_align, bits) = struct.unpack("<HHIIHH", fmt_payload[:16])
    if audio_format not in (PCM_FORMAT, IEEE_FLOAT_FORMAT):
        raise WavUnsupportedError(
            f"{path}: unsupported audio format {audio_format} "
            f"(only PCM and float32 are decoded)"
        )
    if audio_format == PCM_FORMAT and bits not in (16, 24, 32):
        raise WavUnsupportedError(f"{path}: unsupported PCM depth {bits}")
    if audio_format == IEEE_FLOAT_FORMAT and bits != 32:
        raise WavUnsupportedError(f"{path}: unsupported float depth {bits}")
    if channels < 1 or sample_rate <= 0 or block_align <= 0:
        raise WavStructureError(f"{path}: invalid fmt fields")
    return WavInfo(path, audio_format, channels, sample_rate, bits,
                   block_align, data_off, data_size)


def _decode_pcm24(raw: bytes) -> np.ndarray:
    buf = np.frombuffer(raw, dtype=np.uint8)
    buf = buf[: buf.size // 3 * 3].reshape(-1, 3).astype(np.int32)
    out = buf[:, 0] | (buf[:, 1] << 8) | (buf[:, 2] << 16)
    out = np.where(out & 0x800000, out - 0x1000000, out)
    return out.astype(np.float32) / 8388608.0


def read_mono(path: str):
    """Return (mono float32 samples in [-1, 1], WavInfo)."""
    info = read_info(path)
    frames = info.frames
    n_bytes = frames * info.block_align
    with open(path, "rb") as fh:
        fh.seek(info.data_offset)
        raw = fh.read(n_bytes)
    if len(raw) < n_bytes:
        raise WavTruncatedError(f"{path}: data chunk shorter than declared")
    if info.audio_format == IEEE_FLOAT_FORMAT:
        data = np.frombuffer(raw, dtype="<f4").astype(np.float32)
    elif info.bits_per_sample == 16:
        data = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
    elif info.bits_per_sample == 24:
        data = _decode_pcm24(raw)
    else:
        data = np.frombuffer(raw, dtype="<i4").astype(np.float32) / 2147483648.0
    data = data.reshape(frames, info.channels)
    if info.channels > 1:
        mono = data.mean(axis=1, dtype=np.float32)
    else:
        mono = data[:, 0]
    return np.ascontiguousarray(mono), info
