"""Master-bus peak soft limiting with clip counting and loudness stats.

The gain envelope uses a linear attack and linear release expressed as
min-plus prefix scans, so it is computed with NumPy and — crucially — produces
the identical sample sequence whether the timeline is fed as one buffer or as
256/1024-frame blocks. The only state carried between blocks is the two prefix
minima and the previous frame index.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .model import TrackStats

EPS = 1e-12
CLIP_THRESHOLD = 0.999


def peak_db(value: float) -> float:
    return -math.inf if value <= 0.0 else float(20.0 * math.log10(value))


@dataclass
class MeterSnapshot:
    frames: int = 0
    peak_db: float = -math.inf
    rms_db: float = -math.inf
    clip_count: int = 0
    max_gain_delta_db: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "frames": self.frames,
            "peak_dbfs": round(self.peak_db, 4),
            "rms_dbfs": round(self.rms_db, 4),
            "clip_count": self.clip_count,
            "max_gain_delta_db": round(self.max_gain_delta_db, 6),
        }


class Meter:
    """Running peak / RMS / clip counter; retains no sample history."""

    def __init__(self, clip_threshold: float = CLIP_THRESHOLD) -> None:
        self.clip_threshold = float(clip_threshold)
        self.frames = 0
        self.peak = 0.0
        self.sum_squares = 0.0
        self.clip_count = 0
        self.max_gain_delta_db = 0.0

    def add(self, block: np.ndarray, gain: np.ndarray | None = None) -> None:
        if block.size == 0:
            return
        magnitude = np.abs(block)
        self.frames += int(block.shape[0])
        self.peak = max(self.peak, float(magnitude.max()))
        self.sum_squares += float(np.sum(np.square(block, dtype=np.float64)))
        self.clip_count += int(np.count_nonzero(magnitude >= self.clip_threshold))
        if gain is not None:
            gains = np.asarray(gain, dtype=np.float64)
            if gains.size > 1:
                db = 20.0 * np.log10(np.maximum(gains, EPS))
                self.max_gain_delta_db = max(
                    self.max_gain_delta_db, float(np.max(np.abs(np.diff(db))))
                )

    @property
    def rms(self) -> float:
        return 0.0 if self.frames == 0 else math.sqrt(self.sum_squares / self.frames)

    def snapshot(self) -> MeterSnapshot:
        return MeterSnapshot(
            frames=self.frames,
            peak_db=peak_db(self.peak),
            rms_db=peak_db(self.rms),
            clip_count=self.clip_count,
            max_gain_delta_db=self.max_gain_delta_db,
        )


class Limiter:
    """Feed-forward peak limiter with linear attack/release and a soft backstop.

    ``process_block`` may be called with any block size; the returned samples
    are independent of how the stream was split.
    """

    def __init__(
        self,
        rate: int,
        threshold_db: float = -0.5,
        attack_ms: float = 2.0,
        release_ms: float = 80.0,
        knee: float = 0.98,
    ) -> None:
        if not -60.0 < threshold_db < 0.0:
            raise ValueError("threshold_db must be between -60 and 0 dBFS")
        self.rate = int(rate)
        self.threshold_db = float(threshold_db)
        self.threshold = float(10.0 ** (threshold_db / 20.0))
        self.knee = float(knee)
        attack_samples = max(1.0, attack_ms * 0.001 * self.rate)
        release_samples = max(1.0, release_ms * 0.001 * self.rate)
        # Linear gain-domain slopes (gain units per sample).
        self._attack_slope = 1.0 / attack_samples
        self._release_slope = 1.0 / release_samples
        self.reset()

    def reset(self) -> None:
        self.input_meter = Meter()
        self.output_meter = Meter()
        self._frame = 0
        self.min_envelope = 1.0
        self._attack_state = math.inf  # min over k of (g[k] - slope_a * k)
        self._release_state = math.inf  # min over k of (f[k] - slope_r * k)
        self.max_reduction_db = 0.0

    # -- core -------------------------------------------------------------
    def _envelope(self, desired: np.ndarray) -> np.ndarray:
        """Attack then release, both as min-plus prefix scans."""
        n = desired.size
        k = np.arange(self._frame, self._frame + n, dtype=np.float64)
        a = self._attack_slope
        r = self._release_slope
        # f[n] = min_k (g[k] + a * (n - k))  ==  a*n + prefix_min(g[k] - a*k)
        raw = desired - a * k
        attack_scan = np.minimum.accumulate(np.concatenate(([self._attack_state], raw)))[1:]
        attack = a * k + attack_scan
        # e[n] = min_k (f[k] + r * (n - k))  ==  r*n + prefix_min(f[k] - r*k)
        raw_r = attack - r * k
        release_scan = np.minimum.accumulate(np.concatenate(([self._release_state], raw_r)))[1:]
        envelope = r * k + release_scan
        self._attack_state = float(attack_scan[-1])
        self._release_state = float(release_scan[-1])
        self._frame += n
        return np.clip(envelope, 0.0, 1.0)

    def process_block(self, block: np.ndarray) -> np.ndarray:
        """Limit a stereo (or mono) float block and return the limited copy."""
        if block.size == 0:
            return block
        self.input_meter.add(block)
        frame_peak = np.max(np.abs(block), axis=1)
        desired = np.ones(frame_peak.size, dtype=np.float64)
        over = frame_peak > self.threshold
        if over.any():
            desired[over] = self.threshold / np.maximum(frame_peak[over], EPS)
        envelope = self._envelope(desired)
        self.min_envelope = min(self.min_envelope, float(envelope.min()))
        self.max_reduction_db = -peak_db(self.min_envelope)
        reduced = block * envelope[:, None]
        output = self._soft_clip(reduced)
        self.output_meter.add(output)
        return output

    def _soft_clip(self, block: np.ndarray) -> np.ndarray:
        """Soft knee above ``knee``, then a hard bounded backstop."""
        magnitude = np.abs(block)
        hot = magnitude > self.knee
        limited = block
        if hot.any():
            limited = block.copy()
            sign = np.sign(block[hot])
            span = max(1.0 - self.knee, EPS)
            over = magnitude[hot] - self.knee
            limited[hot] = sign * (self.knee + span * np.tanh(over / span))
        return np.clip(limited, -1.0, 1.0, out=limited)

    # -- reporting --------------------------------------------------------
    def stats(self) -> dict[str, Any]:
        before = self.input_meter.snapshot()
        after = self.output_meter.snapshot()
        return {
            "threshold_dbfs": round(self.threshold_db, 4),
            "peak_in_dbfs": round(before.peak_db, 4),
            "peak_out_dbfs": round(after.peak_db, 4),
            "rms_in_dbfs": round(before.rms_db, 4),
            "rms_out_dbfs": round(after.rms_db, 4),
            "clip_count": after.clip_count,
            "frames": after.frames,
            "max_reduction_db": round(self.max_reduction_db, 4),
        }


def track_stats(name: str, meter: Meter) -> TrackStats:
    snap = meter.snapshot()
    return TrackStats(
        name=name,
        frames=snap.frames,
        peak_in_dbfs=snap.peak_db,
        peak_out_dbfs=snap.peak_db,
        rms_dbfs=snap.rms_db,
        clip_count=snap.clip_count,
        max_gain_delta_db=snap.max_gain_delta_db,
    )
