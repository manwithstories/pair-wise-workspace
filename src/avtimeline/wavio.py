"""Minimal deterministic WAV writing.

``soundfile`` appends a ``PEAK`` chunk to floating-point WAV files whose payload
contains the wall-clock time, which would make fixture checksums differ between
runs.  This module writes the canonical 44-byte RIFF header plus raw float32 or
int16 samples, so identical arrays always produce identical bytes.
"""

from __future__ import annotations

import os
import struct

import numpy as np


def _header(channels: int, rate: int, bits: int, data_bytes: int, fmt: int) -> bytes:
    block_align = channels * (bits // 8)
    byte_rate = rate * block_align
    size = 36 + data_bytes + (data_bytes & 1)
    return (
        b"RIFF"
        + struct.pack("<I", size)
        + b"WAVE"
        + b"fmt "
        + struct.pack("<IHHIIHH", 16, fmt, channels, rate, byte_rate, block_align, bits)
        + b"data"
        + struct.pack("<I", data_bytes)
    )


def encode_wav(data: np.ndarray, rate: int, subtype: str = "FLOAT") -> bytes:
    """Encode ``(samples, channels)`` or ``(channels, samples)`` to WAV bytes."""
    array = np.asarray(data)
    if array.ndim == 1:
        array = array[:, None]
    if array.ndim != 2:
        raise ValueError("audio data must be one or two dimensional")
    # Accept both sample-major and channel-major layouts by picking the axis
    # that looks like channels for small channel counts.
    if array.shape[1] > 16 and array.shape[0] <= 16:
        array = array.T
    samples, channels = array.shape
    if subtype == "FLOAT":
        payload = np.ascontiguousarray(array, dtype="<f4").tobytes()
        bits, fmt = 32, 3
    elif subtype == "PCM_16":
        clipped = np.clip(array, -1.0, 1.0)
        scaled = np.rint(clipped * 32767.0).astype("<i2")
        payload = np.ascontiguousarray(scaled).tobytes()
        bits, fmt = 16, 1
    else:  # pragma: no cover - only two subtypes are used
        raise ValueError(f"unsupported WAV subtype {subtype!r}")
    header = _header(channels, rate, bits, len(payload), fmt)
    body = payload + (b"\0" if len(payload) & 1 else b"")
    return header + body


def write_wav(path: str, data: np.ndarray, rate: int, subtype: str = "FLOAT") -> None:
    """Atomically write *data* to *path* as a canonical WAV file."""
    payload = encode_wav(data, rate, subtype)
    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    temp = os.path.join(directory, f".wav-{os.getpid()}-{id(data)}.tmp")
    try:
        with open(temp, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, path)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def samples_in_wav(path: str) -> tuple[int, int, int]:
    """Return ``(frames, channels, rate)`` from a WAV header."""
    with open(path, "rb") as handle:
        header = handle.read(44)
    if len(header) < 44 or header[:4] != b"RIFF" or header[8:12] != b"WAVE":
        raise ValueError(f"{path} is not a canonical RIFF/WAVE file")
    channels, rate = struct.unpack("<HI", header[22:28])
    bits = struct.unpack("<H", header[34:36])[0]
    declared = struct.unpack("<I", header[40:44])[0]
    block = max(1, channels * (bits // 8))
    return declared // block, channels, rate
