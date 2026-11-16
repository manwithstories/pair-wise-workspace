"""WAV intake: PCM 16/24/32-bit, mono/stereo, downmixed to float32 mono."""
from __future__ import annotations

import hashlib
import wave
from pathlib import Path

import numpy as np

VALID_SAMPLE_RATES = frozenset(
    {8000, 11025, 16000, 22050, 24000, 32000, 44100, 48000,
     88200, 96000, 176400, 192000}
)
_CHUNK_FRAMES = 1 << 20


class WavFormatError(Exception):
    """Unsupported or unreadable input; maps to exit code 2."""


def safe_resolve(path, base=None):
    """Normalize a path and optionally confine it under ``base``."""
    resolved = Path(path).expanduser().resolve()
    if base is not None:
        root = Path(base).expanduser().resolve()
        if resolved != root and root not in resolved.parents:
            raise WavFormatError(f"path escapes input directory: {path}")
    return resolved


def file_sha256(path, chunk_size=1 << 20):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(chunk_size), b""):
            digest.update(block)
    return digest.hexdigest()


def _decode_block(raw, sampwidth):
    if sampwidth == 2:
        return np.frombuffer(raw, dtype="<i2").astype(np.float32) * (1.0 / 32768.0)
    if sampwidth == 3:
        octets = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3).astype(np.int32)
        values = octets[:, 0] | (octets[:, 1] << 8) | (octets[:, 2] << 16)
        values = np.where(values >= 1 << 23, values - (1 << 24), values)
        return values.astype(np.float32) * (1.0 / 8388608.0)
    if sampwidth == 4:
        return np.frombuffer(raw, dtype="<i4").astype(np.float32) * (1.0 / 2147483648.0)
    raise WavFormatError(f"unsupported sample width: {sampwidth}")


def read_wav(path):
    """Decode a PCM WAV file to mono float32 samples in [-1, 1].

    Stereo is downmixed by channel mean. Returns (samples, sample_rate).
    """
    candidate = Path(path)
    if candidate.suffix.lower() != ".wav":
        raise WavFormatError(f"not a .wav file: {candidate}")
    try:
        reader = wave.open(str(candidate), "rb")
    except (wave.Error, OSError, EOFError) as exc:
        raise WavFormatError(f"cannot open {candidate}: {exc}") from exc
    with reader:
        channels = reader.getnchannels()
        sampwidth = reader.getsampwidth()
        rate = reader.getframerate()
        frames = reader.getnframes()
        if channels not in (1, 2):
            raise WavFormatError(f"unsupported channel count {channels}: {candidate}")
        if sampwidth not in (2, 3, 4):
            raise WavFormatError(f"unsupported sample width {sampwidth}: {candidate}")
        if rate not in VALID_SAMPLE_RATES:
            raise WavFormatError(f"unsupported sample rate {rate}: {candidate}")
        out = np.empty(frames, dtype=np.float32)
        pos = 0
        while pos < frames:
            want = min(_CHUNK_FRAMES, frames - pos)
            block = _decode_block(reader.readframes(want), sampwidth)
            if channels == 2:
                block = block.reshape(-1, 2).mean(axis=1, dtype=np.float32)
            count = block.shape[0]
            out[pos:pos + count] = block
            pos += count
    return out, rate
