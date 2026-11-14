"""Self-contained dual-threshold hysteresis segment detector."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np


@dataclass(frozen=True)
class Segment:
    start_sample: int
    end_sample: int


@dataclass(frozen=True)
class DetectorParams:
    sample_rate: int
    frame: int
    hop: int
    high_offset_db: float = 9.0    # enter speech above floor + this
    low_offset_db: float = 5.0     # leave speech below floor + this
    fade_offset_db: float = 6.0    # fade boundary: floor + this
    fade_limit_ms: float = 150.0   # max rollback / extension
    min_silence_ms: float = 180.0
    min_segment_ms: float = 120.0
    keep_peak_db: float = 18.0     # short segment kept if peak >= floor + this


def _hysteresis(env: np.ndarray, high: np.ndarray, low: np.ndarray):
    segs, start = [], None
    for i, db in enumerate(env):
        if start is None:
            if db >= high[i]:
                start = i
        elif db < low[i]:
            segs.append([start, i])
            start = None
    if start is not None:
        segs.append([start, env.size])
    return segs


def _merge_close(segs, min_gap_frames):
    merged = []
    for seg in segs:
        if merged and seg[0] - merged[-1][1] < min_gap_frames:
            merged[-1][1] = seg[1]
        else:
            merged.append(list(seg))
    return merged


def _refine_fades(env, floor, segs, fade_off, limit_frames):
    out = []
    for idx, (s, e) in enumerate(segs):
        lo_bound = segs[idx - 1][1] if idx else 0
        hi_bound = segs[idx + 1][0] if idx + 1 < len(segs) else env.size
        ns = s
        while ns - 1 >= max(lo_bound, s - limit_frames) and env[ns - 1] >= floor[ns - 1] + fade_off:
            ns -= 1
        ne = e
        while ne < min(hi_bound, e + limit_frames) and env[ne] >= floor[ne] + fade_off:
            ne += 1
        out.append([ns, ne])
    return out


def detect(env_db: np.ndarray, floor_db: np.ndarray, params: DetectorParams,
           total_samples: int, tail_db: float | None = None,
           tail_samples: int = 0):
    """Return (segments, silences) in sample units, ordered and non-overlapping."""
    if env_db.size == 0:
        return [], []
    high = floor_db + params.high_offset_db
    low = floor_db + params.low_offset_db
    segs = _hysteresis(env_db, high, low)
    limit = int(round(params.fade_limit_ms * params.sample_rate / 1000.0 / params.hop))
    segs = _refine_fades(env_db, floor_db, segs, params.fade_offset_db, limit)
    min_gap = int(round(params.min_silence_ms * params.sample_rate / 1000.0 / params.hop))
    segs = _merge_close(segs, max(1, min_gap))
    min_len = int(round(params.min_segment_ms * params.sample_rate / 1000.0 / params.hop))
    kept = []
    for s, e in segs:
        if e - s >= min_len:
            kept.append((s, e))
        elif env_db[s:e].size and float(env_db[s:e].max() - floor_db[s:e].max()) >= params.keep_peak_db:
            kept.append((s, e))  # real short syllable: strong peak survives debounce
    segments = []
    for s, e in kept:
        start = s * params.hop
        end = min(e * params.hop + params.frame, total_samples)
        segments.append(Segment(start, max(end, start + 1)))
    silences = []
    prev = 0
    for seg in segments:
        if seg.start_sample > prev:
            silences.append(Segment(prev, seg.start_sample))
        prev = seg.end_sample
    if prev < total_samples:
        silences.append(Segment(prev, total_samples))
    return segments, silences
