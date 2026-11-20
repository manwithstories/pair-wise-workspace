"""ITU-R BS.1770-4 loudness measurement and static-gain policy.

K-weighting is two cascaded biquads (high-shelf pre-filter, RLB high-pass).
Integrated loudness uses 400 ms blocks with 75 % overlap plus the absolute
(-70 LUFS) and relative (-10 LU) gates.  The filter is evaluated as an FFT
convolution with a truncated impulse response so a ten-minute pass is close
to real time; the result is linear and repeatable.

The gain policy is a single static multiply.  If the loudness gain would push
the sample peak above the ceiling, the gain is reduced to the ceiling instead
of compressing — deterministic and free of codec artifacts.
"""

from __future__ import annotations

import numpy as np

from .audio_kweighting import _KWeighting
from .audio_model import AudioBuffer, LoudnessStats, _finite
from .errors import AudioError

BLOCK_SECONDS = 0.400
HOP_SECONDS = 0.100
ABSOLUTE_GATE_LUFS = -70.0
RELATIVE_GATE_LU = -10.0
CHUNK_SAMPLES = 4096
DEFAULT_CEILING_DBFS = -1.0
SILENCE_LUFS = float("-inf")










class LoudnessMeter:
    """Streaming BS.1770 integrated loudness and sample peak.

    Feed resampled planar blocks in order; call :meth:`result` at the end.
    Memory is bounded by the 400 ms window, not by the clip length.
    """

    def __init__(self, sample_rate: int, channels: int) -> None:
        self.sample_rate = sample_rate
        self.channels = channels
        self._filter = _KWeighting(sample_rate, channels)
        self._window = max(1, round(BLOCK_SECONDS * sample_rate))
        self._hop = max(1, round(HOP_SECONDS * sample_rate))
        self._ring = np.zeros((channels, self._window), dtype=np.float64)
        self._filled = 0          # samples currently in the ring
        self._since_hop = 0       # samples since the last block emitted
        self._total = 0           # total samples seen
        self.block_powers: list[float] = []
        self._peak = 0.0

    def feed(self, block: np.ndarray) -> None:
        if block.size == 0:
            return
        if block.shape[0] != self.channels:
            raise AudioError(
                "loudness meter channel mismatch",
                expected=self.channels,
                got=int(block.shape[0]),
            )
        self._peak = max(self._peak, float(np.max(np.abs(block))))
        weighted = self._filter.process(block)
        # Slide through the block one hop-sized piece at a time so block
        # boundaries are identical regardless of how the caller chunks.
        offset = 0
        n = weighted.shape[1]
        while offset < n:
            take = min(self._hop - self._since_hop, n - offset)
            self._push(weighted[:, offset : offset + take])
            offset += take
            self._since_hop += take
            if self._since_hop >= self._hop:
                self._since_hop = 0
                if self._total >= self._window:
                    self._emit_block()

    def _push(self, piece: np.ndarray) -> None:
        take = piece.shape[1]
        if take >= self._window:
            self._ring[:] = piece[:, -self._window :]
            self._filled = self._window
        else:
            self._ring[:, : self._window - take] = self._ring[:, take:]
            self._ring[:, self._window - take :] = piece
            self._filled = min(self._window, self._filled + take)
        self._total += take

    def _emit_block(self) -> None:
        means = np.mean(self._ring * self._ring, axis=1)
        power = float(np.sum(means))  # channel weights are 1.0 for L/R/mono
        self.block_powers.append(power)

    def result(self) -> tuple[float, float, int, str]:
        """Return ``(integrated_lufs, peak_dbfs, gated_blocks, measurement)``."""
        peak = self._peak
        peak_dbfs = 20.0 * np.log10(peak) if peak > 0 else SILENCE_LUFS
        if self.block_powers:
            loudness, gated, measurement = integrated_loudness(self.block_powers)
        elif self._total > 0:
            # Shorter than one 400 ms block: measure the whole clip.  This is
            # the documented policy for very short segments; it is *not* the
            # gated value and the report says so via ``measurement``.
            weighted_power = self._whole_clip_power()
            loudness = (
                -0.691 + 10.0 * np.log10(weighted_power)
                if weighted_power > 0
                else SILENCE_LUFS
            )
            gated = 0
            measurement = "whole_clip"
        else:
            loudness, gated, measurement = SILENCE_LUFS, 0, "empty"
        return float(loudness), float(peak_dbfs), gated, measurement

    def _whole_clip_power(self) -> float:
        # The ring holds the last <=400 ms; for a short clip that is the clip.
        filled = self._filled
        if filled == 0:
            return 0.0
        window = self._ring[:, self._window - filled :]
        return float(np.sum(np.mean(window * window, axis=1)))


def integrated_loudness(block_powers: list[float]) -> tuple[float, int, str]:
    """Gated integrated loudness in LUFS from per-block mean powers.

    Returns ``(lufs, gated_block_count, measurement)``.  The gates are the
    BS.1770 absolute gate (-70 LUFS) and relative gate (-10 LU below the
    absolute-gated mean).
    """
    if not block_powers:
        return SILENCE_LUFS, 0, "empty"
    powers = np.asarray(block_powers, dtype=np.float64)
    loud = np.where(
        powers > 0,
        -0.691 + 10.0 * np.log10(np.maximum(powers, 1e-20)),
        SILENCE_LUFS,
    )
    absolute = powers[loud > ABSOLUTE_GATE_LUFS]
    if absolute.size == 0:
        return SILENCE_LUFS, 0, "gated"
    mean_abs = float(np.mean(absolute))
    relative_gate = -0.691 + 10.0 * np.log10(mean_abs) + RELATIVE_GATE_LU
    gated = absolute[loud[loud > ABSOLUTE_GATE_LUFS] > relative_gate]
    if gated.size == 0:
        return SILENCE_LUFS, 0, "gated"
    lufs = -0.691 + 10.0 * np.log10(float(np.mean(gated)))
    return float(lufs), int(gated.size), "gated"


def measure_loudness(buffer: AudioBuffer) -> LoudnessStats:
    """Convenience wrapper for in-memory audio (used by tests)."""
    meter = LoudnessMeter(buffer.sample_rate, buffer.samples.shape[0])
    for start in range(0, buffer.frames, CHUNK_SAMPLES):
        meter.feed(buffer.samples[:, start : start + CHUNK_SAMPLES])
    lufs, peak, gated, measurement = meter.result()
    stats = _stats_for(lufs, peak, gated, measurement, target_lufs=None, ceiling_dbfs=None)
    return stats


def gain_for_target(
    integrated_lufs: float,
    peak_dbfs: float,
    target_lufs: float,
    ceiling_dbfs: float,
) -> tuple[float, bool, bool]:
    """Return ``(gain_db, peak_limited, silent)`` for a static gain.

    The loudness gain is capped so the post-gain sample peak does not exceed
    ``ceiling_dbfs``.  Capping (instead of compressing) keeps the transform a
    single multiply: deterministic and free of codec-like artifacts.
    """
    if integrated_lufs == SILENCE_LUFS or peak_dbfs == SILENCE_LUFS:
        return 0.0, False, True
    gain = target_lufs - integrated_lufs
    limited = False
    headroom = ceiling_dbfs - peak_dbfs
    if gain > headroom:
        gain = headroom
        limited = True
    return gain, limited, False


def _stats_for(
    lufs: float,
    peak_dbfs: float,
    gated: int,
    measurement: str,
    *,
    target_lufs: float | None,
    ceiling_dbfs: float | None,
) -> LoudnessStats:
    if target_lufs is None:
        return LoudnessStats(
            integrated_lufs=lufs,
            peak_dbfs=peak_dbfs,
            gain_db=0.0,
            peak_limited=False,
            silent=lufs == SILENCE_LUFS,
            gated_blocks=gated,
            measurement=measurement,
        )
    gain, limited, silent = gain_for_target(
        lufs, peak_dbfs, target_lufs, ceiling_dbfs or DEFAULT_CEILING_DBFS
    )
    return LoudnessStats(
        integrated_lufs=lufs,
        peak_dbfs=peak_dbfs,
        gain_db=gain,
        peak_limited=limited,
        silent=silent,
        gated_blocks=gated,
        measurement=measurement,
    )

