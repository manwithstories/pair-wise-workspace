"""Local WAV and JSON I/O for NumPy arrays and render metadata.

WAV is written as 16-bit PCM (the universal game-audio container) from
float64 arrays in the range [-1, 1]. Over-unity samples are hard-clipped
and the clip is reported, never silently truncated. JSON goes through a
small encoder so NumPy scalars/arrays and dataclass-like records survive
``json.dump`` without custom callers.
"""
from __future__ import annotations

import json
import wave
from dataclasses import asdict, is_dataclass
from pathlib import Path

import numpy as np


def _to_2d(audio: np.ndarray) -> np.ndarray:
    a = np.asarray(audio, dtype=np.float64)
    return a if a.ndim == 2 else a[:, None]


def write_wav(path, audio: np.ndarray, sample_rate: int) -> dict:
    """Write a float64 ``(n, channels)`` array to 16-bit PCM WAV."""
    data = _to_2d(audio)
    n, channels = data.shape
    if channels not in (1, 2):
        raise ValueError(f"WAV supports 1 or 2 channels, got {channels}")

    clipped = int(np.sum(np.abs(data) > 1.0))
    bounded = np.clip(data, -1.0, 1.0)
    pcm = np.round(bounded * 32767.0).astype("<i2")
    interleaved = pcm.reshape(-1).tobytes()

    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(2)
        wf.setframerate(int(sample_rate))
        wf.writeframes(interleaved)
    return {
        "path": str(path),
        "samples": int(n),
        "channels": int(channels),
        "sample_rate": int(sample_rate),
        "clipped_samples": clipped,
    }


def read_wav(path) -> np.ndarray:
    """Read a PCM WAV back to a float64 ``(n, channels)`` array."""
    with wave.open(str(path), "rb") as wf:
        channels = wf.getnchannels()
        width = wf.getsampwidth()
        rate = wf.getframerate()
        raw = wf.readframes(wf.getnframes())
    if width != 2:
        raise ValueError(f"only 16-bit PCM WAV is supported, got width {width}")
    pcm = np.frombuffer(raw, dtype="<i2").astype(np.float64) / 32767.0
    return pcm.reshape(-1, channels), rate


class _ReverbEncoder(json.JSONEncoder):
    def default(self, obj):
        if isinstance(obj, np.ndarray):
            return obj.tolist()
        if isinstance(obj, np.floating):
            return float(obj)
        if isinstance(obj, np.integer):
            return int(obj)
        if is_dataclass(obj):
            return asdict(obj)
        if isinstance(obj, Path):
            return str(obj)
        return super().default(obj)


def write_json(path, payload) -> str:
    """Write any JSON-serialisable structure (NumPy/dataclass aware)."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2, cls=_ReverbEncoder, ensure_ascii=False)
    return str(path)


def read_json(path):
    with Path(path).open("r", encoding="utf-8") as fh:
        return json.load(fh)
