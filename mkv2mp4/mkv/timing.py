"""Timestamp rewriting: PTS normalisation, DTS derivation, edit lists.

All arithmetic is exact integer math on int64 NumPy arrays.  Source
timecodes (already converted to nanoseconds by the document layer) are
scaled into the target MP4 timescale, the first frame is rebased to
PTS 0, and a monotonically non-decreasing DTS is derived by stable
PTS ordering.  Reordering (B-frames) that would push DTS negative is
compensated by shifting composition offsets up and moving the
presentation origin forward with an edit list.
"""

import numpy as np

from ..errors import TimestampError

NS_PER_SECOND = 1_000_000_000


class TrackTiming:
    """Per-track timing tables in the target timescale (decode order)."""

    def __init__(self, pts, dts, ctts, delta, edit_media_time, timescale):
        self.pts = pts            # int64 array, zero-based presentation
        self.dts = dts            # int64 array, monotonic non-decreasing
        self.ctts = ctts          # int64 array, composition offsets >= 0
        self.delta = delta        # nominal sample duration in ticks
        self.edit_media_time = edit_media_time  # elst media_time (ticks)
        self.timescale = timescale

    @property
    def sample_count(self):
        return int(self.pts.size)

    @property
    def duration_ticks(self):
        return int(self.dts[-1] + self.delta)

    @property
    def has_b_frames(self):
        return bool(np.any(self.ctts != 0))

    @property
    def first_pts_ms(self):
        return float(self.pts.min()) * 1000.0 / self.timescale

    @property
    def last_pts_ms(self):
        return float(self.pts.max()) * 1000.0 / self.timescale


def ns_to_ticks(pts_ns, timescale):
    """Convert nanosecond timestamps to ticks with exact rounding."""
    if not isinstance(timescale, int) or timescale <= 0:
        raise TimestampError(
            "target timescale must be a positive integer, got %r"
            % (timescale,))
    arr = np.asarray(pts_ns, dtype=np.int64)
    return (arr * timescale + NS_PER_SECOND // 2) // NS_PER_SECOND


def build_timing(pts_ns, timescale, default_duration_ns=0):
    """Build DTS/CTTS tables from source PTS (nanoseconds, file order)."""
    ticks = ns_to_ticks(pts_ns, timescale)
    if ticks.size == 0:
        raise TimestampError("track has no samples")
    ticks = ticks - ticks.min()  # first frame rebased to PTS 0
    delta = 0
    if default_duration_ns > 0:
        delta = int((default_duration_ns * timescale
                     + NS_PER_SECOND // 2) // NS_PER_SECOND)
    if delta <= 0 and ticks.size > 1:
        ordered = np.sort(ticks, kind="stable")
        span = int(ordered[-1] - ordered[0])
        delta = (span + ticks.size - 2) // (ticks.size - 1)
        if delta <= 0:
            raise TimestampError(
                "cannot derive a positive frame duration "
                "(all timestamps identical)")
    if delta <= 0:
        delta = timescale  # single-sample track: arbitrary 1s duration
    dts = np.arange(ticks.size, dtype=np.int64) * delta
    offsets = ticks.astype(np.int64) - dts
    shift = int(-offsets.min()) if offsets.min() < 0 else 0
    ctts = offsets + shift
    return TrackTiming(ticks, dts, ctts, delta, shift, timescale)


def rle(values):
    """Run-length encode an int array -> [(count, value), ...]."""
    values = np.asarray(values)
    if values.size == 0:
        return []
    change = np.nonzero(np.diff(values))[0] + 1
    starts = np.concatenate(([0], change))
    counts = np.diff(np.concatenate((starts, [values.size])))
    return [(int(c), int(values[s])) for c, s in zip(counts, starts)]
