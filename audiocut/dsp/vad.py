"""Hysteresis VAD state machine over per-frame RMS dB.

Speech starts when energy crosses the (higher) enter threshold and ends
only after energy stays below the (lower) exit threshold for at least
min_silence frames, so gaps shorter than min_silence are bridged and
the thresholds never chatter on the noise floor. Segments shorter than
min_speech frames are dropped.
"""
from __future__ import annotations

import numpy as np


def detect_segments(rms_db: np.ndarray, enter_db: float, exit_db: float,
                    min_speech_frames: int,
                    min_silence_frames: int) -> list[tuple[int, int]]:
    """Return coarse (start_frame, end_frame_exclusive) segment list."""
    segs: list[tuple[int, int]] = []
    in_speech = False
    start = 0
    below = 0
    last_above = 0
    for i in range(rms_db.shape[0]):
        level = rms_db[i]
        if level >= exit_db:
            last_above = i
        if not in_speech:
            if level >= enter_db:
                in_speech = True
                start = i
                below = 0
        elif level < exit_db:
            below += 1
            if below >= min_silence_frames:
                segs.append((start, last_above + 1))
                in_speech = False
                below = 0
        else:
            below = 0
    if in_speech:
        segs.append((start, last_above + 1))
    return [(s, e) for s, e in segs if e - s >= min_speech_frames]
