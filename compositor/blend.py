"""RGBA 混合器: source-over / add / multiply / screen。

约定:
- 所有数组为 float64, 形状 ``H x W x 4``, RGB 与 A 均归一化到 [0, 1];
- 内部以**非预乘**颜色参与 W3C 风格合成公式, 仅在公式内部临时预乘,
  保证 alpha=0 的透明像素不会把黑色/NaN 污染到最终颜色;
- 未知混合模式抛 :class:`CompositorError`, 进入 CLI 的渲染错误链路(退出码 3)。
"""

from __future__ import annotations

import numpy as np

from .types import BlendMode, CompositorError, ErrorType


# ---------------------------------------------------------------------------
# 预乘 <-> 非预乘
# ---------------------------------------------------------------------------

def to_premultiplied(rgba: np.ndarray) -> np.ndarray:
    """非预乘 RGBA -> 预乘 RGBA(alpha 通道保持不变)。

    透明像素的 RGB 乘以 0 后恒为 0, 不携带任何颜色信息。
    """
    out = np.array(rgba, dtype=np.float64, copy=True)
    alpha = out[..., 3:4]
    out[..., 0:3] = out[..., 0:3] * alpha
    return out


def from_premultiplied(rgb_premul: np.ndarray, alpha: np.ndarray) -> np.ndarray:
    """预乘 RGB + 独立 alpha -> 非预乘 RGBA。

    alpha=0 处直接输出黑色 RGB(颜色本就不可见), 杜绝 0/0 产生 NaN。
    """
    alpha = alpha.astype(np.float64)
    safe_alpha = np.where(alpha > 0.0, alpha, 1.0)
    rgb = rgb_premul.astype(np.float64) / safe_alpha
    rgb = np.clip(rgb, 0.0, 1.0)
    rgb = np.where(alpha > 0.0, rgb, 0.0)
    return np.concatenate([rgb, alpha], axis=-1)


def quantize_rgba(rgba: np.ndarray) -> np.ndarray:
    """把 [0,1] 的 float RGBA 确定性量化为 uint8(四舍五入, 再裁剪)。"""
    clipped = np.clip(rgba, 0.0, 1.0)
    return np.round(clipped * 255.0).astype(np.uint8)


# ---------------------------------------------------------------------------
# 单图层合成
# ---------------------------------------------------------------------------

def _blend_channel(cs: np.ndarray, cd: np.ndarray, mode: BlendMode) -> np.ndarray:
    """非预乘颜色上的混合函数 B(Cd, Cs)。"""
    if mode is BlendMode.SOURCE_OVER:
        return cs
    if mode is BlendMode.MULTIPLY:
        return cs * cd
    if mode is BlendMode.SCREEN:
        return 1.0 - (1.0 - cs) * (1.0 - cd)
    raise CompositorError(f"未知混合模式: {mode!r}", ErrorType.UNKNOWN_BLEND)


def composite_layer(dst: np.ndarray, layer: np.ndarray, mode: BlendMode) -> None:
    """把单个图层就地合成到目标画布(两者同尺寸, 均为非预乘 float RGBA)。

    图层的 alpha 平面应已包含全局 alpha 与局部遮罩的作用(renderer 负责)。
    全透明图层为零操作, 目标画布逐像素保持不变。
    """
    if not isinstance(mode, BlendMode):
        raise CompositorError(f"未知混合模式: {mode!r}", ErrorType.UNKNOWN_BLEND)

    cs = layer[..., 0:3]
    cd = dst[..., 0:3]
    as_ = layer[..., 3:4]
    ad = dst[..., 3:4]

    if mode is BlendMode.ADD:
        # 加色(plus-lighter): 预乘颜色直接相加, alpha 饱和到 1。
        ao = np.minimum(1.0, as_ + ad)
        premul_out = cs * as_ + cd * ad
        safe_ao = np.where(ao > 0.0, ao, 1.0)
        co = np.clip(premul_out / safe_ao, 0.0, 1.0)
        co = np.where(ao > 0.0, co, 0.0)
        dst[..., 0:3] = np.where(ao > 0.0, co, cd)
        dst[..., 3:4] = ao
        return

    b = _blend_channel(cs, cd, mode)
    # W3C 通用合成公式(非预乘表达):
    #   ao = as + ad*(1-as)
    #   co = [as*(1-ad)*Cs + as*ad*B + ad*(1-as)*Cd] / ao
    ao = as_ + ad * (1.0 - as_)
    weighted = as_ * (1.0 - ad) * cs + as_ * ad * b + ad * (1.0 - as_) * cd
    safe_ao = np.where(ao > 0.0, ao, 1.0)
    co = np.clip(weighted / safe_ao, 0.0, 1.0)
    # ao=0 处保留原颜色(透明像素不污染), 其余写入合成结果。
    visible = ao > 0.0
    dst[..., 0:3] = np.where(visible, co, cd)
    dst[..., 3:4] = ao


def composite_background(dst: np.ndarray, layer: np.ndarray) -> None:
    """不透明底图快速路径: 直接覆盖(首帧背景图层使用, 减少一次混合运算)。"""
    dst[:] = layer
