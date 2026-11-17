"""First-order real spherical-harmonic bases.

Channel orders:
- ACN conventions (acn-sn3d, acn-n3d): (W, Y, Z, X)
- FuMa: (W, X, Y, Z)

All bases are real-valued. Direction cosines use x = front, y = left,
z = up (see types.Direction.unit_vector).
"""
from __future__ import annotations

import math
from typing import Sequence

import numpy as np

from .types import Direction, NormMode

SQRT3 = math.sqrt(3)
INV_SQRT2 = 1.0 / math.sqrt(2.0)

_CHANNELS = {
    NormMode.ACN_SN3D: ("W", "Y", "Z", "X"),
    NormMode.ACN_N3D: ("W", "Y", "Z", "X"),
    NormMode.FUMA: ("W", "X", "Y", "Z"),
}

# Per-channel scale applied to the raw (1, y, z, x) / (1, x, y, z) cosines.
_SCALES = {
    NormMode.ACN_SN3D: (1.0, 1.0, 1.0, 1.0),
    NormMode.ACN_N3D: (1.0, SQRT3, SQRT3, SQRT3),
    NormMode.FUMA: (INV_SQRT2, 1.0, 1.0, 1.0),
}


def channel_labels(mode: NormMode) -> tuple[str, ...]:
    """Channel labels in storage order for `mode`."""
    return _CHANNELS[mode]


def channel_scales(mode: NormMode) -> np.ndarray:
    """Per-channel normalization scales in storage order."""
    return np.array(_SCALES[mode], dtype=np.float64)


def basis_vector(direction: Direction, mode: NormMode) -> np.ndarray:
    """Encode `direction` into the 4 first-order components of `mode`."""
    x, y, z = direction.unit_vector()
    if mode is NormMode.FUMA:
        raw = (1.0, x, y, z)  # W, X, Y, Z
    else:
        raw = (1.0, y, z, x)  # W, Y, Z, X (ACN order)
    scales = _SCALES[mode]
    return np.array([s * v for s, v in zip(scales, raw)], dtype=np.float64)


def basis_matrix(directions: Sequence[Direction], mode: NormMode) -> np.ndarray:
    """Stack basis vectors row-wise: returns (len(directions), 4)."""
    if not directions:
        raise ValueError("at least one direction is required")
    return np.stack([basis_vector(d, mode) for d in directions])
