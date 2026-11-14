"""Deterministic layers, PCM cues, and a demo timeline generated without assets."""

from __future__ import annotations

import json
import os
import tempfile
import wave
from pathlib import Path

import numpy as np
from PIL import Image

from .errors import ProjectError
from .timeline import Fps

SAMPLE_RATE = 2_400_000
CUE_COUNT = 6


def _atomic_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def _save_png(path: Path, pixels: np.ndarray) -> None:
    image = Image.fromarray(pixels, "RGBA" if pixels.shape[2] == 4 else "RGB")
    path.parent.mkdir(parents=True, exist_ok=True)
    image.save(path, format="PNG", optimize=False)


def _make_base(width: int, height: int) -> np.ndarray:
    x = np.linspace(0.0, 1.0, width, dtype=np.float32)[None, :]
    y = np.linspace(0.0, 1.0, height, dtype=np.float32)[:, None]
    pixels = np.empty((height, width, 3), dtype=np.uint8)
    pixels[..., 0] = np.clip(22 + 90 * x + 35 * y, 0, 255)
    pixels[..., 1] = np.clip(26 + 40 * x + 80 * y, 0, 255)
    pixels[..., 2] = np.clip(46 + 100 * (1.0 - y), 0, 255)
    for line in range(0, width, 120):
        pixels[:, line : line + 2, :] = 96
    for line in range(0, height, 160):
        pixels[line : line + 2, :, :] = 72
    return pixels


def _make_product(width: int = 600, height: int = 760) -> np.ndarray:
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float32)
    center_x, center_y = width / 2.0, height / 2.0
    radius_x, radius_y = width / 2.0 - 8.0, height / 2.0 - 8.0
    distance = ((xx - center_x) / radius_x) ** 2 + ((yy - center_y) / radius_y) ** 2
    alpha = np.clip((1.0 - distance) * 24.0, 0.0, 1.0)
    pixels = np.zeros((height, width, 4), dtype=np.uint8)
    pixels[..., 0] = np.clip(244 - 18 * (yy / height), 0, 255)
    pixels[..., 1] = np.clip(148 + 52 * (xx / width), 0, 255)
    pixels[..., 2] = np.clip(42 + 38 * (1.0 - yy / height), 0, 255)
    pixels[..., 3] = np.round(alpha * 255.0).astype(np.uint8)
    highlight = ((xx - center_x + 120) ** 2 + (yy - center_y + 220) ** 2) < 65 ** 2
    pixels[highlight, :3] = np.clip(pixels[highlight, :3].astype(np.int16) + 56, 0, 255)
    return pixels


def _make_subtitle(width: int = 980, height: int = 180) -> np.ndarray:
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float32)
    corner = 32.0
    dx = np.maximum(np.maximum(corner - xx, xx - (width - corner)), 0.0)
    dy = np.maximum(np.maximum(corner - yy, yy - (height - corner)), 0.0)
    outside = np.sqrt(dx ** 2 + dy ** 2) - corner
    alpha = np.clip(-outside, 0.0, 1.0) * 0.94
    pixels = np.zeros((height, width, 4), dtype=np.uint8)
    pixels[..., :3] = np.array([244, 233, 202], dtype=np.uint8)
    pixels[..., 3] = np.round(alpha * 255.0).astype(np.uint8)
    blocks = [(70, 65, 720, 45), (105, 122, 520, 18), (45, 122, 36, 18)]
    for x0, y0, block_width, block_height in blocks:
        pixels[y0 : y0 + block_height, x0 : x0 + block_width, :3] = (39, 52, 66)
    return pixels


def _make_watermark(width: int = 210, height: int = 92) -> np.ndarray:
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float32)
    pixels = np.zeros((height, width, 4), dtype=np.uint8)
    ring = np.abs(np.sqrt((xx - 46) ** 2 + (yy - 46) ** 2) - 30.0) < 8.0
    bars = (xx > 94) & (xx < 190) & (yy > 24) & (yy < 68)
    pixels[ring | bars, :3] = (246, 248, 252)
    pixels[ring | bars, 3] = 214
    return pixels


def _cue_frames(frame_count: int) -> list[int]:
    return [int(round((index + 0.5) * frame_count / CUE_COUNT)) for index in range(CUE_COUNT)]


def _write_cues(path: Path, fps: Fps, frame_count: int) -> list[int]:
    cues = _cue_frames(frame_count)
    total_samples = fps.sample_index(frame_count, SAMPLE_RATE)
    pulse_samples = max(1, SAMPLE_RATE // 100)
    starts = [fps.sample_index(frame, SAMPLE_RATE) + (1 if index == 2 else 0) for index, frame in enumerate(cues)]
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as raw_handle:
            with wave.open(raw_handle, "wb") as handle:
                handle.setnchannels(1)
                handle.setsampwidth(2)
                handle.setframerate(SAMPLE_RATE)
                chunk_size = 8192
                for chunk_start in range(0, total_samples, chunk_size):
                    chunk_stop = min(total_samples, chunk_start + chunk_size)
                    block = np.zeros(chunk_stop - chunk_start, dtype=np.int16)
                    for start in starts:
                        overlap_start = max(start, chunk_start)
                        overlap_stop = min(start + pulse_samples, chunk_stop)
                        if overlap_start < overlap_stop:
                            block[overlap_start - chunk_start : overlap_stop - chunk_start] = 26214
                    handle.writeframesraw(block.tobytes())
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise
    return cues


def generate_fixtures(out_dir: str | Path, frames: int = 120, fps_text: str = "30000/1001") -> dict[str, object]:
    if isinstance(frames, bool) or not isinstance(frames, int) or frames < CUE_COUNT:
        raise ProjectError("fixtures requires at least 6 frames")
    fps = Fps.parse(fps_text)
    output = Path(out_dir)
    assets = output / "assets"
    assets.mkdir(parents=True, exist_ok=True)
    width, height = 1080, 1920
    _save_png(assets / "base.png", _make_base(width, height))
    _save_png(assets / "product.png", _make_product())
    _save_png(assets / "subtitle.png", _make_subtitle())
    _save_png(assets / "watermark.png", _make_watermark())
    cues = _write_cues(assets / "cues.wav", fps, frames)
    duration_end = frames - 1
    product_keys = [
        {"frame": 0, "position": [540, 900], "scale": [1.0, 1.0], "rotation": -3, "opacity": 0.92, "easing": "linear"},
        {"frame": duration_end // 2, "position": [575, 820], "scale": [1.06, 1.04], "rotation": 2, "opacity": 1.0, "easing": "ease-in-out"},
        {"frame": duration_end, "position": [520, 880], "scale": [1.0, 1.0], "rotation": -1, "opacity": 0.94},
    ]
    subtitle_keys = [
        {"frame": 0, "position": [540, 1510], "opacity": 0.94},
        {"frame": duration_end // 2, "position": [526, 1485], "opacity": 1.0, "easing": "ease-in-out"},
        {"frame": duration_end, "position": [540, 1502], "opacity": 0.96},
    ]
    watermark_keys = [
        {"frame": 0, "position": [952, 1840], "rotation": -8, "opacity": 0.78},
        {"frame": duration_end // 2, "position": [946, 1830], "rotation": 3, "opacity": 0.88, "easing": "ease-in-out"},
        {"frame": duration_end, "position": [958, 1845], "rotation": -4, "opacity": 0.82},
    ]
    project = {
        "version": 1,
        "fps": fps_text,
        "frame_count": frames,
        "canvas": {"width": width, "height": height, "background": [9, 12, 20, 255]},
        "layers": [
            {
                "id": "base", "type": "image", "path": "assets/base.png", "in": 0, "out": duration_end,
                "blend": "normal", "transform": {"anchor": [540, 960], "position": [540, 960], "scale": [1, 1], "rotation": 0},
            },
            {
                "id": "product", "type": "image", "path": "assets/product.png", "in": 0, "out": duration_end,
                "blend": "normal", "opacity": 1.0, "transform": {"anchor": [300, 380]}, "keyframes": product_keys,
            },
            {
                "id": "subtitle", "type": "image", "path": "assets/subtitle.png", "in": 0, "out": duration_end,
                "blend": "normal", "opacity": 1.0, "transform": {"anchor": [490, 90]}, "keyframes": subtitle_keys,
            },
            {
                "id": "watermark", "type": "image", "path": "assets/watermark.png", "in": 0, "out": duration_end,
                "blend": "multiply", "opacity": 0.9, "transform": {"anchor": [105, 46]}, "keyframes": watermark_keys,
            },
        ],
        "audio": {"path": "assets/cues.wav", "cues": cues, "threshold": 0.5, "search_ms": 20},
    }
    project_path = output / "project.json"
    _atomic_bytes(project_path, (json.dumps(project, ensure_ascii=False, indent=2) + "\n").encode("utf-8"))
    return {
        "project": str(project_path),
        "frames": frames,
        "fps": fps_text,
        "cues": len(cues),
        "audio_sample_rate": SAMPLE_RATE,
    }
