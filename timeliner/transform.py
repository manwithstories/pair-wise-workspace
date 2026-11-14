"""Affine transforms, bilinear resampling, and bounding-box clipping."""

import math

import numpy as np

_GRID_CACHE = {}
_BAND_ROWS = 256


def _grids(x0, y0, x1, y1):
    """Cached canvas-pixel-center coordinate grids for a bbox."""
    key = (x0, y0, x1, y1)
    grids = _GRID_CACHE.get(key)
    if grids is None:
        cx = np.arange(x0, x1, dtype=np.float32) + np.float32(0.5)
        cy = np.arange(y0, y1, dtype=np.float32) + np.float32(0.5)
        grids = np.meshgrid(cx, cy)
        if len(_GRID_CACHE) < 16:
            _GRID_CACHE[key] = grids
    return grids


def affine_matrix(x, y, scale_x, scale_y, rotate_deg, anchor, src_w, src_h):
    """Source-pixel to canvas-pixel affine matrix (3x3, homogeneous).

    The anchor is given in normalized source coordinates; the layer is
    rotated and scaled about the anchor point, then translated by (x, y),
    which positions the anchor on the canvas.
    """
    ax = anchor[0] * src_w
    ay = anchor[1] * src_h
    theta = math.radians(rotate_deg)
    cos_t = math.cos(theta)
    sin_t = math.sin(theta)
    m = np.array([
        [cos_t * scale_x, -sin_t * scale_y,
         x + ax - ax * cos_t * scale_x + ay * sin_t * scale_y],
        [sin_t * scale_x, cos_t * scale_y,
         y + ay - ax * sin_t * scale_x - ay * cos_t * scale_y],
        [0.0, 0.0, 1.0],
    ], dtype=np.float64)
    return m


def dest_bbox(matrix, src_w, src_h, canvas_w, canvas_h):
    """Integer bbox of the transformed source, clipped to the canvas.

    Returns (x0, y0, x1, y1) with x1/y1 exclusive, or None when the layer
    does not intersect the canvas.
    """
    corners = np.array([
        [0.0, 0.0, 1.0],
        [src_w, 0.0, 1.0],
        [0.0, src_h, 1.0],
        [src_w, src_h, 1.0],
    ])
    pts = corners @ matrix.T
    xs = pts[:, 0]
    ys = pts[:, 1]
    x0 = max(int(math.floor(xs.min())), 0)
    y0 = max(int(math.floor(ys.min())), 0)
    x1 = min(int(math.ceil(xs.max())), canvas_w)
    y1 = min(int(math.ceil(ys.max())), canvas_h)
    if x1 <= x0 or y1 <= y0:
        return None
    return (x0, y0, x1, y1)


def warp(src, matrix, bbox):
    """Bilinear-resample an RGBA source into the canvas region of bbox.

    src: (H, W, 4) uint8 array. Returns a (bh, bw, 4) uint8 array covering
    the bbox exactly; pixels outside the source are transparent. Rows are
    processed in bands to bound peak memory.
    """
    x0, y0, x1, y1 = bbox
    inv = np.linalg.inv(matrix).astype(np.float32)
    out = np.empty((y1 - y0, x1 - x0, 4), dtype=np.uint8)
    for band_y0 in range(y0, y1, _BAND_ROWS):
        band_y1 = min(band_y0 + _BAND_ROWS, y1)
        out[band_y0 - y0:band_y1 - y0] = _warp_band(
            src, inv, x0, band_y0, x1, band_y1)
    return out


def _warp_band(src, inv, x0, y0, x1, y1):
    gx, gy = _grids(x0, y0, x1, y1)
    sx = gx * inv[0, 0]
    sx += gy * inv[0, 1]
    sx += np.float32(inv[0, 2] - 0.5)
    sy = gx * inv[1, 0]
    sy += gy * inv[1, 1]
    sy += np.float32(inv[1, 2] - 0.5)
    h, w = src.shape[0], src.shape[1]
    xf = np.floor(sx)
    yf = np.floor(sy)
    x_lo = xf.astype(np.int32)
    y_lo = yf.astype(np.int32)
    sx -= xf
    sy -= yf
    fx, fy = sx, sy
    x_hi = x_lo + np.int32(1)
    y_hi = y_lo + np.int32(1)
    one = np.float32(1.0)
    w00 = (one - fx) * (one - fy)
    w10 = fx * (one - fy)
    w01 = (one - fx) * fy
    w11 = fx * fy
    w00 *= (x_lo >= 0) & (x_lo < w) & (y_lo >= 0) & (y_lo < h)
    w10 *= (x_hi >= 0) & (x_hi < w) & (y_lo >= 0) & (y_lo < h)
    w01 *= (x_lo >= 0) & (x_lo < w) & (y_hi >= 0) & (y_hi < h)
    w11 *= (x_hi >= 0) & (x_hi < w) & (y_hi >= 0) & (y_hi < h)
    np.clip(x_lo, 0, w - 1, out=x_lo)
    np.clip(x_hi, 0, w - 1, out=x_hi)
    np.clip(y_lo, 0, h - 1, out=y_lo)
    np.clip(y_hi, 0, h - 1, out=y_hi)
    src_f = src.astype(np.float32)
    out = src_f[y_lo, x_lo]
    out *= w00[..., None]
    out += src_f[y_lo, x_hi] * w10[..., None]
    out += src_f[y_hi, x_lo] * w01[..., None]
    out += src_f[y_hi, x_hi] * w11[..., None]
    return np.rint(out).astype(np.uint8)
