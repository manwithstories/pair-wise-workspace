"""Noise-floor estimation and hysteresis event detection. Pure functions, no IO."""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np

ENTER_OFFSET_DB = 8.0
EXIT_OFFSET_DB = 5.0


@dataclass(frozen=True)
class Segment:
    ordinal: int
    start_sample: int
    end_sample: int
    fade_in_samples: int = 0
    fade_out_samples: int = 0

    @property
    def duration_samples(self):
        return self.end_sample - self.start_sample


@dataclass(frozen=True)
class Silence:
    ordinal: int
    start_sample: int
    end_sample: int

    @property
    def duration_samples(self):
        return self.end_sample - self.start_sample


def median_filter(values, kernel=5):
    if kernel <= 1 or values.shape[0] == 0:
        return values.astype(np.float64)
    kernel += (kernel + 1) % 2
    pad = kernel // 2
    padded = np.pad(values.astype(np.float64), pad, mode="edge")
    windows = np.lib.stride_tricks.sliding_window_view(padded, kernel)
    return np.median(windows, axis=1)


def estimate_noise_floor(frame_db, percentile=20.0):
    """Low percentile of the median-filtered frame envelope (6 dp)."""
    smoothed = median_filter(frame_db, kernel=5)
    return round(float(np.percentile(smoothed, percentile)), 6)


def _active_frames(frame_db, enter_db, exit_db):
    active = np.zeros(frame_db.shape[0], dtype=bool)
    inside = False
    for i, level in enumerate(frame_db):
        if inside:
            if level < exit_db:
                inside = False
        elif level >= enter_db:
            inside = True
        active[i] = inside
    return active


def _bool_runs(mask):
    idx = np.flatnonzero(mask)
    if idx.size == 0:
        return []
    splits = np.flatnonzero(np.diff(idx) > 1) + 1
    return [(int(g[0]), int(g[-1]) + 1) for g in np.split(idx, splits)]


def detect_events(frame_db, frame_len, total_samples, sample_rate,
                  min_event_ms=150.0, min_silence_ms=200.0,
                  noise_floor_db=None):
    """Hysteresis detection. Returns (segments, thresholds dict).

    Gaps shorter than ``min_silence_ms`` are merged into the surrounding
    event; events shorter than ``min_event_ms`` are dropped. A pause
    longer than ``min_silence_ms`` inside an event therefore splits it.
    """
    if noise_floor_db is None:
        noise_floor_db = estimate_noise_floor(frame_db)
    enter_db = round(noise_floor_db + ENTER_OFFSET_DB, 6)
    exit_db = round(noise_floor_db + EXIT_OFFSET_DB, 6)
    active = _active_frames(frame_db, enter_db, exit_db)
    runs = _bool_runs(active)
    frame_ms = frame_len / sample_rate * 1000.0
    min_silence_frames = max(1, int(round(min_silence_ms / frame_ms)))
    min_event_frames = max(1, int(round(min_event_ms / frame_ms)))
    merged = []
    for start, end in runs:
        if merged and start - merged[-1][1] < min_silence_frames:
            merged[-1][1] = end
        else:
            merged.append([start, end])
    segments = []
    for start, end in merged:
        if end - start < min_event_frames:
            continue
        segments.append(Segment(
            ordinal=len(segments) + 1,
            start_sample=start * frame_len,
            end_sample=min(end * frame_len, total_samples),
        ))
    thresholds = {
        "noise_floor_dbfs": noise_floor_db,
        "enter_threshold_dbfs": enter_db,
        "exit_threshold_dbfs": exit_db,
    }
    return segments, thresholds


def compute_silences(segments, total_samples):
    silences = []
    cursor = 0
    for seg in segments:
        if seg.start_sample > cursor:
            silences.append(Silence(len(silences) + 1, cursor, seg.start_sample))
        cursor = max(cursor, seg.end_sample)
    if cursor < total_samples:
        silences.append(Silence(len(silences) + 1, cursor, total_samples))
    return silences


def check_invariants(segments, silences, total_samples):
    """Verify the segment/silence tiling. Returns a list of problems."""
    problems = []
    intervals = (
        [("segment", s.start_sample, s.end_sample) for s in segments]
        + [("silence", s.start_sample, s.end_sample) for s in silences]
    )
    intervals.sort(key=lambda item: item[1])
    cursor = 0
    prev_kind = None
    for kind, start, end in intervals:
        if end <= start:
            problems.append(f"non-positive {kind} length at {start}")
        if start != cursor:
            relation = "overlap" if start < cursor else "gap"
            problems.append(f"{relation} before {kind} at {start}")
        if kind == prev_kind:
            problems.append(f"adjacent {kind} intervals at {start}")
        cursor = end
        prev_kind = kind
    if intervals and intervals[0][1] != 0:
        problems.append("intervals do not start at sample 0")
    if cursor != total_samples:
        problems.append(f"intervals end at {cursor}, expected {total_samples}")
    seg_total = sum(s.duration_samples for s in segments)
    sil_total = sum(s.duration_samples for s in silences)
    if seg_total + sil_total != total_samples:
        problems.append(
            f"sample conservation failed: {seg_total}+{sil_total}!={total_samples}")
    return problems
