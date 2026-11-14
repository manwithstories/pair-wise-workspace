import json
import math

import numpy as np
import pytest

from mixgraph.assets import generate_fixtures
from mixgraph.automation import Automation, AutomationError, AutomationPoint
from mixgraph.graph import GraphError, parse_graph
from mixgraph.meters import Meter
from mixgraph.resample import PolyphaseResampler


def test_automation_curves_and_endpoint_hold():
    curve = Automation(
        [
            AutomationPoint(0.0, -6.0, "linear"),
            AutomationPoint(1.0, 0.0, "exponential"),
            AutomationPoint(2.0, -6.0, "hold"),
            AutomationPoint(3.0, -3.0, "linear"),
        ]
    )
    times = np.array([-1.0, 0.0, 0.5, 1.0, 1.5, 2.5, 3.5])
    gains = curve.gain_at(times)
    assert gains[0] == pytest.approx(10 ** (-6 / 20))
    assert gains[2] == pytest.approx((10 ** (-6 / 20) + 1.0) / 2)
    assert gain_db(gains[3]) == pytest.approx(0.0)
    assert gain_db(gains[4]) == pytest.approx(-3.0)
    assert gain_db(gains[5]) == pytest.approx(-6.0)
    assert gain_db(gains[6]) == pytest.approx(-3.0)
    assert curve.gain_db_at(3.0) == pytest.approx(-3.0)


def gain_db(value):
    return 20.0 * math.log10(float(value))


def test_automation_rejects_empty_and_unsorted():
    with pytest.raises(AutomationError):
        Automation.from_json({"points": []}, "vo")
    with pytest.raises(AutomationError):
        Automation.from_json(
            {"points": [{"time": 1, "gain_db": 0}, {"time": 0, "gain_db": 1}]},
            "vo",
        )
    with pytest.raises(AutomationError):
        Automation.from_json({"points": [{"time": 0, "gain_db": 0, "curve": "bad"}]}, "vo")


def base_project():
    return {
        "sample_rate": 48000,
        "tracks": [
            {"name": "vo", "source": "vo.wav", "sends": [{"bus": "dialogue"}]},
            {"name": "music", "source": "music.wav", "sends": [{"bus": "music_bus"}]},
        ],
        "buses": [
            {"name": "dialogue", "sends": [{"bus": "master"}]},
            {"name": "music_bus", "sends": [{"bus": "master"}]},
        ],
        "master": {"name": "master"},
    }


def test_graph_orders_and_rejects_duplicate_cycle_dangling():
    graph = parse_graph(base_project())
    assert graph.process_order[:2] == ["vo", "music"]
    assert set(graph.process_order[2:]) == {"dialogue", "music_bus", "master"}

    duplicate = base_project()
    duplicate["buses"].append({"name": "vo", "sends": [{"bus": "master"}]})
    with pytest.raises(GraphError, match="duplicate node name 'vo'"):
        parse_graph(duplicate)

    cycle = base_project()
    cycle["buses"] = [
        {"name": "a", "sends": [{"bus": "b"}]},
        {"name": "b", "sends": [{"bus": "a"}, {"bus": "master"}]},
    ]
    cycle["tracks"] = [{"name": "vo", "source": "vo.wav", "sends": [{"bus": "a"}]}]
    with pytest.raises(GraphError, match="cycle detected"):
        parse_graph(cycle)

    dangling = base_project()
    dangling["buses"][0] = {"name": "dialogue", "sends": []}
    with pytest.raises(GraphError, match="dangling bus 'dialogue'"):
        parse_graph(dangling)

    missing = base_project()
    missing["tracks"][0]["sends"] = [{"bus": "not_there"}]
    with pytest.raises(GraphError, match="not_there"):
        parse_graph(missing)


@pytest.mark.parametrize("rate", [44100, 96000])
def test_resampler_block_invariance_and_duration(rate):
    frames = rate // 4
    time = np.arange(frames) / rate
    source = np.sin(2 * math.pi * 440 * time)[None, :]
    outputs = []
    for chunks in ([frames], [64] * (frames // 64) + [frames % 64]):
        resampler = PolyphaseResampler(rate, 48000, 1, frames)
        parts = []
        offset = 0
        for chunk in chunks:
            if chunk:
                parts.append(resampler.process(source[:, offset : offset + chunk]))
                offset += chunk
        parts.append(resampler.finish())
        outputs.append(np.concatenate(parts, axis=1))
    assert outputs[0].shape[1] == round(frames * 48000 / rate)
    assert abs(outputs[0].shape[1] - frames * 48000 / rate) <= 1
    assert np.max(np.abs(outputs[0] - outputs[1])) == 0.0


def test_resampler_suppresses_high_frequencies_before_downsampling():
    rate = 96000
    frames = rate // 4
    time = np.arange(frames) / rate
    source = np.sin(2 * math.pi * 30000 * time)[None, :]
    resampler = PolyphaseResampler(rate, 48000, 1, frames)
    output = np.concatenate([resampler.process(source), resampler.finish()], axis=1)
    rms = math.sqrt(float(np.mean(output ** 2)))
    assert rms < 0.01


def test_meter_counts_clipping_strictly_above_one():
    meter = Meter("test")
    block = meter.update(np.array([[0.5, -1.0], [1.0001, 0.0]]))
    assert block.clip_samples == 1
    assert meter.total().clip_samples == 1
    assert block.peak_dbfs > 0.0
