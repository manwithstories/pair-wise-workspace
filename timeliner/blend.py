"""Vectorized blend-mode kernels with straight (non-premultiplied) alpha.

All kernels operate on float arrays in [0, 1]. Compositing uses the
W3C "source-over with blend" formula so semi-transparent edges keep
their correct color instead of darkening or picking up fringe colors.
"""

import numpy as np

from .errors import BlendModeError
from .model import BLEND_MODES

_U16_MAX = 65535.0
_BAND_ROWS = 256


def _normal(sc, dc):
    return sc


def _multiply(sc, dc):
    return sc * dc


def _screen(sc, dc):
    return sc + dc - sc * dc


def _overlay(sc, dc):
    return np.where(dc <= 0.5, 2.0 * sc * dc,
                    1.0 - 2.0 * (1.0 - sc) * (1.0 - dc))


def _add(sc, dc):
    return np.minimum(sc + dc, 1.0)


def _difference(sc, dc):
    return np.abs(dc - sc)


def _darken(sc, dc):
    return np.minimum(sc, dc)


def _lighten(sc, dc):
    return np.maximum(sc, dc)


_KERNELS = {
    "normal": _normal,
    "multiply": _multiply,
    "screen": _screen,
    "overlay": _overlay,
    "add": _add,
    "difference": _difference,
    "darken": _darken,
    "lighten": _lighten,
}


def blend_kernel(mode, sc, dc):
    """Apply a blend kernel to source/destination color arrays in [0, 1]."""
    try:
        fn = _KERNELS[mode]
    except KeyError:
        raise BlendModeError(
            "unknown blend mode %r (expected one of %s)"
            % (mode, list(BLEND_MODES)))
    return fn(sc, dc)


def composite_region(dst, src, mode, opacity=1.0):
    """Composite an RGBA uint16 source region onto a uint16 destination.

    dst, src: (h, w, 4) uint16 arrays of identical shape, modified in place
    for dst. opacity is an extra layer-level multiplier in [0, 1]. Rows are
    processed in bands to bound peak memory.
    """
    if mode == "normal" and opacity >= 1.0:
        alpha = src[..., 3]
        if alpha.min() == 65535:
            np.copyto(dst, src)
            return dst
    for y0 in range(0, src.shape[0], _BAND_ROWS):
        y1 = min(y0 + _BAND_ROWS, src.shape[0])
        _composite_band(dst[y0:y1], src[y0:y1], mode, opacity)
    return dst


def _composite_band(dst, src, mode, opacity):
    sa = src[..., 3:4].astype(np.float32)
    sa *= np.float32(1.0 / _U16_MAX)
    if opacity < 1.0:
        sa *= np.float32(opacity)
    da = dst[..., 3:4].astype(np.float32)
    da *= np.float32(1.0 / _U16_MAX)
    sc = src[..., :3].astype(np.float32)
    sc *= np.float32(1.0 / _U16_MAX)
    dc = dst[..., :3].astype(np.float32)
    dc *= np.float32(1.0 / _U16_MAX)
    blended = blend_kernel(mode, sc, dc)
    out_a = sa + da * (1.0 - sa)
    # Weighted combination isolating the blended area; safe where out_a == 0.
    num = blended * sa * da + sc * sa * (1.0 - da) + dc * da * (1.0 - sa)
    out_c = np.zeros_like(num)
    np.divide(num, out_a, out=out_c, where=out_a > 0.0)
    dst[..., :3] = np.rint(out_c * _U16_MAX).astype(np.uint16)
    dst[..., 3:4] = np.rint(out_a * _U16_MAX).astype(np.uint16)
    return dst
