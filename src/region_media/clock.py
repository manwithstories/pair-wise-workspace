"""Monotonic media-time basis shared by reporting and verification.

Two clocks exist in a live stream:

* the RTP media clock (32-bit, wraps, sample-rate dependent), and
* the wall clock carried in RTCP sender reports (NTP format).

:class:`Timeline` turns a stream of RTP timestamps into a strictly monotonic
sample counter anchored at the first packet, while :class:`RtcpMapping` keeps
the latest sender-report mapping so callers can convert between the two.  Both
emit the same :class:`~region_media.errors.Code` values so that packet, clock,
assembly and manifest code agree on what went wrong.
"""

from __future__ import annotations

import math
import struct
from dataclasses import dataclass, field
from typing import Iterable, Iterator

from .errors import Code, Diagnostic

RTP_TS_MODULUS = 1 << 32
NTP_MODULUS = 1 << 32
DEFAULT_CLOCK_RATE = 48000
DEFAULT_DRIFT_LIMIT_PPM = 500.0


@dataclass
class RtcpSenderReport:
    """Decoded RTCP sender report (RFC 3550 section 6.4.1)."""

    ssrc: int
    ntp_seconds: int
    ntp_fraction: int
    rtp_timestamp: int
    packet_count: int
    octet_count: int

    @property
    def ntp_time(self) -> float:
        """Sender wall-clock time in seconds since the NTP epoch."""
        return self.ntp_seconds + self.ntp_fraction / NTP_MODULUS

    @property
    def unix_time(self) -> float:
        """Sender wall-clock time in seconds since the Unix epoch."""
        return self.ntp_time - 2208988800.0


RTCP_SR = 200
RTCP_SR_LENGTH = 28


def parse_rtcp_sender_reports(
    data: bytes, *, where: str = ""
) -> tuple[list[RtcpSenderReport], list[Diagnostic]]:
    """Parse a compound RTCP buffer, returning sender reports and diagnostics.

    Unknown packet types are skipped deliberately: RTCP is normally compound.
    A truncated or length-inconsistent packet stops parsing and is reported
    with ``ERR_RTCP_MALFORMED`` so the caller can continue with audio.
    """
    reports: list[RtcpSenderReport] = []
    diags: list[Diagnostic] = []
    offset = 0
    while offset < len(data):
        if len(data) - offset < 4:
            diags.append(
                Diagnostic(
                    Code.RTCP_MALFORMED,
                    "RTCP header truncated",
                    where,
                    {"offset": offset, "remaining": len(data) - offset},
                )
            )
            break
        first, packet_type, length_words = struct.unpack_from(
            "!BBH", data, offset
        )
        packet_len = (length_words + 1) * 4
        if packet_len < 8 or offset + packet_len > len(data):
            diags.append(
                Diagnostic(
                    Code.RTCP_MALFORMED,
                    "RTCP packet length out of range",
                    where,
                    {"offset": offset, "declared": packet_len},
                )
            )
            break
        packet_type = packet_type & 0xFF
        if packet_type == RTCP_SR:
            if packet_len < RTCP_SR_LENGTH:
                diags.append(
                    Diagnostic(
                        Code.RTCP_MALFORMED,
                        "RTCP sender report too short",
                        where,
                        {"offset": offset, "length": packet_len},
                    )
                )
            else:
                (
                    ssrc,
                    ntp_sec,
                    ntp_frac,
                    rtp_ts,
                    pkts,
                    octets,
                ) = struct.unpack_from("!IIIIII", data, offset + 4)
                reports.append(
                    RtcpSenderReport(
                        ssrc=ssrc,
                        ntp_seconds=ntp_sec,
                        ntp_fraction=ntp_frac,
                        rtp_timestamp=rtp_ts,
                        packet_count=pkts,
                        octet_count=octets,
                    )
                )
        offset += packet_len
    return reports, diags


@dataclass
class RtcpMapping:
    """Latest sender-report mapping between the NTP wall clock and RTP clock."""

    clock_rate: int = DEFAULT_CLOCK_RATE
    ssrc: int | None = None
    ntp_time: float | None = None
    rtp_timestamp: int | None = None
    report_count: int = 0
    drift_limit_ppm: float = DEFAULT_DRIFT_LIMIT_PPM
    drift_ppm: float | None = None
    diagnostics: list[Diagnostic] = field(default_factory=list)

    @property
    def available(self) -> bool:
        return self.ntp_time is not None and self.rtp_timestamp is not None

    def media_seconds(self, rtp_timestamp: int) -> float:
        """Convert a raw RTP timestamp to seconds on the wall-clock base."""
        if not self.available:
            raise ValueError("no RTCP mapping available")
        delta = (rtp_timestamp - self.rtp_timestamp) % RTP_TS_MODULUS
        if delta >= RTP_TS_MODULUS // 2:  # signed interpretation
            delta -= RTP_TS_MODULUS
        return self.ntp_time + delta / self.clock_rate

    def wallclock_at(self, rtp_timestamp: int) -> float:
        return self.media_seconds(rtp_timestamp)

    def update(self, report: RtcpSenderReport, *, where: str = "") -> None:
        """Fold a new sender report in and measure media/wall-clock drift."""
        if self.available and self.report_count >= 1:
            wall_delta = report.ntp_time - self.ntp_time
            media_delta = (
                (report.rtp_timestamp - self.rtp_timestamp) % RTP_TS_MODULUS
            ) / self.clock_rate
            if wall_delta > 0:
                drift = (media_delta - wall_delta) / wall_delta * 1e6
                self.drift_ppm = drift
                if abs(drift) > self.drift_limit_ppm:
                    self.diagnostics.append(
                        Diagnostic(
                            Code.CLOCK_DRIFT,
                            "media clock drifts from RTCP wall clock",
                            where,
                            {
                                "drift_ppm": round(drift, 3),
                                "limit_ppm": self.drift_limit_ppm,
                                "wall_delta_s": round(wall_delta, 6),
                                "media_delta_s": round(media_delta, 6),
                            },
                        )
                    )
        self.ssrc = report.ssrc if self.ssrc is None else self.ssrc
        self.ntp_time = report.ntp_time
        self.rtp_timestamp = report.rtp_timestamp
        self.report_count += 1

    def summary(self) -> dict[str, object]:
        return {
            "available": self.available,
            "reports": self.report_count,
            "drift_ppm": None if self.drift_ppm is None else round(self.drift_ppm, 3),
        }


@dataclass
class TimestampEvent:
    """One RTP timestamp turned into a monotonic media position."""

    index: int
    raw: int
    unwrapped: int
    sample_offset: int
    wrap: bool
    discontinuity: bool


class Timeline:
    """Unwrap 32-bit RTP timestamps into a monotonic sample counter.

    ``sample_offset`` is the packet's position relative to the first packet,
    expressed in samples at ``clock_rate``.  Timestamps that jump backwards by
    more than half the modulus or forwards by more than half are treated as a
    discontinuity; they are still unwrapped so assembly can decide to insert a
    gap or trim an overlap rather than silently shifting everything.
    """

    def __init__(self, clock_rate: int = DEFAULT_CLOCK_RATE) -> None:
        if clock_rate <= 0:
            raise ValueError("clock_rate must be positive")
        self.clock_rate = clock_rate
        self._previous_raw: int | None = None
        self._previous_unwrapped: int | None = None
        self._first_unwrapped: int | None = None
        self._offset = 0
        self.diagnostics: list[Diagnostic] = []

    @property
    def origin(self) -> int:
        return self._first_unwrapped or 0

    def push(
        self, raw: int, *, where: str = ""
    ) -> TimestampEvent:
        """Feed one raw timestamp; returns its monotonic media position."""
        raw &= 0xFFFFFFFF
        wrap = False
        discontinuity = False
        if self._previous_raw is None:
            unwrapped = raw
            self._first_unwrapped = unwrapped
            sample_offset = 0
        else:
            prev_u = self._previous_unwrapped
            assert prev_u is not None
            prev_mod = prev_u % RTP_TS_MODULUS
            delta = raw - prev_mod
            if delta > RTP_TS_MODULUS // 2:
                # Raw went "backwards" by more than half the ring: a genuine
                # rewind or an extremely late pre-wrap packet, not a wrap.
                delta -= RTP_TS_MODULUS
                discontinuity = True
            elif delta < -(RTP_TS_MODULUS // 2):
                # Raw is far ahead in ring terms: the timestamp crossed the
                # modulus. The unwrapped step is small and forward.
                delta += RTP_TS_MODULUS
                wrap = True
            if not wrap and delta < 0 and -delta > self.clock_rate:
                # A rewind larger than a second is a genuine timeline break,
                # not ordinary reordering.
                discontinuity = True
            unwrapped = prev_u + delta
            sample_offset = unwrapped - (self._first_unwrapped or 0)
            if discontinuity:
                self.diagnostics.append(
                    Diagnostic(
                        Code.TIMESTAMP_DISCONTINUITY,
                        "RTP timestamp jumps discontinuously",
                        where,
                        {
                            "previous": self._previous_raw,
                            "raw": raw,
                            "delta_samples": delta,
                        },
                    )
                )
        if wrap:
            self.diagnostics.append(
                Diagnostic(
                    Code.TIMESTAMP_WRAP,
                    "RTP timestamp wrapped the 32-bit modulus",
                    where,
                    {"previous": self._previous_raw, "raw": raw},
                )
            )
        self._previous_raw = raw
        self._previous_unwrapped = unwrapped
        self._offset += 1
        return TimestampEvent(
            index=self._offset - 1,
            raw=raw,
            unwrapped=unwrapped,
            sample_offset=sample_offset,
            wrap=wrap,
            discontinuity=discontinuity,
        )

    def samples_between(self, earlier_raw: int, later_raw: int) -> int:
        """Shortest signed distance in samples between two raw timestamps."""
        delta = (later_raw - earlier_raw) % RTP_TS_MODULUS
        if delta > RTP_TS_MODULUS // 2:
            delta -= RTP_TS_MODULUS
        return delta


def media_time_from_samples(samples: int, rate: int) -> float:
    if rate <= 0:
        raise ValueError("rate must be positive")
    return samples / rate


def sample_offset_for_time(seconds: float, rate: int) -> int:
    return int(round(seconds * rate))


def drift_ppm(
    measured_samples: int, rate: int, wall_seconds: float
) -> float:
    """Report how far the media clock ran from the wall clock, in ppm."""
    if wall_seconds <= 0 or rate <= 0:
        return 0.0
    expected = wall_seconds * rate
    return (measured_samples - expected) / expected * 1e6


def iter_unwrapped(
    timestamps: Iterable[int], clock_rate: int = DEFAULT_CLOCK_RATE
) -> Iterator[TimestampEvent]:
    tl = Timeline(clock_rate)
    for ts in timestamps:
        yield tl.push(ts)


def ntp_to_unix(seconds: int, fraction: int) -> float:
    return seconds + fraction / NTP_MODULUS - 2208988800.0


def is_finite_metric(value: float | None) -> bool:
    return value is not None and math.isfinite(value)
