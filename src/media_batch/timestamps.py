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


def split_timecode_lines(data: bytes) -> list[tuple[str, int]]:
    """Split raw timecode text into (stripped line, byte offset) pairs.

    Blank lines are skipped but still advance the byte offset, so every
    reported offset points at the exact first byte of its record.
    """
    out: list[tuple[str, int]] = []
    pos = 0
    for raw in data.split(b"\n"):
        line_off = pos
        pos += len(raw) + 1
        text = raw.rstrip(b"\r").strip()
        if text:
            out.append((text.decode("utf-8", errors="replace"), line_off))
    return out


def unwrap_timecodes(codes: list[str], fps: Fraction, log: ErrorLog,
                     source: str = "", offsets: list[int] | None = None
                     ) -> TimecodeSeries:
    """Convert timecode strings to monotonically unwrapped frame numbers.

    A drop of roughly one day's worth of frames is treated as midnight wrap;
    any other regression or duplicate is logged with the byte offset of the
    offending line, never sorted away.
    """
    frames: list[int] = []
    wraps = 0
    day = frames_per_day(fps)
    prev_raw: int | None = None
    prev_tc: str | None = None
    offset = 0
    for i, tc in enumerate(codes):
        off = offsets[i] if offsets is not None and i < len(offsets) else -1
        try:
            raw = timecode_to_frames(tc, fps)
        except ValueError as exc:
            log.add("invalid_timecode", str(exc), source, off, i)
            continue
        if prev_raw is not None:
            delta = raw - prev_raw
            if delta < 0 and -delta >= day // 2:
                wraps += 1
                offset += day
            elif delta < 0:
                log.add("timestamp_regression",
                        f"timecode {tc!r} regresses from previous {prev_tc!r}",
                        source, off, i)
            elif delta == 0:
                log.add("timestamp_duplicate",
                        f"duplicate timecode {tc!r}", source, off, i)
        prev_raw, prev_tc = raw, tc
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
                   limit: Fraction = DRIFT_LIMIT,
                   window_seconds: int = 60) -> DriftReport:
    """Compare integer audio-sample times against container packet times.

    anchor_samples[i] and packet_times[i] (seconds, rational) describe the same
    media instant from the two clocks. If the implied drift exceeds `limit`
    (default 100 ms/min) the affected span is marked untrusted; durations are
    never compressed to compensate.

    Drift is measured over non-overlapping windows of about `window_seconds`
    rather than between adjacent packets: packet timestamps are quantised by
    the container time base, and a single packet's rounding error would
    otherwise look like a huge local drift.
    """
    if len(anchor_samples) < 2 or len(anchor_samples) != len(packet_times):
        return DriftReport(Fraction(0), Fraction(0), [], trusted=True)
    if anchor_samples[-1] - anchor_samples[0] <= 0:
        return DriftReport(Fraction(0), Fraction(0), [], trusted=True)

    def _local(a: int, b: int) -> Fraction:
        ds = anchor_samples[b] - anchor_samples[a]
        if ds <= 0:
            return Fraction(0)
        elapsed = Fraction(ds, sample_rate)
        return ((packet_times[b] - packet_times[a]) - elapsed) / elapsed

    drift = _local(0, len(anchor_samples) - 1)
    ms_per_min = drift * 60_000
    untrusted: list[tuple[int, int]] = []
    if abs(ms_per_min) > limit * 60_000:
        window = max(1, window_seconds) * sample_rate
        i = 0
        n = len(anchor_samples)
        while i < n - 1:
            j = i
            while j + 1 < n and anchor_samples[j + 1] - anchor_samples[i] <= window:
                j += 1
            if j == i:
                j = i + 1
            if abs(_local(i, j) * 60_000) > limit * 60_000:
                untrusted.append((anchor_samples[i], anchor_samples[j]))
            i = j
    return DriftReport(drift, ms_per_min, untrusted, trusted=not untrusted)


def parse_rate(name: str) -> Fraction:
    return parse_frame_rate(name)
