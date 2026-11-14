from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Callable

import numpy as np
import pytest
from PIL import Image


def _save_rgba(path: Path, pixels: np.ndarray) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    Image.fromarray(pixels, "RGBA").save(path)


@pytest.fixture
def project_factory(tmp_path: Path) -> Callable[..., Path]:
    def make(
        *,
        frames: int = 6,
        fps: str = "30000/1001",
        with_layers: bool = True,
        layers: list[dict[str, Any]] | None = None,
        audio: dict[str, Any] | None = None,
        name: str = "project.json",
    ) -> Path:
        root = tmp_path / name.replace(".json", "")
        assets = root / "assets"
        assets.mkdir(parents=True, exist_ok=True)
        if with_layers:
            height, width = 64, 48
            yy, xx = np.mgrid[0:height, 0:width]
            base = np.empty((height, width, 4), dtype=np.uint8)
            base[..., 0] = (xx * 4 + 20).astype(np.uint8)
            base[..., 1] = (yy * 3 + 30).astype(np.uint8)
            base[..., 2] = ((xx + yy) * 2 + 15).astype(np.uint8)
            base[..., 3] = 255
            _save_rgba(assets / "base.png", base)
            overlay = np.zeros((16, 16, 4), dtype=np.uint8)
            overlay[..., 0] = 220
            overlay[..., 1] = 90
            overlay[..., 2] = 40
            overlay[..., 3] = np.arange(16, dtype=np.uint8)[None, :] * 15 + 15
            _save_rgba(assets / "overlay.png", overlay)
        if layers is None:
            layers = []
            if with_layers:
                layers = [
                    {
                        "id": "base", "path": "assets/base.png", "in": 0, "out": frames - 1,
                        "transform": {"anchor": [24, 32], "position": [24, 32]},
                    },
                    {
                        "id": "overlay", "path": "assets/overlay.png", "in": 0, "out": frames - 1,
                        "transform": {"anchor": [8, 8], "position": [24, 32]},
                    },
                ]
        project = {
            "fps": fps,
            "frame_count": frames,
            "canvas": {"width": 48, "height": 64, "background": [0, 0, 0, 255]},
            "layers": layers,
        }
        if audio is not None:
            project["audio"] = audio
        path = root / "project.json"
        path.write_text(json.dumps(project), encoding="utf-8")
        return path

    return make
