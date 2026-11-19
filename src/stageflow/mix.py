"""Timeline alignment, resampling and multi-track PCM mixing.

All mixing happens in delivery-rate sample space, one output block at a
time.  For each output block the mixer finds every track segment covering
the block's tick window, converts the window into track-file samples with
exact rational arithmetic (clock.py), reads via TrackReader, resamples by
deterministic linear interpolation, maps channel layouts, and sums.

Resampling positions are float64 derived from exact Fractions; the maximum
alignment error introduced by tick->sample rounding is tracked in
max_align_error_ticks so the manifest can report manifest_alignment_ms.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

import numpy as np

from .clock import MASTER_RATE
from .errors import MediaError
from .ingest import Block, TrackReader
from .timeline import Delivery, Timeline, Track


@dataclass
class MixStats:
    """Running quality statistics over the mixed output."""

    frames: int = 0
    peak: float = 0.0
    sum_sq: float = 0.0
    clipped: int = 0
    max_align_error_ticks: Fraction = Fraction(0)

    def update(self, data: np.ndarray) -> None:
        if data.size == 0:
            return
        self.frames += data.shape[0]
        peak = float(np.max(np.abs(data)))
        self.peak = max(self.peak, peak)
        self.sum_sq += float(np.sum(data.astype(np.float64) ** 2))
        self.clipped += int(np.count_nonzero(np.abs(data) >= 1.0))

    @property
    def rms_dbfs(self) -> float:
        if self.frames == 0 or self.sum_sq <= 0.0:
            return -120.0
        mean = self.sum_sq / max(1, self.frames)
        return float(10.0 * np.log10(mean))

    @property
    def clipping_pct(self) -> float:
        if self.frames == 0:
            return 0.0
        return 100.0 * self.clipped / self.frames


def map_channels(data: np.ndarray, src_ch: int, dst_ch: int, track: str) -> np.ndarray:
    """Deterministic channel-layout mapping; unsupported layouts raise."""
    if src_ch == dst_ch:
        return data
    if src_ch == 1:
        return np.repeat(data, dst_ch, axis=1)
    if dst_ch == 1:
        return data.mean(axis=1, keepdims=True).astype(np.float32)
    if src_ch == 2 and dst_ch > 2:  # stereo -> N: L,R then silence
        out = np.zeros((data.shape[0], dst_ch), np.float32)
        out[:, :2] = data
        return out
    if dst_ch == 2 and src_ch > 2:  # N -> stereo: keep first two
        return data[:, :2].copy()
    raise MediaError(
        "LAYOUT_UNSUPPORTED",
        f"cannot map {src_ch}ch to {dst_ch}ch",
        stage="mix", track=track,
    )


def resample_linear(
    data: np.ndarray, src_rate: int, dst_rate: int, start_pos: Fraction
) -> np.ndarray:
    """Resample one block by linear interpolation.

    start_pos is the exact fractional source position of output sample 0.
    Positions are computed as float64 from that exact origin, so repeated
    runs on the same inputs give bit-identical output.
    """
    if src_rate == dst_rate and start_pos.denominator == 1:
        off = int(start_pos)
        return data[off:] if off else data
    n_out = data.shape[0]
    step = Fraction(src_rate, dst_rate)
    pos0 = float(start_pos)
    stepf = float(step)
    pos = pos0 + np.arange(n_out, dtype=np.float64) * stepf
    i0 = np.floor(pos).astype(np.int64)
    frac = (pos - i0).astype(np.float32)
    i0 = np.clip(i0, 0, data.shape[0] - 1)
    i1 = np.clip(i0 + 1, 0, data.shape[0] - 1)
    return data[i0] * (1.0 - frac)[:, None] + data[i1] * frac[:, None]


@dataclass
class Mixer:
    timeline: Timeline
    readers: dict[str, TrackReader]
    stats: MixStats = field(default_factory=MixStats)

    @property
    def delivery(self) -> Delivery:
        return self.timeline.delivery

    def mix_block(self, block_start_tick: int, block_frames: int) -> np.ndarray:
        """Mix one delivery-rate block covering [block_start_tick, ...)."""
        d = self.delivery
        out = np.zeros((block_frames, d.channels), np.float32)
        block_end_tick = block_start_tick + Fraction(block_frames * MASTER_RATE, d.sample_rate)
        for tid in d.mix_order:
            track = self.timeline.track_map[tid]
            reader = self.readers[tid]
            for seg in track.segments:
                lo = max(seg.start_tick, block_start_tick)
                hi = min(seg.end_tick, block_end_tick)
                if hi <= lo:
                    continue
                self._mix_segment(track, reader, lo, hi, block_start_tick, out)
        self.stats.update(out)
        return out

    def _mix_segment(
        self,
        track: Track,
        reader: TrackReader,
        lo_tick: int,
        hi_tick: Fraction | int,
        block_start_tick: int,
        out: np.ndarray,
    ) -> None:
        d = self.delivery
        # Output-sample window inside this block.
        o0 = Fraction((lo_tick - block_start_tick) * d.sample_rate, MASTER_RATE)
        o1 = Fraction((hi_tick - block_start_tick) * d.sample_rate, MASTER_RATE)
        out_start = int(o0)  # segment boundaries are aligned by construction in tests;
        out_end = int(o1)  # int() truncates toward zero; positions are exact or recorded.
        for frac, name in ((o0 - int(o0), "start"), (o1 - int(o1), "end")):
            if frac:
                self._record_align(track.id, Fraction(frac) * Fraction(MASTER_RATE, d.sample_rate))
        n_out = out_end - out_start
        if n_out <= 0:
            return
        # Source window in track-file samples (exact rational positions).
        # Timeline validation guarantees lo_tick >= track.offset_ticks.
        src_lo = Fraction((lo_tick - track.offset_ticks) * reader.samplerate, MASTER_RATE)
        src_hi = Fraction((hi_tick - track.offset_ticks) * reader.samplerate, MASTER_RATE)
        read_start = int(src_lo)  # floor; src_lo >= 0
        # One extra frame of slack for the interpolation tap; the reader
        # zero-pads up to SLACK_FRAMES past EOF and reports anything more.
        read_end = int(src_hi) + 2
        read_frames = max(0, read_end - read_start)
        block: Block = reader.read_range(read_start, read_frames)
        data = map_channels(block.data, reader.channels, d.channels, track.id)
        start_pos = src_lo - read_start
        resampled = resample_linear(data, reader.samplerate, d.sample_rate, start_pos)
        take = min(n_out, resampled.shape[0])
        out[out_start : out_start + take] += resampled[:take]
        err = abs(Fraction((lo_tick - track.offset_ticks) * reader.samplerate, MASTER_RATE) - src_lo)
        if err:
            self._record_align(track.id, err)

    def _record_align(self, track: str, err_ticks: Fraction) -> None:
        if abs(err_ticks) > abs(self.stats.max_align_error_ticks):
            self.stats.max_align_error_ticks = abs(err_ticks)
