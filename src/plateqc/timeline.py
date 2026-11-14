"""Unified FrameTimeline: the single product of reorder + repair.

metrics, report and remux all read this structure; none of them recomputes
timestamps on its own.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

import numpy as np

PICT_I = 0
PICT_P = 1
PICT_B = 2
PICT_OTHER = 3

PICT_CODES = {"I": PICT_I, "P": PICT_P, "B": PICT_B}


def pict_code(name: str) -> int:
    return PICT_CODES.get(name, PICT_OTHER)


@dataclass
class FrameTimeline:
    """Display-order frame table with original and repaired timestamps.

    Arrays are indexed by display row. ``decode_index[row]`` maps a display
    row back to its decode-order position (a permutation of 0..n-1).
    Timestamps are in ``time_base`` units; NaN marks a missing original.
    """

    time_base: Fraction
    frame_rate: Fraction
    interval: int  # nominal pts step in time_base units
    decode_index: np.ndarray  # int64 [n]
    pts_original: np.ndarray  # float64 [n], NaN if missing
    dts_original: np.ndarray  # float64 [n], NaN if missing
    pts_repaired: np.ndarray  # int64 [n], strictly monotonic
    pict_type: np.ndarray  # uint8 [n], PICT_* codes
    repair_mask: np.ndarray  # bool [n], True where pts was rebuilt
    warnings: list = field(default_factory=list)
    frames_decoded: int = 0

    @property
    def count(self) -> int:
        return int(self.pts_repaired.shape[0])

    @property
    def duration_units(self) -> int:
        if self.count == 0:
            return 0
        return int(self.pts_repaired[-1] - self.pts_repaired[0] + self.interval)

    def to_ms(self, units: float) -> float:
        return float(units) * float(self.time_base) * 1000.0

    def decode_to_row(self) -> np.ndarray:
        """Inverse permutation: decode-order position -> display row."""
        inv = np.empty_like(self.decode_index)
        inv[self.decode_index] = np.arange(self.count, dtype=np.int64)
        return inv
