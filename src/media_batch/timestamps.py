"""Multi-standard timecode handling: fractional frame rates, midnight wrap,
duplicate/regression detection, and clock-drift estimation.

Frame numbers are integers; rates are Fractions; no float accumulation.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from fractions import Fraction

from .errors import ErrorLog
from .timebase import FPS_2997, parse_frame_rate

_TC = re.compile(r"^(\d{1,2}):(\d{2}):(\d{2})([:;])(\d{1,2})$")
_FRAMES_PER_DAY_2997 = 2589408  # 24h of 29.97 drop-frame timecode

# Drift beyond 100 ms per minute marks an interval as untrusted.
DRIFT_LIMIT = Fraction(100, 60_000)  # dimensionless: 100ms per 60000ms


def is_drop_frame(fps: Fraction) -> bool:
    return fps == FPS_2997


def timecode_to_frames(tc: str, fps: Fraction) -> int:
    """Parse HH:MM:SS:FF (';' for drop-frame) into an absolute frame number."""
    m = _TC.match(tc.strip())
    if not m:
        raise ValueError(f"malformed timecode: {tc!r}")
    hh, mm, ss, sep, ff = int(m[1]), int(m[2]), int(m[3]), m[4], int(m[5])
    if mm >= 60 or ss >= 60:
        raise ValueError(f"timecode component out of range: {tc!r}")
    nominal = int(fps + Fraction(1, 2))  # 30 for 29.97
    if ff >= nominal:
        raise ValueError(f"frame number out of range: {tc!r}")
    if is_drop_frame(fps):
        if sep == ";" and ss == 0 and ff < 2 and mm % 10 != 0:
            raise ValueError(f"dropped frame number used: {tc!r}")
        total_minutes = hh * 60 + mm
        return ((hh * 3600 + mm * 60 + ss) * nominal + ff
                - 2 * (total_minutes - total_minutes // 10))
    return (hh * 3600 + mm * 60 + ss) * int(fps) + ff


def frames_per_day(fps: Fraction) -> int:
    if is_drop_frame(fps):
        return _FRAMES_PER_DAY_2997
    return int(fps) * 86400


@dataclass
class TimecodeSeries:
    """Ordered timecodes unwrapped across midnight; never silently reordered."""

    frames: list[int]  # unwrapped absolute frame numbers
    wraps: int


def unwrap_timecodes(codes: list[str], fps: Fraction, log: ErrorLog,
                     source: str = "") -> TimecodeSeries:
    """Convert timecode strings to monotonically unwrapped frame numbers.

    A drop of roughly one day's worth of frames is treated as midnight wrap;
    any other regression or duplicate is logged, never sorted away.
    """
    frames: list[int] = []
    wraps = 0
    day = frames_per_day(fps)
    prev_raw: int | None = None
    offset = 0
    for i, tc in enumerate(codes):
        try:
            raw = timecode_to_frames(tc, fps)
        except ValueError as exc:
            log.add("invalid_timecode", str(exc), source, -1, i)
            continue
        if prev_raw is not None:
            delta = raw - prev_raw
            if delta < 0 and -delta >= day // 2:
                wraps += 1
                offset += day
            elif delta < 0:
                log.add("timestamp_regression",
                        f"timecode {tc!r} regresses from previous {codes[i - 1]!r}",
                        source, -1, i)
            elif delta == 0:
                log.add("timestamp_duplicate",
                        f"duplicate timecode {tc!r}", source, -1, i)
        prev_raw = raw
        frames.append(raw + offset)
    return TimecodeSeries(frames, wraps)


@dataclass
class DriftReport:
    drift_ppm: Fraction          # signed drift rate (fraction of elapsed time)
    ms_per_minute: Fraction      # same, in ms/min
    untrusted: list[tuple[int, int]]  # (start_sample, end_sample) intervals
    trusted: bool


def estimate_drift(anchor_samples: list[int], packet_times: list[Fraction],
                   sample_rate: int,
                   limit: Fraction = DRIFT_LIMIT) -> DriftReport:
    """Compare integer audio-sample times against container packet times.

    anchor_samples[i] and packet_times[i] (seconds, rational) describe the same
    media instant from the two clocks. If the implied drift exceeds `limit`
    (default 100 ms/min) the affected span is marked untrusted; durations are
    never compressed to compensate.
    """
    if len(anchor_samples) < 2 or len(anchor_samples) != len(packet_times):
        return DriftReport(Fraction(0), Fraction(0), [], trusted=True)
    s0, s1 = anchor_samples[0], anchor_samples[-1]
    t0, t1 = packet_times[0], packet_times[-1]
    span_samples = s1 - s0
    if span_samples <= 0:
        return DriftReport(Fraction(0), Fraction(0), [], trusted=True)
    sample_span = Fraction(span_samples, sample_rate)
    packet_span = t1 - t0
    if sample_span <= 0:
        return DriftReport(Fraction(0), Fraction(0), [], trusted=True)
    drift = (packet_span - sample_span) / sample_span
    ms_per_min = drift * 60_000
    untrusted: list[tuple[int, int]] = []
    trusted = True
    if abs(drift) > limit:
        trusted = False
        # mark every span between consecutive anchors whose local drift exceeds
        # the limit; fall back to the whole range
        marked = False
        for i in range(len(anchor_samples) - 1):
            ds = anchor_samples[i + 1] - anchor_samples[i]
            if ds <= 0:
                continue
            local = ((packet_times[i + 1] - packet_times[i])
                     - Fraction(ds, sample_rate)) / Fraction(ds, sample_rate)
            if abs(local) > limit:
                untrusted.append((anchor_samples[i], anchor_samples[i + 1]))
                marked = True
        if not marked:
            untrusted.append((s0, s1))
    return DriftReport(drift, ms_per_min, untrusted, trusted)


def parse_rate(name: str) -> Fraction:
    return parse_frame_rate(name)
