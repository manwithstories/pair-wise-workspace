"""First-order encoder: direction + mono signal -> 4-channel B-format."""
from __future__ import annotations

from typing import Sequence

import numpy as np

from .harmonics import basis_vector
from .types import AudioFrame, Diagnostics, Direction, NormMode


class Encoder:
    """Encode mono sources into first-order B-format (W, Y, Z, X)."""

    def __init__(self, mode: NormMode, sample_rate: int = 48000) -> None:
        self.mode = mode
        self.sample_rate = int(sample_rate)

    def encode(self, direction: Direction, mono: np.ndarray) -> np.ndarray:
        """Encode one mono signal; returns (frames, 4)."""
        mono = np.asarray(mono, dtype=np.float64)
        if mono.ndim != 1:
            raise ValueError("mono signal must be 1-D")
        if mono.size == 0:
            raise ValueError("mono signal must not be empty")
        return mono[:, None] * basis_vector(direction, self.mode)[None, :]

    def encode_mix(
        self, sources: Sequence[tuple[Direction, np.ndarray]]
    ) -> tuple[AudioFrame, Diagnostics]:
        """Superpose several sources frame-by-frame.

        Peak overflow is reported through Diagnostics, never silently
        clipped, so downstream stages can decide how to recover.
        """
        if not sources:
            raise ValueError("at least one source is required")
        lengths = {np.asarray(sig).shape[0] for _, sig in sources}
        if len(lengths) != 1:
            raise ValueError(f"source frame counts differ: {sorted(lengths)}")
        mix: np.ndarray | None = None
        for direction, signal in sources:
            encoded = self.encode(direction, signal)
            mix = encoded if mix is None else mix + encoded
        assert mix is not None
        diag = Diagnostics()
        diag.peak = float(np.max(np.abs(mix)))
        diag.clipped_samples = int(np.count_nonzero(np.abs(mix) > 1.0))
        if diag.clipped_samples:
            diag.warn(
                f"{diag.clipped_samples} samples exceed full scale after mixing"
            )
        return AudioFrame(mix, self.sample_rate), diag
