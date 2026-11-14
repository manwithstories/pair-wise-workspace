"""Noise-floor estimation from a low-percentile frame-energy histogram.

The floor is the top of the lowest significant low-energy histogram
region (clusters closer than 6 dB are treated as one drifting body), so
it follows a noise floor that drifts mid-recording and stays correct
even when speech frames outnumber quiet ones. A percentile guard keeps
long quiet head/tail stretches from dragging the floor - and therefore
the thresholds - unrealistically low. Exports separate enter/exit
thresholds (enter above exit) for the hysteresis VAD.
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np


@dataclass
class NoiseProfile:
    noise_floor_db: float
    enter_db: float
    exit_db: float


def estimate_noise(rms_db: np.ndarray,
                   enter_offset_db: float = 10.0,
                   exit_offset_db: float = 6.0,
                   min_hysteresis_db: float = 3.0) -> NoiseProfile:
    db = rms_db[np.isfinite(rms_db)].astype(np.float64)
    if db.size == 0:
        return NoiseProfile(-60.0, -50.0, -54.0)
    lo, hi, bins = -90.0, 0.0, 180  # 0.5 dB bins
    hist, edges = np.histogram(np.clip(db, lo, hi - 1e-9), bins=bins,
                               range=(lo, hi))
    smooth = np.convolve(hist.astype(np.float64), np.ones(5) / 5.0,
                         mode="same")
    centers = (edges[:-1] + edges[1:]) / 2.0
    median = float(np.median(db))
    # Noise region: significant clusters (>= ~1% of frames) reachable
    # from the lowest one, tolerating gaps up to 16 dB and a total span
    # up to 20 dB (the studio-to-studio range), so drift and quieter
    # head/tail stretches count as one noise body while a separated
    # speech body does not.
    significant = (smooth >= 0.01 * db.size) & (centers <= median)
    idx = np.flatnonzero(significant)
    if idx.size:
        max_gap = 32    # bins, i.e. 16 dB between clusters
        max_span = 40   # bins, i.e. 20 dB total region width
        runs: list[tuple[int, int]] = []
        run_start = prev = int(idx[0])
        for i in idx[1:]:
            i = int(i)
            if i - prev > max_gap or i - run_start > max_span:
                break
            if i - prev > 1:
                runs.append((run_start, prev))
                run_start = i
            prev = i
        runs.append((run_start, prev))
        top_start, top_end = runs[-1]
        peak = top_start + int(np.argmax(smooth[top_start:top_end + 1]))
        floor = float(centers[peak])
    else:
        floor = float(np.percentile(db, 20.0))
    # Guard: long quiet head/tail must not pull the floor far below the
    # main noise body; the 25th percentile bounds how low it may sit.
    p25 = float(np.percentile(db, 25.0))
    floor = max(floor, p25 - 12.0)
    exit_db = floor + exit_offset_db
    enter_db = floor + enter_offset_db
    if enter_db - exit_db < min_hysteresis_db:
        enter_db = exit_db + min_hysteresis_db
    return NoiseProfile(floor, enter_db, exit_db)
