"""Feature detection and pyramidal block matching.

Corners: Shi-Tomasi min-eigenvalue response with border masking,
non-maximum suppression, a repetition guard, and automatic threshold
lowering when too few points are found. Matching: coarse-to-fine
exhaustive SAD block search with parabolic sub-pixel refinement.
"""

from __future__ import annotations

import numpy as np


def _box_filter(img: np.ndarray, r: int) -> np.ndarray:
    """Fast box blur via cumulative sums (window 2r+1)."""
    c = np.cumsum(np.cumsum(img, axis=0), axis=1)
    c = np.pad(c, ((1, 0), (1, 0)))
    k = 2 * r + 1
    out = (c[k:, k:] - c[:-k, k:] - c[k:, :-k] + c[:-k, :-k])
    h, w = img.shape
    # pad result back to full size by edge replication of valid region
    full = np.empty_like(img, dtype=np.float64)
    oh, ow = out.shape
    full[:oh, :ow] = out
    if oh < h:
        full[oh:, :ow] = out[-1:, :]
    if ow < w:
        full[:, ow:] = full[:, ow - 1:ow]
    return full / float(k * k)


def corner_response(img: np.ndarray, smooth_r: int = 2) -> np.ndarray:
    """Shi-Tomasi response: smaller eigenvalue of the structure tensor."""
    f = img.astype(np.float64)
    iy, ix = np.gradient(f)
    sxx = _box_filter(ix * ix, smooth_r)
    syy = _box_filter(iy * iy, smooth_r)
    sxy = _box_filter(ix * iy, smooth_r)
    tr = (sxx + syy) * 0.5
    det_term = np.sqrt(np.maximum(((sxx - syy) * 0.5) ** 2 + sxy ** 2, 0.0))
    return np.maximum(tr - det_term, 0.0)


def detect_corners(img: np.ndarray, max_points: int = 150,
                   min_dist: int = 8, border: int = 12,
                   quality: float = 0.01, min_points: int = 40,
                   max_retries: int = 3):
    """Detect corners; lowers the threshold automatically if too few.

    Returns (xy float64 (N,2) as (x, y), scores (N,)).
    """
    response = corner_response(img)
    h, w = response.shape
    response[:border, :] = 0
    response[-border:, :] = 0
    response[:, :border] = 0
    response[:, -border:] = 0
    rmax = response.max()
    if rmax <= 0:
        return np.zeros((0, 2)), np.zeros(0)

    q = quality
    for _ in range(max_retries + 1):
        cand = np.argwhere(response >= q * rmax)
        cap = 20 * max_points
        if len(cand) > cap:  # keep strongest candidates only
            thresh = np.partition(response[cand[:, 0], cand[:, 1]],
                                  -cap)[-cap]
            cand = np.argwhere(response >= thresh)
        scores = response[cand[:, 0], cand[:, 1]]
        order = np.argsort(-scores)
        keep = []
        suppressed = np.zeros(response.shape, dtype=bool)
        for idx in order:
            y, x = cand[idx]
            if suppressed[y, x]:
                continue
            keep.append(idx)
            y0, y1 = max(0, y - min_dist), min(h, y + min_dist + 1)
            x0, x1 = max(0, x - min_dist), min(w, x + min_dist + 1)
            suppressed[y0:y1, x0:x1] = True
            if len(keep) >= max_points:
                break
        pts = cand[keep].astype(np.float64)
        sc = scores[keep]
        pts, sc = _repetition_guard(response, pts, sc)
        if len(pts) >= min_points or q <= quality / (2 ** max_retries):
            xy = pts[:, ::-1]  # (row, col) -> (x, y)
            return xy, sc
        q *= 0.5
    xy = pts[:, ::-1] if len(pts) else np.zeros((0, 2))
    return xy, sc


def _repetition_guard(response, pts, scores, radius=5, ratio=1.5):
    """Drop points in flat/repetitive regions (low local contrast of the
    response map itself)."""
    if len(pts) == 0:
        return pts, scores
    keep = []
    h, w = response.shape
    for i, (y, x) in enumerate(pts.astype(int)):
        y0, y1 = max(0, y - radius), min(h, y + radius + 1)
        x0, x1 = max(0, x - radius), min(w, x + radius + 1)
        local = response[y0:y1, x0:x1]
        if local.mean() * ratio < scores[i] or local.max() == scores[i]:
            keep.append(i)
    if not keep:
        return pts[:0], scores[:0]
    keep = np.asarray(keep)
    return pts[keep], scores[keep]


def build_pyramid(img: np.ndarray, levels: int = 3) -> list[np.ndarray]:
    pyr = [img.astype(np.float64)]
    for _ in range(levels - 1):
        f = pyr[-1]
        h, w = f.shape[0] // 2 * 2, f.shape[1] // 2 * 2
        f = f[:h, :w].reshape(h // 2, 2, w // 2, 2).mean(axis=(1, 3))
        pyr.append(f)
    return pyr


def _patches(img, ys, xs, r):
    """Extract (N, 2r+1, 2r+1) patches centered at integer (ys, xs)."""
    n = len(ys)
    p = 2 * r + 1
    yy = ys[:, None, None] + np.arange(-r, r + 1)[None, :, None]
    xx = xs[:, None, None] + np.arange(-r, r + 1)[None, None, :]
    h, w = img.shape
    ok = (ys >= r) & (ys < h - r) & (xs >= r) & (xs < w - r)
    yyc = np.clip(yy, 0, h - 1)
    xxc = np.clip(xx, 0, w - 1)
    return img[yyc, xxc], ok


def _match_level(prev, curr, templ_pts, centers, search_r, patch_r):
    """Exhaustive SAD search.

    Templates are taken from ``prev`` at ``templ_pts`` (original feature
    positions); the search in ``curr`` is centered at ``centers`` (the
    motion-propagated prediction). All coordinates are (N, 2) (x, y)
    floats at this pyramid level.
    """
    n = len(templ_pts)
    tys = np.round(templ_pts[:, 1]).astype(int)
    txs = np.round(templ_pts[:, 0]).astype(int)
    templ, ok = _patches(prev, tys, txs, patch_r)
    cys = np.round(centers[:, 1]).astype(int)
    cxs = np.round(centers[:, 0]).astype(int)
    s = 2 * search_r + 1
    sad = np.full((n, s, s), np.inf)
    for dy in range(-search_r, search_r + 1):
        for dx in range(-search_r, search_r + 1):
            cur, okc = _patches(curr, cys + dy, cxs + dx, patch_r)
            d = np.abs(cur - templ).sum(axis=(1, 2))
            d[~ok | ~okc] = np.inf
            sad[:, dy + search_r, dx + search_r] = d
    flat = sad.reshape(n, -1)
    best = flat.argmin(axis=1)
    by, bx = np.divmod(best, s)
    best_sad = flat[np.arange(n), best]
    finite = np.isfinite(flat)
    mean_sad = np.where(finite, flat, 0).sum(1) / np.maximum(finite.sum(1), 1)
    # parabolic sub-pixel refinement on the 3x3 SAD neighbourhood
    sub = np.zeros((n, 2))
    for i in range(n):
        y, x = by[i], bx[i]
        if not (1 <= y < s - 1 and 1 <= x < s - 1) or not np.isfinite(best_sad[i]):
            continue
        nb = sad[i, y - 1:y + 2, x - 1:x + 2]
        if not np.isfinite(nb).all():
            continue
        c = sad[i, y, x]
        gx = (sad[i, y, x + 1] - sad[i, y, x - 1]) / 2.0
        gy = (sad[i, y + 1, x] - sad[i, y - 1, x]) / 2.0
        hxx = sad[i, y, x + 1] - 2 * c + sad[i, y, x - 1]
        hyy = sad[i, y + 1, x] - 2 * c + sad[i, y - 1, x]
        if hxx > 1e-9:
            sub[i, 0] = -np.clip(gx / hxx, -1, 1)
        if hyy > 1e-9:
            sub[i, 1] = -np.clip(gy / hyy, -1, 1)
    new = centers + np.stack([bx - search_r + sub[:, 0],
                              by - search_r + sub[:, 1]], axis=1)
    conf = 1.0 - best_sad / np.maximum(mean_sad, 1e-9)
    resid = best_sad / float((2 * patch_r + 1) ** 2)
    valid = ok & np.isfinite(best_sad)
    return new, np.clip(conf, 0, 1), resid, valid


def track_points(prev_img, curr_img, points, levels: int = 3,
                 search_r: int = 4, patch_r: int = 4):
    """Coarse-to-fine pyramidal matching of (N,2) (x, y) points.

    Returns (new_xy, confidence, match_residual, valid).
    """
    points = np.asarray(points, dtype=np.float64)
    if len(points) == 0:
        z = np.zeros(0)
        return points.copy(), z, z, np.zeros(0, dtype=bool)
    prev_pyr = build_pyramid(prev_img, levels)
    curr_pyr = build_pyramid(curr_img, levels)
    scale = 2.0 ** (levels - 1)
    templ = points / scale  # template positions always in prev image
    centers = points / scale
    new, conf, resid, valid = _match_level(
        prev_pyr[-1], curr_pyr[-1], templ, centers, search_r + 2, patch_r)
    for lvl in range(levels - 2, -1, -1):
        templ = points / (2.0 ** lvl)
        centers = new * 2.0
        new, conf, resid, valid = _match_level(
            prev_pyr[lvl], curr_pyr[lvl], templ, centers, search_r, patch_r)
    return new, conf, resid, valid
