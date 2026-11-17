"""Stabilized frame export.

Reuses the tracking/motion/smoothing results produced upstream — this
module only warps: out(x') = input(P_k^-1 S_k x' + o_k(row)), where
P_k is the raw cumulative pose, S_k the smoothed pose, and o_k the
rolling-shutter per-row offset. No motion estimation happens here.
"""

from __future__ import annotations

import numpy as np

from .frameio import bilinear_sample


def stabilize_frame(frame: np.ndarray, raw_pose: np.ndarray,
                    smooth_pose: np.ndarray,
                    row_offsets: np.ndarray | None = None,
                    fill: float = 0.0):
    """Warp one frame onto the smoothed trajectory.

    Returns (stabilized float64 frame, valid mask).
    """
    h, w = frame.shape
    m = np.linalg.inv(raw_pose) @ smooth_pose  # maps out coords -> in coords
    yy, xx = np.mgrid[0:h, 0:w].astype(np.float64)
    qx = m[0, 0] * xx + m[0, 1] * yy + m[0, 2]
    qy = m[1, 0] * xx + m[1, 1] * yy + m[1, 2]
    if row_offsets is not None:
        rows = np.clip(np.round(qy).astype(int), 0, h - 1)
        qx = qx + row_offsets[rows, 0]
        qy = qy + row_offsets[rows, 1]
    valid = (qx >= 0) & (qx <= w - 1) & (qy >= 0) & (qy <= h - 1)
    out = bilinear_sample(frame, qy, qx, fill=fill)
    return out, valid


def stabilize_frames(frames: np.ndarray, raw_poses: list[np.ndarray],
                     smooth_poses: list[np.ndarray],
                     row_offsets: list[np.ndarray] | None = None,
                     fill: float = 0.0):
    """Warp the whole sequence. Returns (frames uint8, valid mask, info)."""
    n = len(frames)
    out = np.empty_like(frames)
    valid_all = np.empty(frames.shape, dtype=bool)
    for k in range(n):
        ro = row_offsets[k] if row_offsets is not None else None
        warped, valid = stabilize_frame(frames[k], raw_poses[k],
                                        smooth_poses[k], ro, fill=fill)
        out[k] = np.clip(np.round(warped), 0, 255).astype(np.uint8)
        valid_all[k] = valid
    info = {"valid_fraction": float(valid_all.mean()) if n else 0.0}
    return out, valid_all, info


def valid_region(valid_all: np.ndarray) -> dict:
    """Largest centered rectangle valid across ALL frames (crop hint)."""
    if not len(valid_all):
        return {"top": 0, "bottom": 0, "left": 0, "right": 0}
    common = valid_all.all(axis=0)
    rows = np.where(common.all(axis=1))[0]
    cols = np.where(common.all(axis=0))[0]
    h, w = valid_all.shape[1:]
    if not len(rows) or not len(cols):
        return {"top": 0, "bottom": h, "left": 0, "right": w}
    return {"top": int(rows[0]), "bottom": int(rows[-1]) + 1,
            "left": int(cols[0]), "right": int(cols[-1]) + 1}
