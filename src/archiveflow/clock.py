"""Unified 90 kHz media clock with RTCP Sender Report mapping.

Maps extended RTP timestamps onto a monotonically increasing media clock
in milliseconds. Sender Reports anchor RTP timestamps to NTP wall-clock
time; when reports are missing or mutually conflicting the most recent
trusted mapping is kept and an uncertainty estimate is reported instead of
silently switching reference. The mapper never emits negative or
non-finite timestamps.
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
# Uncertainty grows by this much per conflict and per missing expected SR.
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
        self._anchor_rtp: int | None = None  # extended RTP ts at anchor
        self._anchor_ntp: float | None = None  # NTP seconds at anchor
        self._origin_ms: float = 0.0  # media clock at anchor
        self._uncertainty_ms: float = 0.0
        self.conflicts: int = 0
        self.reports: int = 0

    @property
    def uncertainty_ms(self) -> float:
        return self._uncertainty_ms

    def _offset(self) -> float | None:
        if self._anchor_rtp is None or self._anchor_ntp is None:
            return None
        return self._anchor_ntp - self._anchor_rtp / self.clock_rate

    def add_sender_report(self, sr: RtcpSR, rtp_ts_ext: int) -> None:
        """Fold a Sender Report into the mapping.

        ``rtp_ts_ext`` is the SR's RTP timestamp extended into the same
        64-bit domain as the packet stream. Conflicting reports are
        counted and ignored; the last trusted mapping stays in effect.
        """
        if not math.isfinite(sr.ntp_seconds) or sr.ntp_seconds < 0:
            raise UnrecoverableError("non-finite NTP time in Sender Report")
        self.reports += 1
        new_offset = sr.ntp_seconds - rtp_ts_ext / self.clock_rate
        offset = self._offset()
        if offset is not None and abs(new_offset - offset) > CONFLICT_TOLERANCE_S:
            self.conflicts += 1
            self._uncertainty_ms += CONFLICT_UNCERTAINTY_MS
            return  # keep last trusted mapping
        if offset is None:
            self._origin_ms = 0.0
        self._anchor_rtp = rtp_ts_ext
        self._anchor_ntp = sr.ntp_seconds

    def note_missing_report(self) -> None:
        """Record that an expected Sender Report never arrived."""
        self._uncertainty_ms += MISSING_SR_UNCERTAINTY_MS

    def map(self, rtp_ts_ext: int) -> ClockSample:
        """Map an extended RTP timestamp to media-clock milliseconds."""
        if self._anchor_rtp is None or self._anchor_ntp is None:
            # No mapping yet: treat first seen timestamp as origin.
            self._anchor_rtp = rtp_ts_ext
            self._anchor_ntp = rtp_ts_ext / self.clock_rate
            self._uncertainty_ms += MISSING_SR_UNCERTAINTY_MS
        delta_s = (rtp_ts_ext - self._anchor_rtp) / self.clock_rate
        clock_ms = self._origin_ms + delta_s * 1000.0
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
