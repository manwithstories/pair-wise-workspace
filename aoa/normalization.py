"""Conversion between first-order conventions (channel order, sign, scale).

Conversions are exact permutation+scale matrices — never implicit guesses.
Unknown, missing, or duplicated channel labels are rejected loudly.
"""
from __future__ import annotations

from typing import Sequence

import numpy as np

from .harmonics import channel_labels, channel_scales
from .types import NormMode


class NormalizationError(ValueError):
    """Raised for unknown, missing, or duplicated channel labels."""


def conversion_matrix(src: NormMode, dst: NormMode) -> np.ndarray:
    """Exact 4x4 matrix mapping B-format frames from `src` to `dst`.

    A source channel with label L and scale s_src carries s_src * f where f
    is the underlying direction cosine; the destination channel for the same
    L therefore takes value * (s_dst / s_src).
    """
    src_labels = channel_labels(src)
    dst_labels = channel_labels(dst)
    src_scales = channel_scales(src)
    dst_scales = channel_scales(dst)
    mat = np.zeros((4, 4), dtype=np.float64)
    for i, label in enumerate(dst_labels):
        matches = [j for j, s in enumerate(src_labels) if s == label]
        if len(matches) != 1:
            raise NormalizationError(
                f"channel {label!r} missing or duplicated in {src.value}"
            )
        j = matches[0]
        mat[i, j] = dst_scales[i] / src_scales[j]
    return mat


def validate_channel_labels(labels: Sequence[str], mode: NormMode) -> None:
    """Reject unknown, missing, or duplicated labels for `mode`."""
    labels = tuple(labels)
    expected = channel_labels(mode)
    for label in labels:
        if label not in expected:
            raise NormalizationError(
                f"unknown channel label {label!r} for {mode.value}"
            )
    if len(set(labels)) != len(labels):
        raise NormalizationError(f"duplicated channel labels in {labels}")
    if set(labels) != set(expected):
        missing = sorted(set(expected) - set(labels))
        raise NormalizationError(f"missing channels {missing} for {mode.value}")


def convert(frames: np.ndarray, src: NormMode, dst: NormMode) -> np.ndarray:
    """Convert a (frames, 4) B-format block between conventions."""
    frames = np.asarray(frames, dtype=np.float64)
    if frames.ndim != 2 or frames.shape[1] != 4:
        raise NormalizationError(
            f"expected (frames, 4) B-format, got shape {frames.shape}"
        )
    if src is dst:
        return frames.copy()
    return frames @ conversion_matrix(src, dst).T


def roundtrip_error(frames: np.ndarray, src: NormMode, dst: NormMode) -> float:
    """Relative max-abs error of src -> dst -> src (must stay near 1e-16)."""
    frames = np.asarray(frames, dtype=np.float64)
    if frames.size == 0:
        return 0.0
    back = convert(convert(frames, src, dst), dst, src)
    ref = float(np.max(np.abs(frames)))
    scale = ref if ref > 0.0 else 1.0
    return float(np.max(np.abs(back - frames)) / scale)
