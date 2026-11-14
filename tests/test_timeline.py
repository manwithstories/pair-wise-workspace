from __future__ import annotations

import json

import pytest

from tlcomp.errors import ProjectError
from tlcomp.timeline import Fps, load_timeline


def test_fractional_fps_pts_and_sample_index() -> None:
    fps = Fps.parse("30000/1001")
    assert fps.pts_ms(0) == 0.0
    assert fps.pts_ms(1) == pytest.approx(33.3666666667)
    assert fps.pts_ms(30) == pytest.approx(1001.0)
    assert fps.sample_index(30, 2_400_000) == 2_402_400


@pytest.mark.parametrize("raw", ["0/1", "-30/1", "bad", "1/0", None, True])
def test_invalid_fps_is_project_error(raw: object) -> None:
    with pytest.raises(ProjectError):
        Fps.parse(raw)


def test_inclusive_layer_bounds_and_endpoint_holds(project_factory) -> None:
    project = project_factory(frames=10, with_layers=False, layers=[])
    data = json.loads(project.read_text())
    data["layers"] = [
        {
            "id": "x", "path": "assets/missing.png", "in": 2, "out": 4,
            "transform": {"anchor": [10, 10], "position": [10, 10]},
            "keyframes": [
                {"frame": 0, "position": [5, 6], "easing": "linear"},
                {"frame": 8, "position": [15, 16], "easing": "ease-in-out"},
            ],
        }
    ]
    project.write_text(json.dumps(data))
    timeline = load_timeline(project)
    layer = timeline.layers[0]
    assert [layer.is_active(frame) for frame in (1, 2, 3, 4, 5)] == [False, True, True, True, False]
    assert layer.state_at(2).position == pytest.approx((7.5, 8.5))
    assert layer.state_at(8).position == pytest.approx((15.0, 16.0))
    assert layer.state_at(99).position == pytest.approx((15.0, 16.0))


def test_ease_in_out_and_missing_field_validation(project_factory) -> None:
    project = project_factory(frames=10, with_layers=False, layers=[])
    data = json.loads(project.read_text())
    layer = {
        "id": "x", "path": "assets/missing.png", "in": 0, "out": 9,
        "transform": {"anchor": [10, 10]},
        "keyframes": [
            {"frame": 0, "position": [0, 0], "scale": [1, 1], "opacity": 0, "easing": "ease-in-out"},
            {"frame": 4, "position": [8, 0], "scale": [2, 2], "opacity": 1},
        ],
    }
    data["layers"] = [layer]
    project.write_text(json.dumps(data))
    state = load_timeline(project).layers[0].state_at(2)
    assert state.position == pytest.approx((4.0, 0.0))
    assert state.scale == pytest.approx((1.5, 1.5))
    assert state.opacity == pytest.approx(0.5)
    del layer["keyframes"][1]["scale"]
    project.write_text(json.dumps(data))
    with pytest.raises(ProjectError, match="same animated fields"):
        load_timeline(project)


def test_out_of_order_keyframes_are_rejected(project_factory) -> None:
    project = project_factory(frames=4, with_layers=False, layers=[])
    data = json.loads(project.read_text())
    data["layers"] = [{
        "id": "x", "path": "assets/missing.png", "in": 0, "out": 3,
        "keyframes": [
            {"frame": 2, "rotation": 0},
            {"frame": 2, "rotation": 90},
        ],
    }]
    project.write_text(json.dumps(data))
    with pytest.raises(ProjectError, match="strictly ordered"):
        load_timeline(project)
