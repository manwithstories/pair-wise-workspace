"""ITU-R BS.1770-4 loudness metering: gating, RMS, peak and clipping.

K-weighting lives in :mod:`audio_tool.kweight`, true-peak interpolation in
:mod:`audio_tool.truepeak`.  This module owns the streaming measurement pass:
the 400 ms / 100 ms block grid, the -70 LUFS absolute gate and the relative
gate 10 LU below the ungated mean.
"""

from __future__ import annotations

import math
from collections import deque
from typing import Sequence

from .kweight import (
    Biquad,
    KWeighting,
    _highpass_coefficients,
    _shelf_coefficients,
)
from .model import ABSOLUTE_GATE, RELATIVE_GATE_OFFSET, LoudnessStats
from .truepeak import TruePeakMeter

LUFS_OFFSET = -0.691
GATE_BLOCK_SECONDS = 0.4
GATE_STEP_SECONDS = 0.1
SHORT_TERM_SECONDS = 3.0

#: Blocks per gate window (400 ms / 100 ms).
BLOCKS_PER_GATE = int(round(GATE_BLOCK_SECONDS / GATE_STEP_SECONDS))
SHORT_TERM_STEPS = int(round(SHORT_TERM_SECONDS / GATE_STEP_SECONDS))


class LoudnessMeter:
    """One streaming pass: RMS, peak, true peak, LUFS, gating, clipping.

    Feed every block that will also be written out.  Gated loudness needs the
    complete set of block energies, so :meth:`finish` performs the reduction.
    """

    def __init__(self, channels: int, sample_rate: int, weights: Sequence[float],
                 *, clip_threshold: float = 1.0):
        self.channels = channels
        self.sample_rate = sample_rate
        self.weights = tuple(weights)
        self.kweighting = KWeighting(channels, sample_rate)
        self.true_peak_meter = TruePeakMeter(channels)
        self._clip_threshold = clip_threshold
        self._step_frames = max(1, int(round(GATE_STEP_SECONDS * sample_rate)))
        self._block_frames = self._step_frames * BLOCKS_PER_GATE
        # Per-channel square sums for the step currently accumulating.
        self._step_squares = [0.0] * channels
        self._step_frames_done = 0
        # Rolling per-step energies: each entry is the weighted block energy.
        self._step_energies: deque[float] = deque(maxlen=BLOCKS_PER_GATE)
        self._block_energies: list[float] = []
        self._short_energies: deque[float] = deque(maxlen=SHORT_TERM_STEPS)
        self._momentary_max: float | None = None
        self._short_max: float | None = None
        self._sum_sq = 0.0
        self._weighted_sq = 0.0
        self._frames = 0
        self._peak = 0.0
        self._clipped = 0

    # -- feeding ------------------------------------------------------------

    def process(self, planes: Sequence[Sequence[float]], frames: int) -> None:
        if frames <= 0:
            return
        weighted = self.kweighting.process(planes, frames)
        self.true_peak_meter.process(planes, frames)
        self._frames += frames
        threshold = self._clip_threshold
        for chan in range(self.channels):
            total = 0.0
            clipped = 0
            peak = self._peak
            weight = self.weights[chan]
            for value, filtered in zip(planes[chan][:frames],
                                       weighted[chan][:frames]):
                total += value * value
                magnitude = value if value >= 0.0 else -value
                if magnitude > peak:
                    peak = magnitude
                if value >= threshold or value <= -threshold:
                    clipped += 1
                if weight:
                    self._weighted_sq += weight * filtered * filtered
            self._peak = peak
            self._clipped += clipped
            self._sum_sq += total
        self._accumulate(weighted, frames)

    def _accumulate(self, weighted: Sequence[Sequence[float]], frames: int) -> None:
        """Roll the weighted signal through the 100 ms / 400 ms gate grid."""

        position = 0
        while position < frames:
            take = min(self._step_frames - self._step_frames_done, frames - position)
            for chan in range(self.channels):
                weight = self.weights[chan]
                if weight:
                    segment = weighted[chan][position:position + take]
                    self._step_squares[chan] += sum(value * value for value in segment)
            self._step_frames_done += take
            position += take
            if self._step_frames_done >= self._step_frames:
                self._emit_step()
                self._step_frames_done = 0
                self._step_squares = [0.0] * self.channels

    def _emit_step(self) -> None:
        # Per-step mean square per channel, weighted and summed -> block energy
        # once four consecutive steps (400 ms) have accumulated.
        step_energy = math.fsum(
            self.weights[chan] * (self._step_squares[chan] / self._step_frames)
            for chan in range(self.channels))
        self._step_energies.append(step_energy)
        if len(self._step_energies) < BLOCKS_PER_GATE:
            return
        energy = math.fsum(self._step_energies) / BLOCKS_PER_GATE
        self._block_energies.append(energy)
        momentary = _lufs_from_energy(energy)
        if self._momentary_max is None or momentary > self._momentary_max:
            self._momentary_max = momentary
        self._short_energies.append(energy)
        short = _lufs_from_energy(math.fsum(self._short_energies)
                                  / len(self._short_energies))
        if self._short_max is None or short > self._short_max:
            self._short_max = short

    # -- results ------------------------------------------------------------

    def finish(self) -> LoudnessStats:
        if self._step_frames_done > 0:
            self._flush_partial_step()
        self.true_peak_meter.flush()
        stats = LoudnessStats(
            frames=self._frames,
            peak=self._peak,
            true_peak=max(self.true_peak_meter.peaks) if self.channels else 0.0,
            rms=math.sqrt(self._sum_sq / (self._frames * self.channels))
            if self._frames and self.channels else 0.0,
            clipped_samples=self._clipped,
            digital_silence=self._sum_sq <= 0.0,
            momentary_max=self._momentary_max,
            short_term_max=self._short_max,
        )
        stats.lufs = self._gated_lufs()
        return stats

    def _flush_partial_step(self) -> None:
        """Count a short final tail as one step so short files still measure.

        The already-emitted steps are kept as-is; the tail only needs to be
        folded in as an extra 100 ms step through the normal block path, which
        means re-running :meth:`_emit_step`'s reduction for it.
        """

        frames = self._step_frames_done
        if frames <= 0:
            return
        step_energy = math.fsum(
            self.weights[chan] * (self._step_squares[chan] / frames)
            for chan in range(self.channels))
        self._step_energies.append(step_energy)
        if len(self._step_energies) >= BLOCKS_PER_GATE:
            energy = math.fsum(self._step_energies) / BLOCKS_PER_GATE
            self._block_energies.append(energy)
            momentary = _lufs_from_energy(energy)
            if self._momentary_max is None or momentary > self._momentary_max:
                self._momentary_max = momentary
        self._step_frames_done = 0

    def _gated_lufs(self) -> float | None:
        energies = self._block_energies
        if not energies:
            # Material shorter than one 400 ms gating block: BS.1770 has no
            # block to gate, so fall back to the whole-file K-weighted mean
            # square (the standard's spirit, and the only finite answer).
            if self._weighted_sq <= 0.0 or self._frames <= 0:
                return None
            energy = self._weighted_sq / self._frames
            if energy <= ABSOLUTE_GATE:
                return None
            return _lufs_from_energy(energy)
        gated = [e for e in energies if e > ABSOLUTE_GATE]
        if not gated:
            return None
        relative = (math.fsum(gated) / len(gated)) * 10.0 ** (RELATIVE_GATE_OFFSET / 10.0)
        threshold = max(relative, ABSOLUTE_GATE)
        final = [e for e in gated if e >= threshold]
        if not final:
            final = gated
        return _lufs_from_energy(math.fsum(final) / len(final))


def _lufs_from_energy(energy: float) -> float:
    if energy <= 0.0:
        return -240.0
    return LUFS_OFFSET + 10.0 * math.log10(energy)


def lufs_to_linear(lufs: float) -> float:
    """Inverse of :func:`_lufs_from_energy` (useful for tests/gain maths)."""

    return 10.0 ** ((lufs - LUFS_OFFSET) / 10.0)


def describe_silence() -> str:
    return ("digital silence: no energy above the -70 LUFS absolute gate; "
            "loudness is undefined")
