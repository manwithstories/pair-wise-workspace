"""PSNR quality measurement against deterministic or decoded references."""

from __future__ import annotations

import av
import numpy as np


def psnr(reference: np.ndarray, coded: np.ndarray) -> float:
    """Mean PSNR across RGB planes; ``inf`` for an exact match."""
    diff = reference.astype(np.float64) - coded.astype(np.float64)
    mse = float(np.mean(diff ** 2))
    if mse <= 1e-12:
        return float("inf")
    return float(10.0 * np.log10((255.0 ** 2) / mse))


def psnr_against_encoded(path: str,
                         source_frames: list[np.ndarray]) -> list[float]:
    """Decode a written segment and score each frame vs its source frame.

    Used for real (non-synthetic) media where no generator exists: the just
    encoded output is decoded and compared to the source frames that fed it.
    """
    try:
        container = av.open(path)
    except av.AVError:
        return []
    decoded: list[np.ndarray] = []
    try:
        for pkt in container.demux():
            if pkt.stream.type != "video":
                continue
            for frame in pkt.decode():
                decoded.append(frame.to_ndarray(format="rgb24"))
    except av.AVError:
        pass
    finally:
        container.close()
    return [psnr(src, coded) for src, coded in zip(source_frames, decoded)
            if src.shape == coded.shape]


def mean_psnr_db(values: list[float], cap_db: float = 60.0) -> float:
    """Average PSNR with infinite (lossless) values clipped to a finite cap."""
    return round(float(np.mean([min(v, cap_db) for v in values])), 2)
