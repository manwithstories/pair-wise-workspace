"""Pillow-backed image decoding with deterministic RGBA premultiplication."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import numpy as np
from PIL import Image, UnidentifiedImageError

from .errors import AssetError


def to_premultiplied(rgba: np.ndarray) -> np.ndarray:
    """Convert uint8 straight RGBA to float32 premultiplied RGBA in [0, 1]."""
    if rgba.ndim != 3 or rgba.shape[2] != 4 or rgba.dtype != np.uint8:
        raise ValueError("rgba must be an HxWx4 uint8 array")
    result = np.empty(rgba.shape, dtype=np.float32)
    alpha = rgba[..., 3].astype(np.float32) / np.float32(255.0)
    result[..., :3] = (
        rgba[..., :3].astype(np.float32) / np.float32(255.0)
    ) * alpha[..., None]
    result[..., 3] = alpha
    return result


def resize_premultiplied(image: np.ndarray, size: tuple[int, int]) -> np.ndarray:
    """Resize a premultiplied float image without introducing alpha fringes."""
    height, width = image.shape[:2]
    if size == (width, height):
        return image.copy()
    if image.dtype != np.float32 or image.ndim != 3 or image.shape[2] != 4:
        raise ValueError("image must be an HxWx4 float32 array")
    straight = np.empty(image.shape, dtype=np.uint8)
    alpha = np.clip(image[..., 3], 0.0, 1.0)
    safe = np.divide(
        image[..., :3],
        alpha[..., None],
        out=np.zeros_like(image[..., :3]),
        where=alpha[..., None] > 0,
    )
    straight[..., :3] = _round_u8(safe)
    straight[..., 3] = _round_u8(alpha)
    pil = Image.fromarray(straight, "RGBA")
    resized = pil.resize(size, Image.Resampling.BILINEAR)
    return to_premultiplied(np.asarray(resized, dtype=np.uint8).copy())


def _round_u8(values: np.ndarray) -> np.ndarray:
    return np.clip(np.floor(values * np.float32(255.0) + np.float32(0.5)), 0, 255).astype(
        np.uint8
    )


@dataclass(frozen=True)
class RasterImage:
    width: int
    height: int
    premultiplied: np.ndarray


class RasterCache:
    """Per-process decoded-image cache; multiprocessing workers never share arrays."""

    def __init__(self) -> None:
        self._images: dict[str, RasterImage] = {}

    def get(self, path: str | Path) -> RasterImage:
        key = str(Path(path).expanduser().resolve())
        cached = self._images.get(key)
        if cached is not None:
            return cached
        try:
            with Image.open(key) as source:
                source.load()
                rgba = np.asarray(source.convert("RGBA"), dtype=np.uint8).copy()
        except (OSError, ValueError, UnidentifiedImageError) as exc:
            raise AssetError(f"cannot decode image {key}: {exc}") from exc
        if rgba.ndim != 3 or rgba.shape[2] != 4 or rgba.shape[0] == 0 or rgba.shape[1] == 0:
            raise AssetError(f"image has no usable RGBA pixels: {key}")
        image = RasterImage(rgba.shape[1], rgba.shape[0], to_premultiplied(rgba))
        self._images[key] = image
        return image

    def clear(self) -> None:
        self._images.clear()
