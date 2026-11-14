from __future__ import annotations

import json

import numpy as np
import pytest

from tlcomp.blend import blend_premultiplied
from tlcomp.compositor import Compositor, premultiplied_to_rgba8
from tlcomp.raster import to_premultiplied
from tlcomp.timeline import load_timeline


def _pixel(rgb: tuple[int, int, int], alpha: int = 255) -> np.ndarray:
    return np.array([[rgb + (alpha,)]], dtype=np.uint8)


@pytest.mark.parametrize(
    ("mode", "expected"),
    [
        ("normal", (0.5, 0.5, 0.5)),
        ("multiply", (0.1, 0.2, 0.3)),
        ("screen", (0.6, 0.7, 0.8)),
        ("overlay", (0.2, 0.4, 0.6)),
        ("add", (0.7, 0.9, 1.0)),
    ],
)
def test_blend_modes_with_opaque_pixels(mode: str, expected: tuple[float, float, float]) -> None:
    backdrop = to_premultiplied(_pixel((51, 102, 153)))
    source = to_premultiplied(_pixel((128, 128, 128)))
    result = blend_premultiplied(backdrop, source, mode)
    assert result[0, 0, :3] == pytest.approx(expected, abs=2e-3)
    assert result[0, 0, 3] == pytest.approx(1.0)


def test_alpha_and_opacity_are_weighted_and_clamped() -> None:
    backdrop = to_premultiplied(_pixel((20, 40, 60)))
    source = to_premultiplied(_pixel((200, 100, 50), 128))
    result = blend_premultiplied(backdrop, source, "normal", opacity=0.5)
    effective_alpha = (128 / 255) * 0.5
    expected = np.array((200, 100, 50)) * effective_alpha + np.array((20, 40, 60)) * (1 - effective_alpha)
    assert result[0, 0, :3] == pytest.approx(expected / 255, abs=1e-6)
    assert result[0, 0, 3] == pytest.approx(1.0)
    assert np.isfinite(result).all()
    assert np.all(result >= 0.0) and np.all(result <= 1.0)


def test_zero_alpha_source_leaves_backdrop_unchanged() -> None:
    backdrop = to_premultiplied(_pixel((81, 92, 103), 77))
    source = to_premultiplied(_pixel((250, 250, 250), 0))
    for mode in ("normal", "multiply", "screen", "overlay", "add"):
        result = blend_premultiplied(backdrop, source, mode, opacity=0.0)
        np.testing.assert_allclose(result, backdrop, atol=1e-7)
        assert np.isfinite(result).all()


def test_fixed_half_up_rounding() -> None:
    premultiplied = np.zeros((1, 1, 4), dtype=np.float32)
    premultiplied[0, 0, 3] = np.float32(1.0)
    premultiplied[0, 0, :3] = np.array([0.5, 1.5, 2.5], dtype=np.float32) / 255.0
    result = premultiplied_to_rgba8(premultiplied)
    assert tuple(result[0, 0]) == (1, 2, 3, 255)


def test_compositor_identity_and_cropping(project_factory) -> None:
    project = project_factory()
    timeline = load_timeline(project)
    frame = Compositor(timeline).render(0)
    assert frame.shape == (64, 48, 4)
    assert tuple(frame[0, 0]) == (20, 30, 15, 255)
    assert tuple(frame[32, 24, :3]) != (20, 30, 15)

    data = json.loads(project.read_text())
    data["layers"] = [data["layers"][0]]
    project.write_text(json.dumps(data))
    base_only = Compositor(load_timeline(project)).render(0)
    data = json.loads(project.read_text())
    data["layers"] = [data["layers"][0], {
        "id": "offscreen", "path": "assets/overlay.png", "in": 0, "out": 5,
        "transform": {"anchor": [8, 8], "position": [-100, -100]},
    }]
    project.write_text(json.dumps(data))
    cropped = Compositor(load_timeline(project)).render(0)
    np.testing.assert_array_equal(cropped, base_only)


def test_keyframed_rotation_and_scale_change_pixels(project_factory) -> None:
    project = project_factory(frames=5, layers=[])
    data = json.loads(project.read_text())
    data["layers"] = [{
        "id": "moving", "path": "assets/overlay.png", "in": 0, "out": 4,
        "transform": {"anchor": [8, 8]},
        "keyframes": [
            {"frame": 0, "position": [24, 32], "scale": [1, 1], "rotation": 0, "opacity": 1, "easing": "ease-in-out"},
            {"frame": 4, "position": [24, 32], "scale": [2, 2], "rotation": 45, "opacity": 0.5},
        ],
    }]
    project.write_text(json.dumps(data))
    compositor = Compositor(load_timeline(project))
    first, last = compositor.render(0), compositor.render(4)
    assert not np.array_equal(first, last)
    assert first[..., 3].max() == 255
