"""Sample-level boundary refinement with fade-in/fade-out protection."""
from __future__ import annotations

import numpy as np

from .detector import Segment

NOISE_HEADROOM_DB = 3.0
MAX_FADE_MS = 2000.0


def _rms_envelope(chunk, win):
    count = chunk.shape[0] // win
    if count == 0:
        if chunk.shape[0] == 0:
            return np.zeros(0, dtype=np.float64)
        return np.array([float(np.sqrt(np.mean(chunk.astype(np.float64) ** 2)))])
    grid = chunk[:count * win].reshape(count, win).astype(np.float64)
    return np.sqrt(np.mean(grid * grid, axis=1))


def _last_quiet_end(env, level, guard, pos):
    quiet = env[:pos] < level
    if quiet.size < guard:
        return None
    sums = np.concatenate(([0], np.cumsum(quiet, dtype=np.int64)))
    hits = np.flatnonzero((sums[guard:] - sums[:-guard]) == guard) + guard
    hits = hits[hits <= pos]
    return int(hits[-1]) if hits.size else None


def _first_quiet_start(env, level, guard, pos):
    quiet = env[pos:] < level
    if quiet.size < guard:
        return None
    sums = np.concatenate(([0], np.cumsum(quiet, dtype=np.int64)))
    hits = np.flatnonzero((sums[guard:] - sums[:-guard]) == guard)
    return pos + int(hits[0]) if hits.size else None


def _refine_start(samples, start, lo, env_enter, env_noise, guard, win, max_scan):
    wlo = max(lo, start - max_scan)
    whi = min(samples.shape[0], start + (guard + 8) * win)
    env = _rms_envelope(samples[wlo:whi], win)
    rel = min((start - wlo) // win, env.shape[0])
    above = np.flatnonzero(env[rel:] >= env_enter)
    cross = rel + int(above[0]) if above.size else rel
    quiet_end = _last_quiet_end(env, env_noise, guard, cross)
    refined = wlo + (quiet_end if quiet_end is not None else 0) * win
    refined = max(refined, lo)
    fade_in = max(0, (wlo + cross * win) - refined)
    return refined, fade_in


def _refine_end(samples, end, hi, env_enter, env_noise, guard, win, max_scan):
    wlo = max(0, end - max_scan)
    whi = min(hi, end + max_scan)
    env = _rms_envelope(samples[wlo:whi], win)
    above = np.flatnonzero(env >= env_enter)
    if above.size == 0:
        return min(end, hi), 0
    cross = int(above[-1])
    quiet_start = _first_quiet_start(env, env_noise, guard, cross + 1)
    if quiet_start is None:
        return min(end, hi), 0
    refined = min(wlo + quiet_start * win, hi)
    fade_out = max(0, refined - (wlo + cross * win))
    return refined, fade_out


def refine_segments(samples, sample_rate, candidates, noise_floor_db,
                    enter_db, fade_guard_ms=30.0):
    """Snap frame-level candidates to sample-level energy boundaries.

    Fade-in is included in full (back to the noise bed); fade-out is
    tracked until the envelope settles at the noise floor.
    """
    win = max(1, sample_rate // 1000)
    guard = max(1, int(round(fade_guard_ms)))
    max_scan = int(round(sample_rate * MAX_FADE_MS / 1000.0))
    env_enter = float(10.0 ** (enter_db / 20.0))
    env_noise = float(10.0 ** ((noise_floor_db + NOISE_HEADROOM_DB) / 20.0))
    total = samples.shape[0]
    refined = []
    prev_end = 0
    for index, cand in enumerate(candidates):
        if index + 1 < len(candidates):
            hi = candidates[index + 1].start_sample
        else:
            hi = total
        start, fade_in = _refine_start(
            samples, cand.start_sample, prev_end,
            env_enter, env_noise, guard, win, max_scan)
        end, fade_out = _refine_end(
            samples, cand.end_sample, hi,
            env_enter, env_noise, guard, win, max_scan)
        end = max(end, start + 1)
        refined.append(Segment(cand.ordinal, start, end, fade_in, fade_out))
        prev_end = end
    return refined
