"""Alpha-weighted source-over compositing for five blend modes."""

from __future__ import annotations

import numpy as np

BLEND_MODES = ("normal", "multiply", "screen", "overlay", "add")


def _unpremultiply(rgba: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    alpha = np.clip(rgba[..., 3], 0.0, 1.0)
    rgb = np.divide(
        rgba[..., :3],
        alpha[..., None],
        out=np.zeros_like(rgba[..., :3]),
        where=alpha[..., None] > 0,
    )
    return np.clip(rgb, 0.0, 1.0), alpha


def _blend_color(mode: str, source: np.ndarray, backdrop: np.ndarray) -> np.ndarray:
    if mode == "normal":
        return source
    if mode == "multiply":
        return source * backdrop
    if mode == "screen":
        return source + backdrop - source * backdrop
    if mode == "overlay":
        low = 2.0 * source * backdrop
        high = 1.0 - 2.0 * (1.0 - source) * (1.0 - backdrop)
        return np.where(backdrop <= 0.5, low, high)
    if mode == "add":
        return np.minimum(source + backdrop, 1.0)
    raise ValueError(f"unknown blend mode: {mode}")


def blend_premultiplied(
    backdrop: np.ndarray,
    source: np.ndarray,
    mode: str = "normal",
    opacity: float = 1.0,
) -> np.ndarray:
    """Composite premultiplied source over premultiplied backdrop.

    Both arrays are HxWx4 float32 in [0, 1]. The mode color is weighted by
    source alpha and backdrop alpha; output alpha is source-over alpha.
    """
    if mode not in BLEND_MODES:
        raise ValueError(f"unknown blend mode: {mode}")
    if backdrop.shape != source.shape or backdrop.shape[-1] != 4:
        raise ValueError("backdrop and source must have the same HxWx4 shape")
    opacity = float(np.clip(opacity, 0.0, 1.0))
    if mode == "normal":
        source_alpha = source[..., 3] * np.float32(opacity)
        result = np.empty_like(backdrop)
        result[..., :3] = (
            source[..., :3] * np.float32(opacity)
            + backdrop[..., :3] * (1.0 - source_alpha[..., None])
        )
        result[..., 3] = source_alpha + backdrop[..., 3] * (1.0 - source_alpha)
        return np.clip(np.nan_to_num(result), 0.0, 1.0)
    src_rgb, src_alpha = _unpremultiply(source)
    dst_rgb, dst_alpha = _unpremultiply(backdrop)
    src_alpha = src_alpha * np.float32(opacity)
    blended = _blend_color(mode, src_rgb, dst_rgb)
    out_alpha = src_alpha + dst_alpha * (1.0 - src_alpha)
    out_rgb = (
        src_alpha[..., None]
        * ((1.0 - dst_alpha[..., None]) * src_rgb + dst_alpha[..., None] * blended)
        + (1.0 - src_alpha[..., None]) * backdrop[..., :3]
    )
    out_rgb = np.nan_to_num(out_rgb, nan=0.0, posinf=1.0, neginf=0.0)
    out_alpha = np.nan_to_num(out_alpha, nan=0.0, posinf=1.0, neginf=0.0)
    result = np.empty_like(backdrop)
    result[..., :3] = np.clip(out_rgb, 0.0, 1.0)
    result[..., 3] = np.clip(out_alpha, 0.0, 1.0)
    return result


def blend_into(
    backdrop: np.ndarray,
    source: np.ndarray,
    mode: str = "normal",
    opacity: float = 1.0,
) -> None:
    """In-place variant used by the compositor."""
    backdrop[...] = blend_premultiplied(backdrop, source, mode, opacity)
