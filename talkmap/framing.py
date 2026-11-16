"""Streaming hop-window framing with RMS energy and dBFS conversion."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .errors import ParamError

_DB_FLOOR = -240.0


def rms_to_dbfs(rms: np.ndarray | float) -> np.ndarray | float:
    values = np.asarray(rms, dtype=np.float64)
    with np.errstate(divide="ignore"):
        db = 20.0 * np.log10(np.maximum(values, 10.0 ** (_DB_FLOOR / 20.0)))
    return np.where(values <= 0.0, _DB_FLOOR, db)


def dbfs_to_rms(db: np.ndarray | float) -> np.ndarray | float:
    return 10.0 ** (np.asarray(db, dtype=np.float64) / 20.0)


@dataclass(frozen=True)
class FrameEnergy:
    index: int
    start_sample: int
    rms: float


class Framer:
    """Accumulates streamed samples and emits full frames only.

    A frame starts every ``hop_samples``; frames shorter than ``frame_samples``
    at the tail are dropped instead of being zero-padded, so every emitted
    frame maps to a real sample range [start_sample, start_sample + frame_len).
    """

    def __init__(self, sample_rate: int, frame_ms: float, hop_ms: float):
        if frame_ms <= 0 or hop_ms <= 0 or hop_ms > frame_ms:
            raise ParamError("require 0 < hop_ms <= frame_ms")
        self.sample_rate = sample_rate
        self.frame_samples = round(sample_rate * frame_ms / 1000.0)
        self.hop_samples = round(sample_rate * hop_ms / 1000.0)
        if self.frame_samples <= 0 or self.hop_samples <= 0:
            raise ParamError("frame/hop duration shorter than one sample")
        self._buffer = np.empty(0, dtype=np.float64)
        self._base_sample = 0
        self._next_frame = 0

    def push(self, block: np.ndarray) -> list[FrameEnergy]:
        if block.size:
            self._buffer = np.concatenate((self._buffer, np.asarray(block, np.float64)))
        emitted: list[FrameEnergy] = []
        while self._buffer.size - self._next_frame * self.hop_samples >= self.frame_samples:
            offset = self._next_frame * self.hop_samples
            frame = self._buffer[offset:offset + self.frame_samples]
            start = self._base_sample + offset
            rms = float(np.sqrt(np.mean(frame * frame)))
            emitted.append(FrameEnergy(self._next_frame, start, rms))
            self._next_frame += 1
        consumed = self._next_frame * self.hop_samples
        if consumed:
            self._buffer = self._buffer[consumed:]
            self._base_sample += consumed
            self._next_frame = 0
        return emitted

    def finish(self) -> int:
        """Return the number of trailing samples that never formed a frame."""
        return int(self._buffer.size)
