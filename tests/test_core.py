"""Focused tests for graph, automation, meters, and terminal errors."""

from __future__ import annotations

import json

import numpy as np
import pytest

from mixgraph.automation import AutomationCurve
from mixgraph.cli import main
from mixgraph.errors import GraphError, ManifestError
from mixgraph.graph import Graph
from mixgraph.meters import Meter
from mixgraph.offline import analyze_project
from mixgraph.project import load_project
from fixtures.gen import generate


def test_automation_sorts_and_ramps_at_sample_resolution():
    curve = AutomationCurve([(1.0, -10.0), (0.0, 0.0), (0.25, -6.0)])
    assert np.array_equal(curve.breakpoints[:, 0], [0.0, 0.25, 1.0])
    values = curve.sample_db(np.array([0.0, 0.125, 0.625, 1.0]))
    assert np.allclose(values, [0.0, -3.0, -8.0, -10.0])
    per_sample = curve.gain_db_at_frames(12000, 4, 48000)
    assert np.all(np.diff(per_sample) <= 0.0)


def test_exponential_interpolation_rejects_non_positive_gain():
    with pytest.raises(ManifestError, match="positive gain"):
        AutomationCurve(
            [{"time": 0.0, "gain": 1.0}, {"time": 1.0, "gain": 0.0}],
            interpolation="exponential",
        )


def test_graph_detects_cycles_and_dangling_ports():
    graph = Graph()
    graph.add_node("a", "gain")
    graph.add_node("b", "gain")
    graph.connect("a", "b")
    graph.connect("b", "a")
    with pytest.raises(GraphError, match="cycle"):
        graph.validate()

    dangling = Graph()
    dangling.add_node("source", "source")
    with pytest.raises(GraphError, match="dangling"):
        dangling.validate()

    dangling_sum = Graph()
    dangling_sum.add_node("source", "source")
    dangling_sum.add_node("total", "sum")
    dangling_sum.connect("source", "total")
    dangling_sum.add_node("out", "output")
    with pytest.raises(GraphError, match="dangling"):
        dangling_sum.validate()


def test_meter_clip_segments_continue_across_blocks():
    meter = Meter(ceiling_db=0.0)
    meter.update(np.array([[0.5], [1.1], [1.2], [0.4]]), absolute_start=0)
    meter.update(np.array([[0.9], [1.5], [2.0], [0.2]]), absolute_start=4)
    report = meter.report()
    assert report["clip_samples"] == 4
    assert report["clip_segments"] == 2
    assert report["first_clip_sample"] == 1


def _write_manifest(tmp_path, *, node_type="sum", sample_rate=48000):
    manifest = {
        "sample_rate": sample_rate,
        "channels": 2,
        "duration": 0.1,
        "nodes": [{"id": "only", "type": node_type}],
        "edges": [],
        "tracks": [],
    }
    path = tmp_path / "project.json"
    path.write_text(json.dumps(manifest), encoding="utf-8")
    return path


def test_cli_exit_codes_for_unknown_node_and_zero_rate(tmp_path):
    unknown = _write_manifest(tmp_path, node_type="mystery")
    assert main(["inspect", "--project", str(unknown)]) == 3
    zero_rate = _write_manifest(tmp_path, sample_rate=0)
    assert main(["inspect", "--project", str(zero_rate)]) == 2


def test_fixture_generation_is_byte_reproducible(tmp_path):
    first, first_files = generate(tmp_path / "first", seed=7)
    second, second_files = generate(tmp_path / "second", seed=7)
    assert first.read_bytes() == second.read_bytes()
    assert [path.read_bytes() for path in first_files] == [
        path.read_bytes() for path in second_files
    ]


def test_solo_overrides_mute(tmp_path):
    manifest_path, _ = generate(tmp_path / "fixtures", seed=7)
    manifest = json.loads(manifest_path.read_text())
    for track in manifest["tracks"]:
        if track["id"] == "dialogue":
            track["solo"] = True
            track["mute"] = True
    modified = manifest_path.with_name("solo.json")
    modified.write_text(json.dumps(manifest), encoding="utf-8")
    project = load_project(modified)
    report = analyze_project(project, 2048)
    levels = {track["id"]: track for track in report["tracks"]}
    assert levels["dialogue"]["active"] is True
    assert levels["dialogue"]["peak_dbfs"] > -20.0
    assert levels["music"]["active"] is False
    assert levels["music"]["peak_dbfs"] == -120.0

