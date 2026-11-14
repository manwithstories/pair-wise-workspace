"""Generate layered PNGs, a timeline JSON, and WAVs for demos and tests."""

import argparse
import json
import os

import numpy as np
from PIL import Image

from timeliner import audio as audio_mod

W, H = 1920, 1080


def _save_png(arr, path):
    Image.fromarray(arr, "RGBA").save(path, optimize=False)


def make_base(rng):
    """Full-frame background: vertical gradient plus soft blocks."""
    y = np.linspace(0.0, 1.0, H)[:, None]
    x = np.linspace(0.0, 1.0, W)[None, :]
    r = 30 + 60 * y + 20 * x
    g = 35 + 50 * y
    b = 60 + 90 * (1.0 - y) + 15 * x
    img = np.stack([
        np.broadcast_to(r, (H, W)),
        np.broadcast_to(g, (H, W)),
        np.broadcast_to(b, (H, W)),
        np.full((H, W), 255.0),
    ], axis=-1)
    for _ in range(6):
        cx, cy = rng.uniform(0.1, 0.9, 2)
        rad = rng.uniform(0.05, 0.2)
        glow = np.exp(-(((x - cx) ** 2 + (y - cy) ** 2) / (2 * rad ** 2)))
        img[..., 1] += 40 * glow
        img[..., 2] += 30 * glow
    return np.clip(img, 0, 255).astype(np.uint8)


def make_mid(rng):
    """Mid-ground artifact: a shaded vessel silhouette with alpha."""
    w, h = 800, 600
    yy, xx = np.mgrid[0:h, 0:w].astype(np.float64)
    cx, cy = w / 2.0, h / 2.0
    body = ((xx - cx) / 260.0) ** 2 + ((yy - cy) / 240.0) ** 2 <= 1.0
    neck = (np.abs(xx - cx) <= 70) & (yy >= 40) & (yy <= cy)
    mask = body | neck
    shade = np.clip(1.0 - (xx - cx) / 500.0 - (yy - cy) / 900.0, 0.3, 1.0)
    img = np.zeros((h, w, 4), np.uint8)
    img[..., 0] = (150 * shade).astype(np.uint8)
    img[..., 1] = (90 * shade).astype(np.uint8)
    img[..., 2] = (60 * shade).astype(np.uint8)
    img[..., 3] = np.where(mask, 255, 0).astype(np.uint8)
    return img


def make_glass(rng):
    """Foreground glass reflection: diagonal translucent streaks."""
    y = np.linspace(0.0, 1.0, H)[:, None]
    x = np.linspace(0.0, 1.0, W)[None, :]
    diag = x + 0.6 * y
    streak = np.clip(1.0 - np.abs(diag - 0.9) / 0.18, 0, 1) ** 2
    streak += 0.6 * np.clip(1.0 - np.abs(diag - 1.35) / 0.10, 0, 1) ** 2
    alpha = np.clip(streak, 0, 1) * 110
    img = np.zeros((H, W, 4), np.uint8)
    img[..., 0] = 220
    img[..., 1] = 230
    img[..., 2] = 255
    img[..., 3] = alpha.astype(np.uint8)
    return img


def make_subtitle(rng):
    """Subtitle bar: dark rounded strip with bright glyph-like blocks."""
    w, h = 1600, 200
    yy, xx = np.mgrid[0:h, 0:w].astype(np.float64)
    img = np.zeros((h, w, 4), np.uint8)
    bar = (yy >= 20) & (yy <= h - 20) & (xx >= 10) & (xx <= w - 10)
    img[..., 3] = np.where(bar, 190, 0).astype(np.uint8)
    img[..., 2] = np.where(bar, 30, 0).astype(np.uint8)
    pos = 80.0
    while pos < w - 160:
        gw = rng.uniform(50, 110)
        glyph = (xx >= pos) & (xx <= pos + gw) & (yy >= 70) & (yy <= 130)
        img[..., 0] = np.where(glyph, 235, img[..., 0])
        img[..., 1] = np.where(glyph, 225, img[..., 1])
        img[..., 2] = np.where(glyph, 210, img[..., 2])
        img[..., 3] = np.where(glyph, 255, img[..., 3])
        pos += gw + rng.uniform(25, 60)
    return img


def make_noise(rng):
    """Fine grain overlay tile, neutral gray with modest alpha."""
    gray = rng.integers(96, 160, (H, W), dtype=np.int64).astype(np.uint8)
    img = np.zeros((H, W, 4), np.uint8)
    img[..., 0] = gray
    img[..., 1] = gray
    img[..., 2] = gray
    img[..., 3] = 26
    return img


def make_timeline():
    return {
        "version": 1,
        "canvas": {"width": W, "height": H},
        "duration": 6.0,
        "layers": [
            {"id": "base", "asset": "base.png", "start": 0.0, "duration": 6.0,
             "blend": "normal", "width": W, "height": H,
             "anchor": [0.5, 0.5],
             "keyframes": [
                 {"t": 0.0, "x": 0.0, "y": 0.0, "scale_x": 1.0,
                  "scale_y": 1.0, "easing": "ease-in-out"},
                 {"t": 6.0, "scale_x": 1.06, "scale_y": 1.06}]},
            {"id": "mid", "asset": "mid.png", "start": 0.0, "duration": 6.0,
             "blend": "normal", "width": 800, "height": 600,
             "anchor": [0.5, 0.5],
             "keyframes": [
                 {"t": 0.0, "x": -500.0, "y": 120.0, "rotate": -6.0,
                  "opacity": 1.0, "easing": {"bezier": [0.4, 0.0, 0.2, 1.0]}},
                 {"t": 3.0, "x": 240.0, "y": 80.0, "rotate": 4.0,
                  "easing": "ease-in-out"},
                 {"t": 6.0, "x": 700.0, "y": 140.0, "rotate": 8.0}]},
            {"id": "glass", "asset": "glass.png", "start": 0.0,
             "duration": 6.0, "blend": "screen", "width": W, "height": H,
             "anchor": [0.5, 0.5],
             "keyframes": [
                 {"t": 0.0, "x": 0.0, "y": 0.0, "opacity": 0.7,
                  "easing": "ease-in-out"},
                 {"t": 3.0, "opacity": 1.0, "easing": "ease-in-out"},
                 {"t": 6.0, "opacity": 0.6}]},
            {"id": "subtitle", "asset": "subtitle.png", "start": 1.0,
             "duration": 4.0, "blend": "normal", "width": 1600, "height": 200,
             "anchor": [0.5, 0.5],
             "keyframes": [
                 {"t": 1.0, "x": 0.0, "y": 380.0, "opacity": 0.0,
                  "easing": "step"},
                 {"t": 1.5, "opacity": 1.0}]},
            {"id": "noise", "asset": "noise.png", "start": 0.0,
             "duration": 6.0, "blend": "overlay", "width": W, "height": H,
             "anchor": [0.5, 0.5],
             "keyframes": [{"t": 0.0, "x": 0.0, "y": 0.0, "opacity": 1.0}]},
        ],
        "audio": [
            {"asset": "voice.wav", "start": 0.5, "gain": 0.9},
            {"asset": "music.wav", "start": 0.0, "gain": 0.5},
        ],
    }


def generate(out_dir, seed):
    os.makedirs(out_dir, exist_ok=True)
    rng = np.random.default_rng(seed)
    _save_png(make_base(rng), os.path.join(out_dir, "base.png"))
    _save_png(make_mid(rng), os.path.join(out_dir, "mid.png"))
    _save_png(make_glass(rng), os.path.join(out_dir, "glass.png"))
    _save_png(make_subtitle(rng), os.path.join(out_dir, "subtitle.png"))
    _save_png(make_noise(rng), os.path.join(out_dir, "noise.png"))
    audio_mod.synth_wav(os.path.join(out_dir, "voice.wav"), "voice", 6.0,
                        seed=seed)
    audio_mod.synth_wav(os.path.join(out_dir, "music.wav"), "music", 6.0,
                        seed=seed + 1)
    timeline = make_timeline()
    with open(os.path.join(out_dir, "edl.json"), "w", encoding="utf-8") as fh:
        json.dump(timeline, fh, indent=2, sort_keys=True)
        fh.write("\n")
    return timeline


def main(argv=None):
    parser = argparse.ArgumentParser(prog="gen_fixtures")
    parser.add_argument("--out", required=True)
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args(argv)
    generate(args.out, args.seed)
    print(json.dumps({"out": args.out, "seed": args.seed, "ok": True}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
