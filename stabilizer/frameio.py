"""Frame and trajectory I/O plus the shared bilinear sampler.

Frames are stored as one ``.npy`` grayscale array per frame
(``frame_00000.npy`` ...). Trajectories and metrics are JSON.
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from . import InputError

FRAME_GLOB = "frame_*.npy"


def save_frames(frames: np.ndarray, directory: str | Path) -> list[Path]:
    """Write an (N, H, W) uint8 array as one .npy file per frame."""
    out = Path(directory)
    out.mkdir(parents=True, exist_ok=True)
    paths = []
    for i, frame in enumerate(frames):
        p = out / f"frame_{i:05d}.npy"
        np.save(p, np.asarray(frame, dtype=np.uint8))
        paths.append(p)
    return paths


def frame_paths(directory: str | Path) -> list[Path]:
    paths = sorted(Path(directory).glob(FRAME_GLOB))
    if not paths:
        raise InputError(f"no frames matching {FRAME_GLOB} in {directory}",
                         {"directory": str(directory)})
    return paths


def load_frames(directory: str | Path) -> np.ndarray:
    """Load all frames, validating shape consistency and NaN content."""
    paths = frame_paths(directory)
    frames = [np.load(p) for p in paths]
    shape = frames[0].shape
    if len(shape) != 2:
        raise InputError("frames must be 2-D grayscale",
                         {"frame": paths[0].name, "shape": list(shape)})
    for p, f in zip(paths, frames):
        if f.shape != shape:
            raise InputError("inconsistent frame sizes",
                             {"frame": p.name, "shape": list(f.shape),
                              "expected": list(shape)})
        if np.issubdtype(f.dtype, np.floating) and np.isnan(f).any():
            raise InputError("frame contains NaN", {"frame": p.name})
    return np.stack(frames)


def bilinear_sample(img: np.ndarray, ys: np.ndarray, xs: np.ndarray,
                    fill: float = 0.0) -> np.ndarray:
    """Sample ``img`` at float coordinates (ys, xs); out-of-bounds -> fill."""
    h, w = img.shape
    ys = np.asarray(ys, dtype=np.float64)
    xs = np.asarray(xs, dtype=np.float64)
    y0 = np.floor(ys).astype(np.int64)
    x0 = np.floor(xs).astype(np.int64)
    fy = ys - y0
    fx = xs - x0
    valid = (y0 >= 0) & (y0 < h - 1) & (x0 >= 0) & (x0 < w - 1)
    y0c = np.clip(y0, 0, h - 2)
    x0c = np.clip(x0, 0, w - 2)
    imgf = img.astype(np.float64)
    ia = imgf[y0c, x0c]
    ib = imgf[y0c, x0c + 1]
    ic = imgf[y0c + 1, x0c]
    id_ = imgf[y0c + 1, x0c + 1]
    out = (ia * (1 - fx) * (1 - fy) + ib * fx * (1 - fy)
           + ic * (1 - fx) * fy + id_ * fx * fy)
    return np.where(valid, out, fill)


def save_json(data: dict, path: str | Path) -> Path:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        json.dump(data, fh, indent=1, sort_keys=True)
    return path


def load_json(path: str | Path) -> dict:
    path = Path(path)
    if not path.exists():
        raise InputError(f"missing JSON file: {path}", {"path": str(path)})
    try:
        with path.open("r", encoding="utf-8") as fh:
            return json.load(fh)
    except json.JSONDecodeError as exc:
        raise InputError(f"invalid JSON: {path}", {"error": str(exc)}) from exc
