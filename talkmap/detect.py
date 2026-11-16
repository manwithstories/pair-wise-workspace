"""Dual-threshold hysteresis labelling implemented with NumPy primitives."""

from __future__ import annotations

import numpy as np


def label_frames(frame_db: np.ndarray, noise_db: np.ndarray, params) -> np.ndarray:
    """Return 1 for speech frames and 0 for silence frames.

    Entering speech needs the level to reach ``noise + enter_margin_db``; while
    already speaking the level must fall below ``noise + exit_margin_db`` before
    the frame is released back to silence.
    """
    frame_db = np.asarray(frame_db, dtype=np.float64)
    noise_db = np.asarray(noise_db, dtype=np.float64)
    enter = noise_db + params.enter_margin_db
    release = noise_db + params.exit_margin_db
    labels = np.zeros(frame_db.size, dtype=np.int8)
    speaking = False
    for idx in range(frame_db.size):
        if not speaking and frame_db[idx] >= enter[idx]:
            speaking = True
        elif speaking and frame_db[idx] < release[idx]:
            speaking = False
        labels[idx] = 1 if speaking else 0
    return labels


def runs(labels: np.ndarray) -> list[tuple[int, int, int]]:
    """Compress a label array into (start_frame, end_frame_exclusive, label)."""
    labels = np.asarray(labels)
    if labels.size == 0:
        return []
    change = np.flatnonzero(np.diff(labels.astype(np.int8))) + 1
    starts = np.concatenate(([0], change))
    ends = np.concatenate((change, [labels.size]))
    return [(int(s), int(e), int(labels[s])) for s, e in zip(starts, ends)]
