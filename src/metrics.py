"""Numeric and timing metrics over render outputs and per-block records.

Pure functions only — they measure, they never modify signals. Every value
returned is a plain Python scalar/list so it serialises straight to JSON.
"""
from __future__ import annotations

import math

import numpy as np

from .config import (
    NumericConstraintError,
    PATH_MAX_ABS_ERROR_TOLERANCE,
    PathMismatchError,
)


def block_time_stats(records) -> dict:
    """min/max/mean per-block processing time in milliseconds."""
    times = [r.time_ms for r in records]
    if not times:
        return {"min_ms": 0.0, "max_ms": 0.0, "mean_ms": 0.0}
    return {
        "min_ms": float(min(times)),
        "max_ms": float(max(times)),
        "mean_ms": float(sum(times) / len(times)),
    }


def output_peak(output: np.ndarray) -> float:
    """Maximum absolute sample across every channel."""
    if output.size == 0:
        return 0.0
    return float(np.max(np.abs(output)))


def output_energy(output: np.ndarray) -> float:
    """Total signal energy: sum of squared samples over all channels."""
    if output.size == 0:
        return 0.0
    return float(np.sum(output.astype(np.float64) ** 2))


def output_length(output: np.ndarray) -> int:
    return int(output.shape[0])


def path_max_abs_error(offline: np.ndarray, streaming: np.ndarray) -> float:
    """Maximum absolute sample difference between the two render paths."""
    if offline.shape != streaming.shape:
        raise PathMismatchError(
            f"path shape mismatch: offline {offline.shape} vs "
            f"streaming {streaming.shape}"
        )
    if offline.size == 0:
        return 0.0
    return float(np.max(np.abs(offline - streaming)))


def scan_anomalies(records, output: np.ndarray) -> dict:
    """Detect NaN, Inf and over-unity peaks at block and aggregate level."""
    nan_blocks = [r.index for r in records if r.has_nan]
    inf_blocks = [r.index for r in records if r.has_inf]
    over_one_blocks = [r.index for r in records if r.peak_over_one]
    global_nan = bool(np.any(np.isnan(output))) if output.size else False
    global_inf = bool(np.any(np.isinf(output))) if output.size else False
    return {
        "nan_block_indexes": nan_blocks,
        "inf_block_indexes": inf_blocks,
        "peak_over_one_block_indexes": over_one_blocks,
        "global_nan": global_nan,
        "global_inf": global_inf,
        "block_peak_max": max((r.peak for r in records), default=0.0),
    }


def assert_healthy(output: np.ndarray) -> None:
    """Hard numeric gate: NaN/Inf cannot be written as a valid render."""
    if output.size == 0:
        return
    if np.any(np.isnan(output)):
        raise NumericConstraintError("output contains NaN samples")
    if np.any(np.isinf(output)):
        raise NumericConstraintError("output contains Inf samples")


def assert_path_agreement(offline: np.ndarray, streaming: np.ndarray,
                          tolerance: float = PATH_MAX_ABS_ERROR_TOLERANCE
                          ) -> float:
    """Raise unless the two paths agree within the shared tolerance."""
    err = path_max_abs_error(offline, streaming)
    if not math.isfinite(err) or err > tolerance:
        raise PathMismatchError(
            f"offline/streaming max abs error {err:.3e} exceeds "
            f"tolerance {tolerance:.0e}"
        )
    return err


def expected_convolution_length(input_samples: int, ir_length: int) -> int:
    """Canonical linear-convolution length N + M - 1."""
    return input_samples + ir_length - 1
