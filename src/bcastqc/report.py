"""End-to-end statistics: sync error, recovery rate and latency quantiles.

All statistics are deterministic functions of the processed material, the
timeline and the decode statistics - never of wall-clock timing alone (latency
is measured, but its *quantiles* are reported over recorded values, and the
acceptance run uses enough blocks for the quantiles to be stable).

Reported fields (the names are part of the acceptance contract):

``sync_error_ms_p95``
    p95 of the absolute per-frame A/V sync error estimate, in milliseconds.
``packet_recovery_rate``
    Recovered losses / (recovered + unrecoverable losses).  Concealed frames
    never count as recoveries.
``latency_ms_p95``
    p95 of per-block producer->consumer latency in milliseconds.
``peak_queue_frames``
    Maximum queue depth observed, in *frames* (blocks x block size).
``frame_hash_mismatches``
    Number of output frames whose hash differs from the recorded manifest.

Degenerate inputs (empty tracks, zero frames, timestamp jumps, total loss)
produce explicit statistics and a documented non-zero exit code rather than a
crash or a silent zero.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Iterable, Sequence

import numpy as np

from . import EXIT_INPUT_INVALID, EXIT_MEDIA_UNRECOVERABLE, EXIT_OK
from .decode import DecodeStats

__all__ = [
    "quantile",
    "LatencyRecorder",
    "Thresholds",
    "evaluate_thresholds",
]


def quantile(values: Sequence[float], q: float) -> float:
    """Linear-interpolated quantile with deterministic tie handling.

    ``q`` is in ``[0, 1]``.  Empty input returns 0.0.  This matches
    ``numpy.percentile(method="linear")`` without importing numpy here.
    """
    if not values:
        return 0.0
    if not 0.0 <= q <= 1.0:
        raise ValueError("q must be within [0, 1]")
    ordered = sorted(float(v) for v in values)
    if len(ordered) == 1:
        return ordered[0]
    position = q * (len(ordered) - 1)
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    weight = position - lower
    return ordered[lower] * (1.0 - weight) + ordered[upper] * weight


@dataclass
class LatencyRecorder:
    """Collects per-block latency samples (milliseconds)."""

    samples: list[float] = field(default_factory=list)

    def add(self, latency_ms: float) -> None:
        if latency_ms < 0:
            raise ValueError("latency cannot be negative")
        self.samples.append(float(latency_ms))

    @property
    def p50(self) -> float:
        return quantile(self.samples, 0.50)

    @property
    def p95(self) -> float:
        return quantile(self.samples, 0.95)

    @property
    def max(self) -> float:
        return max(self.samples) if self.samples else 0.0

    def to_json(self) -> dict:
        return {
            "samples": len(self.samples),
            "p50_ms": round(self.p50, 3),
            "p95_ms": round(self.p95, 3),
            "max_ms": round(self.max, 3),
        }
