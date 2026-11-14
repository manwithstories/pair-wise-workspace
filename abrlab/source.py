"""Deterministic synthetic frame generation and resampling."""

from __future__ import annotations

from functools import lru_cache
from typing import Any

import numpy as np


def _bilinear_axis(size: int, target: int) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    if target == 1:
        positions = np.zeros(1, dtype=np.float64)
    else:
        positions = (np.arange(target, dtype=np.float64) + 0.5) * size / target - 0.5
    positions = np.clip(positions, 0.0, size - 1.0)
    lower = np.floor(positions).astype(np.int64)
    upper = np.minimum(lower + 1, size - 1)
    weight = (positions - lower).astype(np.float32)
    return lower, upper, weight


def resize_rgb(frame: np.ndarray, width: int, height: int) -> np.ndarray:
    src_h, src_w = frame.shape[:2]
    if (src_w, src_h) == (width, height):
        return frame.astype(np.uint8, copy=False)
    y0, y1, wy = _bilinear_axis(src_h, height)
    x0, x1, wx = _bilinear_axis(src_w, width)
    top = frame[y0][:, x0].astype(np.float32)
    top = top * (1.0 - wx)[None, :, None] + frame[y0][:, x1].astype(np.float32) * wx[None, :, None]
    bottom = frame[y1][:, x0].astype(np.float32)
    bottom = bottom * (1.0 - wx)[None, :, None] + frame[y1][:, x1].astype(np.float32) * wx[None, :, None]
    out = top * (1.0 - wy)[:, None, None] + bottom * wy[:, None, None]
    return np.clip(np.rint(out), 0, 255).astype(np.uint8)


@lru_cache(maxsize=4)
def _base_sequence(seed: int, frames: int, width: int, height: int, pattern: str) -> np.ndarray:
    if pattern == "constant":
        value = 64 + seed % 128
        frame = np.full((height, width, 3), value, dtype=np.uint8)
        frame[:, :, 1] = np.clip(value + 18, 0, 255)
        frame[:, :, 2] = np.clip(value - 12, 0, 255)
        return np.repeat(frame[None, :, :, :], frames, axis=0)

    rng = np.random.default_rng(seed)
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float32)
    xn = xx / max(width - 1, 1)
    yn = yy / max(height - 1, 1)
    base = np.empty((frames, height, width, 3), dtype=np.uint8)
    coarse = rng.normal(0.0, 1.0, size=(height // 8 + 2, width // 8 + 2, 3)).astype(np.float32)
    for index in range(frames):
        phase = 2.0 * np.pi * index / max(frames, 1)
        red = 118.0 + 92.0 * np.sin(2.2 * np.pi * xn + phase) * (0.35 + 0.65 * yn)
        green = 104.0 + 78.0 * np.cos(1.7 * np.pi * yn - phase * 0.7)
        blue = 96.0 + 68.0 * np.sin(1.3 * np.pi * (xn + yn) + phase * 1.1)
        texture = np.repeat(np.repeat(coarse, 8, axis=0), 8, axis=1)[:height, :width]
        rgb = np.stack((red, green, blue), axis=-1)
        rgb += texture * 7.0
        stripe = ((xx.astype(np.int32) + 3 * yy.astype(np.int32) + index * 5) // 12) % 2
        rgb += stripe[:, :, None] * 10.0
        cx = 0.5 + 0.28 * np.sin(phase)
        cy = 0.5 + 0.18 * np.cos(phase * 1.3)
        box = ((np.abs(xn - cx) < 0.10) & (np.abs(yn - cy) < 0.15)).astype(np.float32)
        rgb += box[:, :, None] * np.array([35.0, -22.0, 30.0], dtype=np.float32)
        noise = rng.normal(0.0, 2.2, size=rgb.shape).astype(np.float32)
        base[index] = np.clip(np.rint(rgb + noise), 0, 255).astype(np.uint8)
    return base


def generate_sequence(source: dict[str, Any], chroma_mode: str) -> np.ndarray:
    base = _base_sequence(
        int(source["seed"]),
        int(source["frames"]),
        int(source["base_width"]),
        int(source["base_height"]),
        str(source.get("pattern", "mixed")),
    )
    width = int(source["width"])
    height = int(source["height"])
    if base.shape[1:3] != (height, width):
        base = np.stack([resize_rgb(frame, width, height) for frame in base], axis=0)
    if chroma_mode == "gray" or source.get("pattern") == "gray":
        base = _to_gray(base)
    return np.ascontiguousarray(base)


def _to_gray(frames: np.ndarray) -> np.ndarray:
    gray = np.rint(
        0.299 * frames[:, :, :, 0] + 0.587 * frames[:, :, :, 1] + 0.114 * frames[:, :, :, 2]
    ).astype(np.uint8)
    return np.repeat(gray[:, :, :, None], 3, axis=3)


def generate_reference(source: dict[str, Any], chroma_mode: str) -> np.ndarray:
    reference = _base_sequence(
        int(source["seed"]),
        int(source["frames"]),
        int(source["base_width"]),
        int(source["base_height"]),
        str(source.get("pattern", "mixed")),
    )
    if chroma_mode == "gray" or source.get("pattern") == "gray":
        reference = _to_gray(reference)
    return reference


def resize_sequence(frames: np.ndarray, width: int, height: int) -> np.ndarray:
    if frames.shape[1:3] == (height, width):
        return frames
    return np.stack([resize_rgb(frame, width, height) for frame in frames], axis=0)
