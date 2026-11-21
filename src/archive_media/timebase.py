"""Unified media timebase: wraparound unwrap, drift estimation, monotonicity.

Every stream is corrected onto one shared timeline measured in seconds.

Unwrapping and drift are handled in the *interval domain*.  Ordinary
least-squares regression collapses here because PTS is exactly linear in
the reference, making its slope numerically unstable, so instead:

* The expected tick interval is the reference period expressed in ticks.
* A backwards jump larger than a fraction of the wrap period increments the
  wrap counter (modulo counter wraparound); a backwards step that survives
  unwrapping is a genuine non-monotonic discontinuity whose first position
  is recorded.
* Drift is the median ratio of observed-to-expected intervals over a
  calibration window (bounded memory, robust to a single VFR gap).
* Corrected time is measured from each stream's first timestamp, then a
  shared origin aligns audio and video.

The model is explicitly two-pass: :meth:`SharedTimeline.calibrate` learns
unwrap/drift state without monotonicity bookkeeping, the model freezes, and
:meth:`SharedTimeline.correct` then maps the same timestamps again.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from statistics import median

from .errors import E_NON_MONOTONIC, RecoverableMediaError

# A backwards jump must exceed this fraction of the wrap period to count as
# wraparound rather than a genuine discontinuity.
WRAP_JUMP_FRACTION = 0.25
# Calibration window (intervals per track) after which drift freezes.
CALIBRATION_INTERVALS = 16
# Backwards excursion (seconds) tolerated before flagging non-monotonicity.
MONOTONIC_TOLERANCE_S = 0.002


@dataclass
class TrackTimeline:
    """Per-stream unwrap, drift and offset state."""

    name: str
    time_base: float
    wrap_period_ticks: int | None
    ref_period_s: float
    wrap_count: int = 0
    last_raw: int | None = None
    first_unwrapped: int | None = None
    last_unwrapped: int | None = None
    last_corrected: float | None = None
    first_anomaly_s: float | None = None
    observed_intervals: list[int] = field(default_factory=list)
    drift_rate: float = 1.0
    frozen: bool = False

    @property
    def expected_interval_ticks(self) -> float:
        return self.ref_period_s / self.time_base

    def reset(self) -> None:
        """Restart unwrap state for a fresh pass over the same timestamps."""
        self.wrap_count = 0
        self.last_raw = None
        self.first_unwrapped = None
        self.last_unwrapped = None
        self.last_corrected = None

    def unwrap(self, raw_pts: int, *, strict: bool) -> int:
        """Expand modulo wraparound.

        ``strict=False`` (calibration) never raises on a backwards step;
        ``strict=True`` (correction) records the first real discontinuity.
        """
        if self.last_raw is None:
            self.last_raw = raw_pts
            self.last_unwrapped = raw_pts
            self.first_unwrapped = raw_pts
            return raw_pts
        period = self.wrap_period_ticks
        delta = raw_pts - self.last_raw
        if period is not None and delta < -period * WRAP_JUMP_FRACTION:
            self.wrap_count += 1
        unwrapped = raw_pts + (self.wrap_count * period if period else 0)
        if self.last_unwrapped is not None and unwrapped < self.last_unwrapped:
            pts_s = unwrapped * self.time_base
            if strict and self.first_anomaly_s is None:
                self.first_anomaly_s = pts_s
                raise RecoverableMediaError(
                    E_NON_MONOTONIC,
                    f"{self.name}: non-monotonic timestamp at {pts_s:.6f}s",
                    time_s=pts_s,
                    track=self.name,
                )
        if self.last_unwrapped is not None and not self.frozen:
            interval = unwrapped - self.last_unwrapped
            if interval > 0:
                self.observed_intervals.append(interval)
                if len(self.observed_intervals) >= CALIBRATION_INTERVALS:
                    self._freeze_drift()
        self.last_raw = raw_pts
        self.last_unwrapped = unwrapped
        return unwrapped

    def _freeze_drift(self) -> None:
        # The baseline interval is the median observed interval (the normal
        # frame cadence), robust to VFR gaps and to any unreliable nominal
        # frame-rate metadata.  Drift is measured against that baseline.
        baseline = float(median(self.observed_intervals))
        if baseline > 0:
            self.drift_rate = baseline / self.expected_interval_ticks
        self.frozen = True

    def freeze(self) -> None:
        if not self.frozen:
            self._freeze_drift()

    def corrected_seconds(self, unwrapped_ticks: int) -> float:
        """Seconds since the stream's first timestamp, drift-corrected."""
        base = self.first_unwrapped or 0
        return (unwrapped_ticks - base) * self.time_base * self.drift_rate

    @property
    def drift_ppm(self) -> float:
        return (self.drift_rate - 1.0) * 1e6


@dataclass
class SharedTimeline:
    """Owns every :class:`TrackTimeline` and aligns them at one origin."""

    tracks: dict[str, TrackTimeline] = field(default_factory=dict)
    origin_s: float = 0.0
    anomalies: list[dict] = field(default_factory=list)
    _frozen: bool = False

    def add_track(
        self,
        name: str,
        *,
        time_base: float,
        wrap_period_ticks: int | None,
        ref_period_s: float,
    ) -> TrackTimeline:
        track = TrackTimeline(name, time_base, wrap_period_ticks, ref_period_s)
        self.tracks[name] = track
        return track

    def calibrate(self, name: str, raw_pts: int) -> float:
        """First pass: learn unwrap/drift; never raises on discontinuity."""
        track = self.tracks[name]
        unwrapped = track.unwrap(raw_pts, strict=False)
        return unwrapped * track.time_base

    def finalize_calibration(self) -> None:
        """Freeze drift models and reset state for the correction pass."""
        for track in self.tracks.values():
            track.freeze()
            track.reset()
        self._frozen = True

    def correct(self, name: str, raw_pts: int) -> float:
        """Second pass: map a timestamp onto the shared, zero-based timeline."""
        track = self.tracks[name]
        try:
            unwrapped = track.unwrap(raw_pts, strict=True)
        except RecoverableMediaError as exc:
            self._record_anomaly(exc)
            return track.last_corrected or 0.0
        t = track.corrected_seconds(unwrapped) - self.origin_s
        if (
            track.last_corrected is not None
            and t < track.last_corrected - MONOTONIC_TOLERANCE_S
            and track.first_anomaly_s is None
        ):
            track.first_anomaly_s = t
            self._record_anomaly(
                RecoverableMediaError(
                    E_NON_MONOTONIC,
                    f"{name}: corrected timeline runs backwards at {t:.6f}s",
                    time_s=t,
                    track=name,
                )
            )
        track.last_corrected = t
        return t

    def _record_anomaly(self, exc: RecoverableMediaError) -> None:
        self.anomalies.append(exc.to_dict())

    def corrections(self) -> dict[str, dict]:
        """Per-track correction summary for the manifest."""
        return {
            name: {
                "drift_ppm": round(track.drift_ppm, 3),
                "drift_rate": round(track.drift_rate, 8),
                "wrap_count": track.wrap_count,
                "first_anomaly_s": track.first_anomaly_s,
            }
            for name, track in self.tracks.items()
        }
