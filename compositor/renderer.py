"""帧渲染器: 素材物化 -> 几何变换 -> 裁剪 -> 混合。

内存模型: 每个素材只物化一次并缓存, 渲染期同时只持有一张画布
(float64, 大小 = 宽 x 高 x 4), 单帧内存随画布像素线性增长。
渲染器是纯计算模块, 不做任何磁盘 IO, 由 exporter 复用。
"""

from __future__ import annotations

from collections.abc import Iterator

import numpy as np
from PIL import Image

from .blend import composite_layer, quantize_rgba
from .types import CompositorError, ErrorType, Layer, Source, Timeline


def _rgb(spec: dict, key: str, source_id: str) -> np.ndarray:
    value = spec.get(key)
    if not (isinstance(value, list) and len(value) == 3
            and all(isinstance(c, int) and not isinstance(c, bool)
                    and 0 <= c <= 255 for c in value)):
        raise CompositorError(
            f"素材 {source_id!r} 的 spec.{key} 必须是 3 个 0~255 整数",
            ErrorType.RENDER_FAILED)
    return np.asarray(value, dtype=np.float64) / 255.0


def _ratio(spec: dict, key: str, source_id: str) -> float:
    value = spec.get(key)
    if not isinstance(value, (int, float)) or isinstance(value, bool) \
            or not (0.0 < float(value) <= 1.0):
        raise CompositorError(
            f"素材 {source_id!r} 的 spec.{key} 必须是 (0,1] 内的数值",
            ErrorType.RENDER_FAILED)
    return float(value)


def _alpha_value(spec: dict, key: str, source_id: str) -> float:
    value = spec.get(key)
    if not isinstance(value, (int, float)) or isinstance(value, bool) \
            or not (0.0 <= float(value) <= 1.0):
        raise CompositorError(
            f"素材 {source_id!r} 的 spec.{key} 必须是 [0,1] 内的数值",
            ErrorType.ALPHA_OUT_OF_RANGE)
    return float(value)


def materialize_rgba(source: Source) -> np.ndarray:
    """把程序化素材物化为非预乘 float64 RGBA 数组 (H x W x 4)。"""
    w, h, sid = source.width, source.height, source.source_id
    cols = np.arange(w, dtype=np.float64)
    rows = np.arange(h, dtype=np.float64)
    gx = cols / max(1, w - 1)
    gy = rows / max(1, h - 1)
    t_x, t_y = np.meshgrid(gx, gy)

    if source.kind == "gradient":
        ca, cb = _rgb(source.spec, "color_a", sid), _rgb(source.spec, "color_b", sid)
        t = ((t_x + t_y) * 0.5)[..., None]
        rgb = ca * (1.0 - t) + cb * t
        alpha = np.ones((h, w, 1), dtype=np.float64)
        return np.concatenate([np.clip(rgb, 0, 1), alpha], axis=-1)

    if source.kind == "sprite":
        center = _rgb(source.spec, "center_rgb", sid)
        a0 = _alpha_value(source.spec, "center_alpha", sid)
        a1 = _alpha_value(source.spec, "edge_alpha", sid)
        dx = (t_x - 0.5) * 2.0
        dy = (t_y - 0.5) * 2.0
        d = np.minimum(1.0, np.sqrt(dx * dx + dy * dy))[..., None]
        alpha = np.clip(a0 * (1.0 - d) + a1 * d, 0.0, 1.0)
        rgb = np.broadcast_to(center, (h, w, 3)).copy()
        return np.concatenate([rgb, alpha], axis=-1)

    raise CompositorError(f"素材 {sid!r} 类型 {source.kind!r} 不是 RGBA 素材",
                          ErrorType.RENDER_FAILED)


def materialize_mask(source: Source) -> np.ndarray:
    """把 mask 素材物化为 H x W float64 覆盖率平面 (椭圆内为 1)。"""
    if source.kind != "mask":
        raise CompositorError(f"素材 {source.source_id!r} 不是 mask 类型",
                              ErrorType.RENDER_FAILED)
    rx = _ratio(source.spec, "rx_ratio", source.source_id)
    ry = _ratio(source.spec, "ry_ratio", source.source_id)
    w, h = source.width, source.height
    gx, gy = np.meshgrid(np.arange(w, dtype=np.float64),
                         np.arange(h, dtype=np.float64))
    nx = ((gx - (w - 1) / 2.0) / max(1.0, w * rx / 2.0)) ** 2
    ny = ((gy - (h - 1) / 2.0) / max(1.0, h * ry / 2.0)) ** 2
    return (nx + ny <= 1.0).astype(np.float64)


def _pil_transform(image: Image.Image, scale: float, rotation: float) -> Image.Image:
    """先缩放后绕中心逆时针旋转, 保持画布尺寸 (不扩展)。"""
    w, h = image.size
    new_w, new_h = max(1, int(round(w * scale))), max(1, int(round(h * scale)))
    resized = image.resize((new_w, new_h), Image.Resampling.BILINEAR)
    if rotation == 0.0:
        return resized
    return resized.rotate(rotation, resample=Image.Resampling.BILINEAR,
                          expand=False)


class FrameRenderer:
    """按 Timeline 逐帧合成。线程不安全, 但可重复渲染且结果确定。"""

    def __init__(self, timeline: Timeline) -> None:
        self.timeline = timeline
        self._rgba_cache: dict[str, np.ndarray] = {}
        self._mask_cache: dict[str, np.ndarray] = {}

    def _get_rgba(self, source_id: str) -> np.ndarray:
        if source_id not in self._rgba_cache:
            self._rgba_cache[source_id] = materialize_rgba(
                self.timeline.sources[source_id])
        return self._rgba_cache[source_id]

    def _get_mask(self, source_id: str) -> np.ndarray:
        if source_id not in self._mask_cache:
            self._mask_cache[source_id] = materialize_mask(
                self.timeline.sources[source_id])
        return self._mask_cache[source_id]

    def _prepare_layer(self, layer: Layer) -> tuple[np.ndarray, int, int]:
        """物化 + 缩放旋转 + alpha/遮罩, 返回 (float RGBA, 宽, 高)。"""
        rgba = self._get_rgba(layer.source_id)
        transformed = np.asarray(
            _pil_transform(Image.fromarray(quantize_rgba(rgba)),
                           layer.scale, layer.rotation),
            dtype=np.float64) / 255.0
        if layer.mask_source_id is not None:
            mask = self._get_mask(layer.mask_source_id)
            mask_img = Image.fromarray(np.round(mask * 255.0).astype(np.uint8),
                                       mode="L")
            coverage = np.asarray(
                _pil_transform(mask_img, layer.scale, layer.rotation),
                dtype=np.float64) / 255.0
            transformed[..., 3] *= coverage
        transformed[..., 3] = np.clip(transformed[..., 3] * layer.alpha, 0.0, 1.0)
        return transformed, transformed.shape[1], transformed.shape[0]

    def active_layers(self, frame_index: int) -> list[Layer]:
        """返回该帧活跃图层, 按 (轨道号, 声明顺序) 稳定排序。

        半开区间 [frame_in, frame_out): 边界帧只属于后一图层, 不重复。
        """
        active = [layer for layer in self.timeline.layers
                  if layer.frame_in <= frame_index < layer.frame_out]
        return sorted(active, key=lambda lyr: (lyr.track, lyr.order))

    def render_frame(self, frame_index: int) -> np.ndarray:
        """渲染单帧, 返回 uint8 RGBA 画布 (H x W x 4)。"""
        tl = self.timeline
        if not (0 <= frame_index < tl.frame_count):
            raise CompositorError(
                f"帧号 {frame_index} 越界, 合法范围 [0, {tl.frame_count})",
                ErrorType.FRAME_OUT_OF_RANGE)
        canvas = np.zeros((tl.height, tl.width, 4), dtype=np.float64)
        for layer in self.active_layers(frame_index):
            pixels, lw, lh = self._prepare_layer(layer)
            # 画布外裁剪: 只取两边相交的矩形, 彻底避免数组越界。
            cx0, cy0 = max(0, layer.x), max(0, layer.y)
            cx1, cy1 = min(tl.width, layer.x + lw), min(tl.height, layer.y + lh)
            if cx0 >= cx1 or cy0 >= cy1:
                continue
            lx0, ly0 = cx0 - layer.x, cy0 - layer.y
            region = pixels[ly0:ly0 + (cy1 - cy0), lx0:lx0 + (cx1 - cx0)]
            composite_layer(canvas[cy0:cy1, cx0:cx1], region, layer.blend)
        return quantize_rgba(canvas)

    def render_all(self) -> Iterator[tuple[int, np.ndarray]]:
        """流式产出 (帧号, uint8 RGBA 画布), 调用方逐帧消费即可控内存。"""
        for frame_index in range(self.timeline.frame_count):
            yield frame_index, self.render_frame(frame_index)
