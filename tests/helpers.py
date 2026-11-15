from __future__ import annotations

from pathlib import Path
import struct

import numpy as np


def encode_pcm(frames: np.ndarray, bits: int) -> bytes:
    frames = np.asarray(frames, dtype=np.float64)
    if frames.ndim == 1:
        frames = frames[:, None]
    flat = frames.reshape(-1)
    if bits == 8:
        values = np.clip(np.rint(flat * 127.0) + 128.0, 0, 255).astype(np.uint8)
        return values.tobytes()
    if bits == 16:
        values = np.clip(np.rint(flat * 32767.0), -32768, 32767).astype("<i2")
        return values.tobytes()
    if bits == 24:
        values = np.clip(np.rint(flat * 8388607.0), -8388608, 8388607).astype(np.int64)
        out = bytearray()
        for value in values:
            unsigned = int(value) & 0xFFFFFF
            out.extend((unsigned & 0xFF, (unsigned >> 8) & 0xFF, (unsigned >> 16) & 0xFF))
        return bytes(out)
    if bits == 32:
        values = np.clip(np.rint(flat * 2147483647.0), -2147483648, 2147483647).astype("<i4")
        return values.tobytes()
    raise ValueError(f"unsupported test bit depth {bits}")


def make_wav_bytes(
    frames: np.ndarray,
    *,
    sample_rate: int = 48000,
    bits: int = 16,
    format_tag: int = 1,
    byte_rate: int | None = None,
    declared_data_size: int | None = None,
    data_bytes: bytes | None = None,
) -> bytes:
    frames = np.asarray(frames)
    channels = 1 if frames.ndim == 1 else frames.shape[1]
    raw = data_bytes if data_bytes is not None else encode_pcm(frames, bits)
    block_align = channels * max(1, bits // 8)
    declared = len(raw) if declared_data_size is None else declared_data_size
    riff_size = 36 + declared
    header = struct.pack(
        "<4sI4s4sIHHIIHH4sI",
        b"RIFF",
        riff_size,
        b"WAVE",
        b"fmt ",
        16,
        format_tag,
        channels,
        sample_rate,
        byte_rate if byte_rate is not None else sample_rate * block_align,
        block_align,
        bits,
        b"data",
        declared,
    )
    return header + raw


def write_tmp_wav(path: Path, frames: np.ndarray, **kwargs) -> Path:
    path.write_bytes(make_wav_bytes(frames, **kwargs))
    return path


def decode_pcm(raw: bytes, bits: int, channels: int = 1) -> np.ndarray:
    if bits == 16:
        values = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32768.0
    elif bits == 8:
        values = (np.frombuffer(raw, dtype=np.uint8).astype(np.float64) - 128.0) / 128.0
    else:
        raise ValueError("decode helper supports 8/16 bit")
    return values.reshape(-1, channels)

