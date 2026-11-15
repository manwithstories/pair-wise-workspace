"""Rational polyphase FIR design used by the streaming resampler."""

from __future__ import annotations

from dataclasses import dataclass
from functools import lru_cache
from hashlib import sha256
import math

import numpy as np


MAX_PHASES = 4096
MAX_RATIO = 256
MAX_TAPS = 4096
QUALITY_TAPS = {"standard": 2047, "high": 4095}


class KernelLimitError(ValueError):
    """Raised when a rational ratio or FIR length exceeds the hard limits."""

    exit_code = 4


@dataclass(frozen=True)
class ResamplingKernel:
    fs_in: int
    fs_out: int
    L: int
    M: int
    taps_per_phase: int
    cutoff: float
    table: np.ndarray
    offsets: np.ndarray
    fingerprint: str

    @property
    def total_taps(self) -> int:
        return int(self.table.size)

    @property
    def half_span(self) -> int:
        return int(self.offsets[-1])


def _canonical(fs_in: int, fs_out: int, taps: int) -> str:
    ratio = math.gcd(fs_in, fs_out)
    payload = (
        f"v1|in={fs_in}|out={fs_out}|L={fs_out // ratio}|M={fs_in // ratio}|"
        f"budget={taps}|cutoff=0.92|window=kaiser8.6|dc=phase"
    )
    return sha256(payload.encode("ascii")).hexdigest()[:24]


def _design(fs_in: int, fs_out: int, taps: int) -> ResamplingKernel:
    if fs_in <= 0 or fs_out <= 0:
        raise KernelLimitError("sample rates must be positive")
    divisor = math.gcd(fs_in, fs_out)
    L, M = fs_out // divisor, fs_in // divisor
    if L > MAX_PHASES or M > MAX_PHASES or L > MAX_RATIO * M:
        raise KernelLimitError(f"reduced ratio L/M={L}/{M} exceeds {MAX_RATIO}")
    if taps > MAX_TAPS:
        raise KernelLimitError(f"requested FIR taps {taps} exceed {MAX_TAPS}")

    per_phase = max(3, taps // L)
    if per_phase % 2 == 0:
        per_phase -= 1
    if per_phase > MAX_TAPS or per_phase * L > MAX_TAPS:
        raise KernelLimitError(f"FIR taps {per_phase * L} exceed {MAX_TAPS}")

    half = (per_phase - 1) // 2
    offsets = np.arange(-half, half + 1, dtype=np.int64)
    # The kernel lives in input-sample time.  This is the given output-domain
    # cutoff after converting to input-domain normalized frequency.
    cutoff = 0.92 * min(1.0, L / M)
    positions = offsets.astype(np.float64)
    window = np.kaiser(per_phase, 8.6)
    table = np.empty((L, per_phase), dtype=np.float64)
    for phase in range(L):
        frac = positions + phase / L
        values = cutoff * np.sinc(cutoff * frac) * window
        total = float(values.sum())
        if not np.isfinite(total) or abs(total) < 1e-15:
            raise KernelLimitError("degenerate FIR kernel")
        table[phase] = values / total

    fingerprint = _canonical(fs_in, fs_out, taps)
    table.setflags(write=False)
    offsets.setflags(write=False)
    return ResamplingKernel(
        fs_in=fs_in,
        fs_out=fs_out,
        L=L,
        M=M,
        taps_per_phase=per_phase,
        cutoff=0.92 * min(1.0, M / L),
        table=table,
        offsets=offsets,
        fingerprint=fingerprint,
    )


@lru_cache(maxsize=64)
def design_kernel(
    fs_in: int,
    fs_out: int,
    quality: str = "high",
    taps: int | None = None,
) -> ResamplingKernel:
    """Return a cached kernel; equal parameters return the same object."""

    if quality not in QUALITY_TAPS:
        raise ValueError(f"unknown quality {quality!r}")
    budget = QUALITY_TAPS[quality] if taps is None else int(taps)
    if budget <= 0:
        raise KernelLimitError("FIR tap budget must be positive")
    return _design(int(fs_in), int(fs_out), budget)


get_kernel = design_kernel


def clear_kernel_cache() -> None:
    design_kernel.cache_clear()
