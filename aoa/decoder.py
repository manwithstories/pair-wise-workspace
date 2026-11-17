"""Loudspeaker decoder design from a layout of directions.

Strategies:
- least_squares:     D = pinv(E)
- regularized:       D = (E^T E + lam*I)^-1 E^T
- energy_preserving: regularized D rescaled so decoded energy matches
                     encoded energy over a horizontal probe set.

Singular layouts (collinear/duplicate directions, unobserved channels)
emit warnings and fall back to the regularized inverse.
"""
from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .harmonics import basis_matrix
from .types import ChannelLayout, Direction, NormMode

LEAST_SQUARES = "least_squares"
REGULARIZED = "regularized"
ENERGY_PRESERVING = "energy_preserving"
STRATEGIES = (LEAST_SQUARES, REGULARIZED, ENERGY_PRESERVING)

#: condition number above which verify treats the layout as a stability failure
COND_FAILURE = 1e12


@dataclass
class DecoderDesign:
    matrix: np.ndarray  # (speakers, 4)
    strategy: str
    condition_number: float
    warnings: list[str] = field(default_factory=list)


def _energy_scale(matrix: np.ndarray, mode: NormMode) -> float:
    """Scale so mean decoded energy equals mean encoded energy on probes."""
    probes = [Direction(a, 0.0) for a in np.linspace(-180.0, 180.0, 16, endpoint=False)]
    H = basis_matrix(probes, mode)  # (16, 4)
    decoded = H @ matrix.T  # (16, speakers)
    out = float(np.mean(np.sum(decoded**2, axis=1)))
    if out <= 0.0:
        return 1.0
    ref = float(np.mean(np.sum(H**2, axis=1)))
    return float(np.sqrt(ref / out))


def build_decoder(
    layout: ChannelLayout,
    mode: NormMode,
    strategy: str = ENERGY_PRESERVING,
    regularization: float = 1e-6,
) -> DecoderDesign:
    """Design the decode matrix for `layout` in convention `mode`."""
    if strategy not in STRATEGIES:
        raise ValueError(f"unknown strategy {strategy!r}; expected {STRATEGIES}")
    if not regularization > 0.0:
        raise ValueError("regularization must be positive")
    warnings: list[str] = []
    dirs = layout.directions
    if len(dirs) < 4:
        warnings.append(
            f"layout has only {len(dirs)} loudspeakers; first-order decode "
            "is underdetermined"
        )
    vecs = [d.unit_vector() for d in dirs]
    for i in range(len(dirs)):
        for j in range(i + 1, len(dirs)):
            if float(vecs[i] @ vecs[j]) > 1.0 - 1e-9:
                warnings.append(
                    f"duplicate loudspeaker direction at index {i} and {j}"
                )
    E = basis_matrix(dirs, mode)  # (speakers, 4)
    gram = E.T @ E
    eig = np.linalg.eigvalsh(gram)
    lam = float(regularization)
    cond = float((eig[-1] + lam) / (eig[0] + lam))
    singular = bool(eig[0] < 1e-9 * max(float(eig[-1]), 1.0))
    if singular:
        warnings.append(
            "layout does not observe all four channels (singular Gram "
            "matrix); regularized inverse enforced"
        )
    used = strategy
    if strategy == LEAST_SQUARES and singular:
        warnings.append("least-squares decoder unstable here; switched to regularized")
        used = REGULARIZED
    if used == LEAST_SQUARES:
        matrix = np.linalg.pinv(E).T  # (speakers, 4)
    else:
        matrix = np.linalg.solve(gram + lam * np.eye(4), E.T).T
    if strategy == ENERGY_PRESERVING:
        scale = _energy_scale(matrix, mode)
        if scale == 1.0 and not np.any(matrix):
            warnings.append("decoder energy is zero; energy scaling skipped")
        matrix = matrix * scale
    return DecoderDesign(matrix, used, cond, warnings)
