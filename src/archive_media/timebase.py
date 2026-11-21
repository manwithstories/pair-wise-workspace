"""Unified media timebase: wraparound unwrap, drift estimation, monotonicity.

Every stream is corrected onto one shared timeline measured in seconds:

1. Raw integer PTS ticks are *unwrapped* across modulo counter wraparound.
2. Tick values become seconds via the stream's own ``time_base``.
3. A linear model ``corrected = alpha + beta * pts_seconds`` is fit against
   a hardware-independent reference (audio sample index / sample-rate, or
   video frame index / nominal frame-rate).  Its slope captures linear
   clock drift and its intercept the clock offset.
4. A shared origin (earliest first timestamp) is subtracted so audio and
   video share a common zero, and the corrected stream is checked for
   monotonicity; the first un-recoverable position is recorded.

Regression uses running sums only (O(1) state, no per-sample cache) and is
frozen after a calibration window so output is deterministic for reruns.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .errors import E_NON_MONOTONIC, RecoverableMediaError

# Fraction of the wrap period by which a backwards jump must exceed to be
# treated as a genuine discontinuity rather than a wraparound step.
WRAP_JUMP_FRACTION = 0.25
# Calibration window (per track) after which the drift model is frozen.
CALIBRATION_SAMPLES = 64
# Allowed backwards excursion (seconds) before a region is non-monotonic.
MONOTONIC_TOLERANCE_S = 0.002


@dataclass
class LinearModel:
    """y = alpha + beta * x fit with running sums (least squares)."""

    n: int = 0
    sx: float = 0.0
    sy: float = 0.0
    sxx: float = 0.0
    sxy: float = 0.0
    alpha: float = 0.0
    beta: float = 1.0
    frozen: bool = False

    def observe(self, x: float, y: float) -> None:
        if self.frozen:
            return
        self.n += 1
        self.sx += x
        self.sy += y
        self.sxx += x * x
        self.sxy += x * y

    def freeze(self) -> None:
        if self.frozen or self.n < 2:
            self.frozen = True
            return
        denom = self.n * self.sxx - self.sx * self.sx
        if denom != 0.0:
            self.beta = (self.n * self.sxy - self.sx * self.sy) / denom
            self.alpha = (self.sy - self.beta * self.sx) / self.n
        self.frozen = True

    def predict(self, x: float) -> float:
        return self.alpha + self.beta * x


@dataclass
class TrackTimeline:
    """Per-stream unwrap + drift correction state."""

    name: str
    time_base: float
    wrap_period_ticks: int | None
    ref_period_s: float
    wrap_count: int = 0
    last_raw: int | None = None
    last_unwrapped: int | None = None
    last_corrected: float | None = None
    first_anomaly_s: float | None = None
    model: LinearModel = field(default_factory=LinearModel)
    _first_pts_s: float | None = None

    def unwrap(self, raw_pts: int) -> int:
        """Expand modulo wraparound; flag jumps that cannot be explained."""
        if self.last_raw is None:
            self.last_raw = raw_pts
            self.last_unwrapped = raw_pts
            return raw_pts
        period = self.wrap_period_ticks
        delta = raw_pts - self.last_raw
        if period is not None and delta < -period * WRAP_JUMP_FRACTION:
            self.wrap_count += 1
        unwrapped = raw_pts + (self.wrap_count * period if period else 0)
        if self.last_unwrapped is not None and unwrapped < self.last_unwrapped:
            # A backwards step surviving unwrap is a real discontinuity.
            pts_s = unwrapped * self.time_base
            if self.first_anomaly_s is None:
                self.first_anomaly_s = pts_s
            raise RecoverableMediaError(
                E_NON_MONOTONIC,
                f"{self.name}: non-monotonic timestamp at {pts_s:.6f}s",
                time_s=pts_s,
                track=self.name,
            )
        self.last_raw = raw_pts
        self.last_unwrapped = unwrapped
        return unwrapped

    def observe(self, raw_pts: int, ref_index: int) -> float:
        """Feed one timestamp; return corrected seconds (pre-shared-origin)."""
        unwrapped = self.unwrap(raw_pts)
        pts_s = unwrapped * self.time_base
        if self._first_pts_s is None:
            self._first_pts_s = pts_s
        ref_s = ref_index * self.ref_period_s
        self.model.observe(pts_s, ref_s)
        if not self.model.frozen and self.model.n >= CALIBRATION_SAMPLES:
            self.model.freeze()
        return self.model.predict(pts_s)

    def freeze(self) -> None:
        self.model.freeze()

    def correct(self, raw_pts: int) -> float:
        unwrapped = self.unwrap(raw_pts)
        return self.model.predict(unwrapped * self.time_base)

    @property
    def drift_ppm(self) -> float:
        """Linear clock drift in parts-per-million vs. the reference."""
        return (self.model.beta - 1.0) * 1e6

    @property
    def offset_s(self) -> float:
        return self.model.alpha


@dataclass
class SharedTimeline:
    """Owns every :class:`TrackTimeline` and aligns them at one origin."""

    tracks: dict[str, TrackTimeline] = field(default_factory=dict)
    origin_s: float | None = None
    anomalies: list[dict] = field(default_factory=list)

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

    def _place(self, track: TrackTimeline, corrected_s: float) -> float:
        if self.origin_s is None or corrected_s < self.origin_s:
            self.origin_s = corrected_s
        return corrected_s - (self.origin_s or 0.0)

    def observe(self, name: str, raw_pts: int, ref_index: int) -> float:
        """Calibration-phase feed; returns shared-timeline seconds."""
        track = self.tracks[name]
        try:
            corrected = track.observe(raw_pts, ref_index)
        except RecoverableMediaError as exc:
            self._record_anomaly(exc)
            corrected = track.last_corrected or 0.0
        return self._place(track, corrected)

    def correct(self, name: str, raw_pts: int) -> float:
        """Streaming correction after calibration; monotonicity checked."""
        track = self.tracks[name]
        try:
            corrected = track.correct(raw_pts)
        except RecoverableMediaError as exc:
            self._record_anomaly(exc)
            return track.last_corrected or 0.0
        t = corrected - (self.origin_s or 0.0)
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

    def freeze(self) -> None:
        for track in self.tracks.values():
            track.freeze()

    def corrections(self) -> dict[str, dict]:
        """Per-track correction summary for the manifest."""
        out = {}
        for name, track in self.tracks.items():
            out[name] = {
                "drift_ppm": round(track.drift_ppm, 3),
                "offset_s": round(track.offset_s, 6),
                "wrap_count": track.wrap_count,
                "first_anomaly_s": track.first_anomaly_s,
            }
        return out
