"""Frame/hop derivation and streaming dBFS envelope computation."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

FLOOR_DB = -120.0


def frame_params(sample_rate: int, frame_ms: float = 20.0, hop_ms: float = 10.0):
    """Derive frame and hop lengths in samples from the sample rate."""
    frame = max(1, int(round(sample_rate * frame_ms / 1000.0)))
    hop = max(1, int(round(sample_rate * hop_ms / 1000.0)))
    return frame, hop


@dataclass
class EnvelopeResult:
    env_db: np.ndarray      # dBFS per full frame
    tail_db: float | None   # dBFS of the sub-frame remainder, if any
    tail_samples: int
    total_samples: int


class FrameEnveloper:
    """Streaming RMS->dBFS envelope over arbitrarily sized blocks."""

    def __init__(self, frame: int, hop: int):
        self.frame, self.hop = frame, hop
        self._buf = np.empty(0, dtype=np.float64)
        self._env: list[float] = []
        self.total_samples = 0

    def update(self, block: np.ndarray) -> None:
        self.total_samples += block.size
        self._buf = np.concatenate([self._buf, block])
        while self._buf.size >= self.frame:
            win = self._buf[: self.frame]
            self._env.append(_to_db(win))
            self._buf = self._buf[self.hop:]

    def finish(self) -> EnvelopeResult:
        n_frames = len(self._env)
        covered = (n_frames - 1) * self.hop + self.frame if n_frames else 0
        tail = self.total_samples - covered
        tail_db = _to_db(self._buf[-tail:]) if tail > 0 else None
        return EnvelopeResult(np.array(self._env), tail_db, tail, self.total_samples)


def _to_db(win: np.ndarray) -> float:
    if win.size == 0:
        return FLOOR_DB
    rms = float(np.sqrt(np.mean(win * win)))
    if rms <= 0.0:
        return FLOOR_DB
    return max(20.0 * np.log10(rms), FLOOR_DB)


def envelope_dbfs(blocks, frame: int, hop: int) -> EnvelopeResult:
    env = FrameEnveloper(frame, hop)
    for block in blocks:
        env.update(block)
    return env.finish()
