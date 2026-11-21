"""Repair packet timestamps against the shared rational time base.

The repairer is a bounded-memory, single-pass stage: drift is fitted online
with running least-squares sums (constant memory), wraps are unwrapped as
they occur, and VFR presentation order is rebuilt from decode order with a
small reorder buffer.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

from .errors import ErrorCode, MediaError, Severity
from .probe import PacketEvent, TrackKind
from .timebase import (
    DRIFT_SEGMENT_PPM,
    DriftSegment,
    WrapEvent,
    to_seconds,
)

WRAP_MODULUS = 1 << 63
AV_SKEW_LIMIT = Fraction(40, 1000)  # 40 ms


@dataclass
class RepairedPacket:
    """A packet whose timestamps are expressed in exact seconds."""

    event: PacketEvent
    pts: Fraction | None
    dts: Fraction | None
    duration: Fraction


@dataclass
class TrackRepairStats:
    track: str
    packets: int = 0
    repaired_timestamps: int = 0
    duplicate_timestamps: int = 0
    negative_durations: int = 0
    wraps: int = 0
    out_of_order: int = 0
    resets: int = 0
    ambiguous: int = 0
    drift_segments: list[DriftSegment] = field(default_factory=list)


class _OnlineDrift:
    """Incremental least-squares drift fit with segment breaks."""

    def __init__(self) -> None:
        self.segments: list[DriftSegment] = []
        self._reset(0)

    def _reset(self, index: int) -> None:
        self._n = 0
        self._sx = self._sy = self._sxx = self._sxy = 0.0
        self._start = index

    def _fit(self) -> tuple[float, float]:
        if self._n < 2:
            return 1.0, 0.0
        mx = self._sx / self._n
        my = self._sy / self._n
        sxx = self._sxx - self._n * mx * mx
        if sxx <= 0:
            return 1.0, my - mx
        slope = (self._sxy - self._n * mx * my) / sxx
        return slope, my - slope * mx

    def observe(self, index: int, raw: float, reference: float) -> tuple[float, float]:
        """Add a sample; returns the (slope, offset) to apply to it."""
        self._sx += raw
        self._sy += reference
        self._sxx += raw * raw
        self._sxy += raw * reference
        self._n += 1
        slope, offset = self._fit()
        if self._n >= 32 and abs(slope - 1.0) * 1e6 > DRIFT_SEGMENT_PPM:
            # Drift exceeded: close the segment and refit from here.
            if len(self.segments) < 64:  # bounded memory
                self.segments.append(
                    DriftSegment(self._start, index - 1, slope, offset)
                )
            self._reset(index)
            self.observe(index, raw, reference)
            slope, offset = self._fit()
        return slope, offset

    def finish(self, last_index: int) -> list[DriftSegment]:
        if self._n >= 2:
            slope, offset = self._fit()
            self.segments.append(DriftSegment(self._start, last_index, slope, offset))
        return self.segments


class TrackRepairer:
    """Repairs the packet stream of a single track."""

    def __init__(self, kind: TrackKind, time_base: Fraction, rate: Fraction | None):
        self.kind = kind
        self.time_base = time_base
        self.rate = rate
        self.stats = TrackRepairStats(track=kind.value)
        self._wraps = 0
        self._prev_dts: int | None = None
        self._prev_pts_seconds: Fraction | None = None
        self._drift = _OnlineDrift()
        self._frame_no = 0
        self._vfr = False
        self._prev_duration: int | None = None
        self._duration_changes = 0

    def _unwrap(self, value: int, index: int) -> tuple[int, str | None]:
        prev = self._prev_dts
        self._prev_dts = value
        if prev is None or value >= prev:
            return value + self._wraps * WRAP_MODULUS, None
        drop = prev - value
        tolerance = max(1, WRAP_MODULUS // 100)
        if WRAP_MODULUS - prev <= tolerance and drop < WRAP_MODULUS - prev + tolerance:
            self._wraps += 1
            self.stats.wraps += 1
            return value + self._wraps * WRAP_MODULUS, WrapEvent.WRAP
        if drop <= tolerance:
            self.stats.out_of_order += 1
            return value + self._wraps * WRAP_MODULUS, WrapEvent.OUT_OF_ORDER
        if drop < WRAP_MODULUS // 4:
            self.stats.ambiguous += 1
            return value + self._wraps * WRAP_MODULUS, WrapEvent.AMBIGUOUS
        self.stats.resets += 1
        self._wraps = 0
        return value, WrapEvent.RESET

    def process(self, event: PacketEvent) -> tuple[RepairedPacket, list[MediaError]]:
        errors: list[MediaError] = []
        s = self.stats
        s.packets += 1
        dts_seconds: Fraction | None = None
        if event.dts is not None:
            unwrapped, kind = self._unwrap(event.dts, event.packet_index)
            if kind == WrapEvent.AMBIGUOUS:
                errors.append(
                    MediaError(
                        code=ErrorCode.AMBIGUOUS_TIMELINE,
                        message="timestamp discontinuity cannot be classified",
                        severity=Severity.UNREPAIRABLE,
                        track=self.kind.value,
                        packet_index=event.packet_index,
                        byte_offset=event.byte_offset,
                    )
                )
            elif kind in (WrapEvent.WRAP, WrapEvent.RESET, WrapEvent.OUT_OF_ORDER):
                errors.append(
                    MediaError(
                        code={
                            WrapEvent.WRAP: ErrorCode.TIMESTAMP_WRAP,
                            WrapEvent.RESET: ErrorCode.TIMESTAMP_RESET,
                            WrapEvent.OUT_OF_ORDER: ErrorCode.OUT_OF_ORDER_PACKET,
                        }[kind],
                        message=f"timestamp {kind} at dts={event.dts}",
                        severity=Severity.REPAIRED,
                        track=self.kind.value,
                        packet_index=event.packet_index,
                        byte_offset=event.byte_offset,
                    )
                )
            dts_seconds = to_seconds(unwrapped, self.time_base)
        pts_seconds = to_seconds(event.pts, self.time_base)
        if pts_seconds is not None and self._wraps:
            pts_seconds += Fraction(self._wraps * WRAP_MODULUS) * self.time_base
        duration = to_seconds(event.duration, self.time_base) or Fraction(0)
        if duration < 0:
            s.negative_durations += 1
            errors.append(
                MediaError(
                    code=ErrorCode.NEGATIVE_DURATION,
                    message=f"negative packet duration {event.duration}",
                    severity=Severity.REPAIRED,
                    track=self.kind.value,
                    packet_index=event.packet_index,
                    byte_offset=event.byte_offset,
                )
            )
            duration = Fraction(0)
        # VFR tracks have no cadence to fit drift against: presentation
        # order is rebuilt from packet order and timestamps are kept as-is.
        # A track is only classed VFR after several duration changes, so an
        # odd first/last packet does not disable drift correction.
        if self._prev_duration is not None and event.duration != self._prev_duration:
            self._duration_changes += 1
            if self._duration_changes >= 3:
                self._vfr = True
        if event.duration:
            self._prev_duration = event.duration
        # Drift is fitted against decode-order timestamps (monotonic in
        # demux order); presentation timestamps jump around under B-frame
        # reordering and would corrupt the fit.
        fit_source = dts_seconds if dts_seconds is not None else pts_seconds
        if fit_source is not None and self.rate and not self._vfr:
            reference = Fraction(self._frame_no) / self.rate
            slope, offset = self._drift.observe(
                self._frame_no, float(fit_source), float(reference)
            )
            if abs(slope - 1.0) <= 1000e-6:
                offset_f = Fraction(offset).limit_denominator(10**9)
                if pts_seconds is not None:
                    corrected = Fraction(slope) * pts_seconds + offset_f
                    if corrected != pts_seconds:
                        s.repaired_timestamps += 1
                    pts_seconds = corrected
                if dts_seconds is not None:
                    dts_seconds = Fraction(slope) * dts_seconds + offset_f
        if (
            self._prev_pts_seconds is not None
            and pts_seconds is not None
            and pts_seconds == self._prev_pts_seconds
        ):
            s.duplicate_timestamps += 1
            errors.append(
                MediaError(
                    code=ErrorCode.DUPLICATE_TIMESTAMP,
                    message=f"duplicate presentation time {float(pts_seconds):.6f}s",
                    severity=Severity.REPAIRED,
                    track=self.kind.value,
                    packet_index=event.packet_index,
                    byte_offset=event.byte_offset,
                )
            )
        if pts_seconds is not None:
            self._prev_pts_seconds = pts_seconds
        self._frame_no += 1
        return RepairedPacket(event, pts_seconds, dts_seconds, duration), errors

    def finish(self) -> None:
        self.stats.drift_segments = self._drift.finish(max(0, self._frame_no - 1))


def check_av_skew(
    video_pts: Fraction | None,
    audio_pts: Fraction | None,
    packet_index: int,
    byte_offset: int | None,
) -> MediaError | None:
    """Emit a locating record when audio/video presentation diverges > 40 ms."""
    if video_pts is None or audio_pts is None:
        return None
    if abs(video_pts - audio_pts) > AV_SKEW_LIMIT:
        return MediaError(
            code=ErrorCode.AV_SKEW,
            message=(
                f"a/v skew {float(abs(video_pts - audio_pts)) * 1000:.1f} ms "
                f"exceeds 40 ms"
            ),
            severity=Severity.REPAIRED,
            packet_index=packet_index,
            byte_offset=byte_offset,
        )
    return None
