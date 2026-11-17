"""Deterministic synthetic sequence generator.

Builds a textured 2-D "world", then renders frames by sampling it along
a camera path with intentional pan/rotation, high-frequency jitter,
rolling-shutter row delays, occlusions, moving foreground and noise.
Everything is driven by one seed, so sequences are reproducible.
"""

from __future__ import annotations

import numpy as np

from .frameio import bilinear_sample


def _smooth_noise(rng: np.random.Generator, h: int, w: int,
                  cell: int) -> np.ndarray:
    """Random noise upsampled from a coarse grid -> smooth blobs."""
    ch, cw = max(2, h // cell), max(2, w // cell)
    coarse = rng.random((ch, cw))
    ys = np.linspace(0, ch - 1.001, h)
    xs = np.linspace(0, cw - 1.001, w)
    yy, xx = np.meshgrid(ys, xs, indexing="ij")
    return bilinear_sample(coarse, yy, xx)


def make_world(width: int, height: int,
               rng: np.random.Generator) -> np.ndarray:
    """Multi-scale textured scene with strong structural edges."""
    world = np.zeros((height, width))
    for cell, amp in ((64, 0.35), (24, 0.30), (9, 0.25), (4, 0.10)):
        world += amp * _smooth_noise(rng, height, width, cell)
    for _ in range(max(12, width // 60)):  # structural edges for corners
        x0, y0 = rng.integers(0, width), rng.integers(0, height)
        x1 = int(np.clip(x0 + rng.integers(-120, 120), 0, width - 1))
        y1 = int(np.clip(y0 + rng.integers(-120, 120), 0, height - 1))
        val = rng.uniform(0.3, 1.0)
        n = max(abs(x1 - x0), abs(y1 - y0), 1)
        xs = np.linspace(x0, x1, n).astype(int)
        ys = np.linspace(y0, y1, n).astype(int)
        world[ys, xs] = val
    for _ in range(max(10, width // 80)):  # unique blocks (buildings/rocks)
        bw, bh = int(rng.integers(8, 40)), int(rng.integers(6, 30))
        bx, by = int(rng.integers(0, width - bw)), int(rng.integers(0, height - bh))
        world[by:by + bh, bx:bx + bw] = rng.uniform(0.1, 0.95)
        world[by:by + 2, bx:bx + bw] = rng.uniform(0.0, 1.0)  # distinct rim
    lo, hi = world.min(), world.max()
    return (world - lo) / max(hi - lo, 1e-9)


def camera_path(n_frames: int, pan: float, jitter_px: float,
                rot_deg: float, rng: np.random.Generator):
    """Intentional pan + drift plus sinusoidal high-frequency jitter."""
    t = np.arange(n_frames, dtype=np.float64)
    jitter = {}
    for axis in ("x", "y"):
        sig = np.zeros(n_frames)
        for _ in range(3):
            amp = rng.uniform(0.6, 1.0) * jitter_px
            freq = rng.uniform(0.08, 0.35)
            sig += amp * np.sin(2 * np.pi * freq * t + rng.uniform(0, 2 * np.pi))
        sig *= jitter_px / max(sig.std(), 1e-9)
        jitter[axis] = sig
    rot = np.deg2rad(rot_deg) * np.sin(2 * np.pi * 0.05 * t)
    rot += np.deg2rad(rot_deg * 0.3) * rng.standard_normal(n_frames)
    drift = 0.15 * np.sin(2 * np.pi * 0.01 * t)
    return {"tx": pan * t + jitter["x"], "ty": 4.0 * drift + jitter["y"],
            "theta": rot}


def _render(world, cx, cy, theta, w, h, rs, direction,
            cx1, cy1, theta1) -> np.ndarray:
    rows = np.arange(h, dtype=np.float64)
    frac = rows / max(h - 1, 1) - 0.5
    tau = rs * direction * frac  # row exposure offset in frame periods
    cx_r = cx + tau * (cx1 - cx)
    cy_r = cy + tau * (cy1 - cy)
    th_r = theta + tau * (theta1 - theta)
    cols = np.arange(w, dtype=np.float64) - w / 2.0
    rows_c = rows - h / 2.0
    out = np.empty((h, w))
    for r in range(h):
        cos, sin = np.cos(th_r[r]), np.sin(th_r[r])
        xs = cx_r[r] + cos * cols - sin * rows_c[r]
        ys = cy_r[r] + sin * cols + cos * rows_c[r]
        out[r] = bilinear_sample(world, ys, xs)
    return out


def generate_sequence(n_frames: int, width: int, height: int, seed: int,
                      pan: float = 12.0, rolling_shutter: float = 0.0,
                      jitter_px: float = 10.0, rot_deg: float = 0.6,
                      n_occlusions: int = 4, n_foreground: int = 3,
                      noise_sigma: float = 2.0, direction: int = 1):
    """Generate a reproducible sequence; returns (frames uint8, truth)."""
    rng = np.random.default_rng(seed)
    path = camera_path(n_frames, pan, jitter_px, rot_deg, rng)
    margin_x = int(abs(pan) * 0 + 4 * jitter_px + 60)
    margin_y = int(4 * jitter_px + 60)
    world_w = int(width + 2 * margin_x + abs(pan) * (n_frames + 1))
    world_h = int(height + 2 * margin_y)
    world = make_world(world_w, world_h, rng) * 255.0
    x0 = margin_x + width / 2.0
    y0 = world_h / 2.0

    frames = np.empty((n_frames, height, width))
    for k in range(n_frames):
        k1 = min(k + 1, n_frames - 1)
        fr = _render(world, x0 + path["tx"][k], y0 + path["ty"][k],
                     path["theta"][k], width, height, rolling_shutter,
                     direction, x0 + path["tx"][k1], y0 + path["ty"][k1],
                     path["theta"][k1])
        frames[k] = fr

    occ = []
    for _ in range(n_occlusions):  # random occluding rectangles
        f0 = int(rng.integers(0, max(n_frames - 8, 1)))
        f1 = min(f0 + int(rng.integers(3, 9)), n_frames)
        r0 = int(rng.integers(0, height - 20))
        c0 = int(rng.integers(0, width - 20))
        rh = int(rng.integers(10, 25))
        cw = int(rng.integers(10, 30))
        frames[f0:f1, r0:r0 + rh, c0:c0 + cw] = rng.uniform(90, 130)
        occ.append({"frames": [f0, f1], "rect": [r0, c0, rh, cw]})

    fg = []
    for _ in range(n_foreground):  # moving foreground blobs
        vy = rng.uniform(-1.5, 1.5)
        vx = rng.uniform(-2.5, 2.5)
        rad = int(rng.integers(4, 9))
        cy0 = rng.uniform(0.2, 0.8) * height
        cx0 = rng.uniform(0.2, 0.8) * width
        val = rng.uniform(40, 220)
        fg.append({"v": [vy, vx], "r": rad})
        yy, xx = np.mgrid[0:height, 0:width]
        for k in range(n_frames):
            py, px = cy0 + vy * k, cx0 + vx * k
            mask = (yy - py) ** 2 + (xx - px) ** 2 <= rad ** 2
            frames[k][mask] = val

    frames += rng.standard_normal(frames.shape) * noise_sigma
    frames = np.clip(frames, 0, 255).astype(np.uint8)
    truth = {"tx": (x0 + path["tx"]).tolist(), "ty": (y0 + path["ty"]).tolist(),
             "theta": path["theta"].tolist(), "pan": pan,
             "rolling_shutter": rolling_shutter, "direction": direction,
             "jitter_px": jitter_px, "seed": seed, "occlusions": occ,
             "foreground": fg, "origin": [x0, y0]}
    return frames, truth
