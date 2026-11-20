"""Unified 90 kHz media clock with RTCP Sender Report mapping.

Extended RTP timestamps are mapped onto a monotonically increasing media
clock in milliseconds, anchored at an explicit origin (the earliest
timestamp seen on the stream). Sender Reports anchor the RTP timeline to
NTP wall-clock time and validate the mapping; when reports are missing or
mutually conflicting, the most recent trusted mapping is kept and an
uncertainty estimate is reported instead of silently switching reference.
The mapper never emits negative or non-finite timestamps.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

from . import CLOCK_RATE
from .errors import UnrecoverableError
from .rtp import RtcpSR

# Two SRs conflict when their implied RTP<->NTP offset differs by more
# than this many seconds.
CONFLICT_TOLERANCE_S = 0.5
CONFLICT_UNCERTAINTY_MS = 250.0
MISSING_SR_UNCERTAINTY_MS = 100.0


@dataclass(frozen=True)
class ClockSample:
    clock_ms: float  # media clock in milliseconds, always >= 0 and finite
    uncertainty_ms: float


class MediaClock:
    """RTP-timestamp -> media-clock mapper for one SSRC."""

    def __init__(self, clock_rate: int = CLOCK_RATE) -> None:
        self.clock_rate = clock_rate
        self._origin_rtp: int | None = None
        self._trusted_offset: float | None = None  # ntp - rtp/rate seconds
        self._uncertainty_ms: float = 0.0
        self.conflicts: int = 0
        self.reports: int = 0

    @property
    def uncertainty_ms(self) -> float:
        return self._uncertainty_ms

    def set_origin(self, rtp_ts_ext: int) -> None:
        """Pin the media-clock origin to the earliest stream timestamp."""
        if self._origin_rtp is None or rtp_ts_ext < self._origin_rtp:
            self._origin_rtp = rtp_ts_ext

    def add_sender_report(self, sr: RtcpSR, rtp_ts_ext: int) -> None:
        """Fold a Sender Report into the mapping.

        Conflicting reports are counted and ignored; the last trusted
        mapping stays in effect and uncertainty grows instead.
        """
        if not math.isfinite(sr.ntp_seconds) or sr.ntp_seconds < 0:
            raise UnrecoverableError("non-finite NTP time in Sender Report")
        self.reports += 1
        offset = sr.ntp_seconds - rtp_ts_ext / self.clock_rate
        if self._trusted_offset is None:
            self._trusted_offset = offset
            return
        if abs(offset - self._trusted_offset) > CONFLICT_TOLERANCE_S:
            self.conflicts += 1
            self._uncertainty_ms += CONFLICT_UNCERTAINTY_MS
            return  # keep last trusted mapping
        self._trusted_offset = offset

    def note_missing_reports(self) -> None:
        """Record that no usable Sender Report anchored this stream."""
        self._uncertainty_ms += MISSING_SR_UNCERTAINTY_MS

    def map(self, rtp_ts_ext: int) -> ClockSample:
        """Map an extended RTP timestamp to media-clock milliseconds."""
        if self._origin_rtp is None:
            self._origin_rtp = rtp_ts_ext
        if self._trusted_offset is None and self.reports == 0:
            self.note_missing_reports()
            self.reports = -1  # only note once
        clock_ms = (rtp_ts_ext - self._origin_rtp) / self.clock_rate * 1000.0
        if not math.isfinite(clock_ms):
            raise UnrecoverableError("non-finite media clock value")
        return ClockSample(clock_ms=max(0.0, clock_ms),
                           uncertainty_ms=self._uncertainty_ms)


class ClockDomain:
    """Holds one MediaClock per SSRC and routes Sender Reports."""

    def __init__(self) -> None:
        self._clocks: dict[int, MediaClock] = {}

    def clock_for(self, ssrc: int) -> MediaClock:
        return self._clocks.setdefault(ssrc, MediaClock())

    def add_sender_report(self, sr: RtcpSR, rtp_ts_ext: int) -> None:
        self.clock_for(sr.ssrc).add_sender_report(sr, rtp_ts_ext)

    def max_uncertainty_ms(self) -> float:
        if not self._clocks:
            return 0.0
        return max(c.uncertainty_ms for c in self._clocks.values())
