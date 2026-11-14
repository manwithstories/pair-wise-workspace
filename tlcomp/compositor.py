"""Deterministic inverse-mapped layer compositing and RGBA frame output."""

from __future__ import annotations

import math

import numpy as np

from . import blend
from .raster import RasterCache, RasterImage, to_premultiplied
from .timeline import Layer, LayerState, Timeline


def _background_layer(color: tuple[int, int, int, int], width: int, height: int) -> np.ndarray:
    alpha = np.float32(color[3]) / np.float32(255.0)
    canvas = np.empty((height, width, 4), dtype=np.float32)
    canvas[..., :3] = np.asarray(color[:3], dtype=np.float32) / np.float32(255.0) * alpha
    canvas[..., 3] = alpha
    return canvas


def _destination_bounds(
    layer: Layer,
    state: LayerState,
    image: RasterImage,
    width: int,
    height: int,
) -> tuple[int, int, int, int] | None:
    anchor_x, anchor_y = layer.anchor or (image.width / 2.0, image.height / 2.0)
    scale_x, scale_y = state.scale
    radians = math.radians(state.rotation)
    cosine, sine = math.cos(radians), math.sin(radians)
    corners = ((0.0, 0.0), (image.width, 0.0), (image.width, image.height), (0.0, image.height))
    points: list[tuple[float, float]] = []
    for source_x, source_y in corners:
        local_x = (source_x - anchor_x) * scale_x
        local_y = (source_y - anchor_y) * scale_y
        points.append(
            (
                state.position[0] + cosine * local_x - sine * local_y,
                state.position[1] + sine * local_x + cosine * local_y,
            )
        )
    left = max(0, math.floor(min(point[0] for point in points)))
    top = max(0, math.floor(min(point[1] for point in points)))
    right = min(width, math.ceil(max(point[0] for point in points)))
    bottom = min(height, math.ceil(max(point[1] for point in points)))
    if left >= right or top >= bottom:
        return None
    return left, top, right, bottom


def _direct_source(layer: Layer, state: LayerState, image: RasterImage) -> np.ndarray | None:
    """Return a pixel-aligned source slice when no resampling is needed."""
    if abs(state.rotation) > 1e-12 or state.scale != (1.0, 1.0):
        return None
    anchor_x, anchor_y = layer.anchor or (image.width / 2.0, image.height / 2.0)
    offset_x = state.position[0] - anchor_x
    offset_y = state.position[1] - anchor_y
    if abs(offset_x - round(offset_x)) > 1e-9 or abs(offset_y - round(offset_y)) > 1e-9:
        return None
    return image.premultiplied


def _bilinear_sample(
    image: RasterImage,
    layer: Layer,
    state: LayerState,
    bounds: tuple[int, int, int, int],
) -> np.ndarray:
    left, top, right, bottom = bounds
    anchor_x, anchor_y = layer.anchor or (image.width / 2.0, image.height / 2.0)
    scale_x, scale_y = state.scale
    radians = math.radians(state.rotation)
    cosine, sine = math.cos(radians), math.sin(radians)
    xs = np.arange(left, right, dtype=np.float32) + np.float32(0.5)
    ys = np.arange(top, bottom, dtype=np.float32) + np.float32(0.5)
    delta_x = xs[None, :] - np.float32(state.position[0])
    delta_y = ys[:, None] - np.float32(state.position[1])
    local_x = cosine * delta_x + sine * delta_y
    local_y = -sine * delta_x + cosine * delta_y
    sample_x = np.float32(anchor_x) + local_x / np.float32(scale_x) - np.float32(0.5)
    sample_y = np.float32(anchor_y) + local_y / np.float32(scale_y) - np.float32(0.5)
    valid = (
        (sample_x >= 0.0)
        & (sample_x <= image.width - 1)
        & (sample_y >= 0.0)
        & (sample_y <= image.height - 1)
    )
    clipped_x = np.clip(sample_x, 0.0, image.width - 1)
    clipped_y = np.clip(sample_y, 0.0, image.height - 1)
    x0 = np.floor(clipped_x).astype(np.int32)
    y0 = np.floor(clipped_y).astype(np.int32)
    x1 = np.minimum(x0 + 1, image.width - 1)
    y1 = np.minimum(y0 + 1, image.height - 1)
    fx = (clipped_x - x0).astype(np.float32)[..., None]
    fy = (clipped_y - y0).astype(np.float32)[..., None]
    source = image.premultiplied
    top_row = source[y0, x0] * (1.0 - fx) + source[y0, x1] * fx
    bottom_row = source[y1, x0] * (1.0 - fx) + source[y1, x1] * fx
    sampled = top_row * (1.0 - fy) + bottom_row * fy
    sampled *= valid[..., None]
    return sampled.astype(np.float32, copy=False)


def _composite_layer(canvas: np.ndarray, layer: Layer, state: LayerState, image: RasterImage) -> None:
    if state.opacity <= 0.0:
        return
    bounds = _destination_bounds(layer, state, image, canvas.shape[1], canvas.shape[0])
    if bounds is None:
        return
    left, top, right, bottom = bounds
    source = _direct_source(layer, state, image)
    if source is not None:
        anchor_x, anchor_y = layer.anchor or (image.width / 2.0, image.height / 2.0)
        source_left = left - int(round(state.position[0] - anchor_x))
        source_top = top - int(round(state.position[1] - anchor_y))
        crop_right = min(source.shape[1], source_left + (right - left))
        crop_bottom = min(source.shape[0], source_top + (bottom - top))
        if source_left >= crop_right or source_top >= crop_bottom:
            return
        sample = source[source_top:crop_bottom, source_left:crop_right]
        target_left = left + max(0, -source_left)
        target_top = top + max(0, -source_top)
        region = canvas[target_top : target_top + sample.shape[0], target_left : target_left + sample.shape[1]]
    else:
        sample = _bilinear_sample(image, layer, state, bounds)
        region = canvas[top:bottom, left:right]
    blend.blend_into(region, sample, layer.blend, state.opacity)


def premultiplied_to_rgba8(image: np.ndarray) -> np.ndarray:
    alpha = np.clip(image[..., 3], 0.0, 1.0)
    rgb = np.divide(
        image[..., :3],
        alpha[..., None],
        out=np.zeros_like(image[..., :3]),
        where=alpha[..., None] > 0,
    )
    rgba = np.empty(image.shape, dtype=np.uint8)
    rgba[..., :3] = np.clip(np.floor(rgb * 255.0 + 0.5), 0, 255).astype(np.uint8)
    rgba[..., 3] = np.clip(np.floor(alpha * 255.0 + 0.5), 0, 255).astype(np.uint8)
    return rgba


class Compositor:
    """Stateful frame renderer; the raster cache is local to one process."""

    def __init__(self, timeline: Timeline, cache: RasterCache | None = None) -> None:
        self.timeline = timeline
        self.cache = cache or RasterCache()

    def render_premultiplied(self, frame: int) -> np.ndarray:
        if not 0 <= frame < self.timeline.frame_count:
            raise ValueError(f"frame out of range: {frame}")
        canvas = _background_layer(self.timeline.background, self.timeline.width, self.timeline.height)
        for layer in self.timeline.active_layers(frame):
            image = self.cache.get(layer.path)
            _composite_layer(canvas, layer, layer.state_at(frame), image)
        return canvas

    def render(self, frame: int) -> np.ndarray:
        return premultiplied_to_rgba8(self.render_premultiplied(frame))
