import json
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from mixgraph import cli
from mixgraph.assets import generate_fixtures
from mixgraph.graph import MixGraph, parse_graph
from mixgraph.offline import compare_paths, render_offline
from mixgraph.report import build_report
from mixgraph.streaming import MixEngine, stream_project


@pytest.fixture(scope="module")
def demo(tmp_path_factory):
    base = tmp_path_factory.mktemp("demo")
    project = base / "demo/project.json"
    generate_fixtures(base / "assets", 7, project)
    graph = parse_graph(json.loads(project.read_text(encoding="utf-8")))
    return base, project, graph


def test_offline_stream_metrics_and_block_independence(demo, tmp_path):
    base, project, graph = demo
    offline_metrics = render_offline(graph, project.parent, tmp_path / "offline.wav")
    reports = [build_report(offline_metrics)]
    for block in (64, 256, 1024):
        metrics = stream_project(
            graph, project.parent, block, tmp_path / f"stream_{block}.wav"
        )
        reports.append(build_report(metrics))
    assert all(report == reports[0] for report in reports[1:])
    assert len(reports[0]["tracks"]) == 4
    assert len(reports[0]["buses"]) == 2
    for metric in reports[0]["tracks"]:
        assert -12.0 <= metric["peak_dbfs"] <= -0.5
        assert -20.1 <= metric["rms_dbfs"] <= -14.0
        assert 0 <= metric["clip_samples"] <= 9
    maximum, compared = compare_paths(graph, project.parent, 4096, 256)
    assert maximum <= 1e-6
    assert compared == 288000


def test_solo_does_not_hide_track_details(demo):
    base, project, graph = demo
    baseline_engine = MixEngine(graph, project.parent)
    baseline_tracks = {}
    while baseline_engine.position < baseline_engine.total_frames:
        baseline_engine.process(min(2048, baseline_engine.total_frames - baseline_engine.position))
    for track in graph.tracks:
        baseline_tracks[track.name] = baseline_engine.meters.total(track.name).as_dict()
    baseline_engine.close()

    solo_graph = parse_graph(json.loads(project.read_text(encoding="utf-8")))
    solo_graph.tracks[0].solo = True
    solo_engine = MixEngine(solo_graph, project.parent)
    while solo_engine.position < solo_engine.total_frames:
        solo_engine.process(min(2048, solo_engine.total_frames - solo_engine.position))
    for track in solo_graph.tracks:
        assert solo_engine.meters.total(track.name).as_dict() == baseline_tracks[track.name]
    solo_engine.close()


def test_cli_acceptance_commands(demo, capsys):
    base, project, graph = demo
    output = base / "outputs/mix.wav"
    report = base / "outputs/render.json"
    assert cli.main(["render", "--project", str(project), "--out", str(output), "--report", str(report)]) == 0
    assert cli.main([
        "stream", "--project", str(project), "--block", "256",
        "--out", str(base / "outputs/stream.wav"), "--report", str(base / "outputs/stream.json"),
    ]) == 0
    assert cli.main(["curves", "--project", str(project), "--node", "vo", "--times", "0,1.25,3.5"]) == 0
    assert cli.main(["verify", "--project", str(project), "--tol", "1e-6"]) == 0
    assert json.loads(report.read_text())["tracks"][0]["name"] == "vo"


def test_cli_error_codes(demo, tmp_path, monkeypatch, capsys):
    base, project, _ = demo
    assert cli.main(["stream", "--project", str(project), "--block", "100", "--out", str(tmp_path / "x.wav"), "--report", str(tmp_path / "x.json")]) == 2
    missing = json.loads(project.read_text())
    missing["tracks"][0]["source"] = "missing.wav"
    missing_project = tmp_path / "missing.json"
    missing_project.write_text(json.dumps(missing))
    assert cli.main(["verify", "--project", str(missing_project)]) == 3
    empty = json.loads(project.read_text())
    empty["tracks"][0]["automation"] = {"points": []}
    empty_project = tmp_path / "empty.json"
    empty_project.write_text(json.dumps(empty))
    assert cli.main(["verify", "--project", str(empty_project)]) == 4
    bad_rate = json.loads(project.read_text())
    bad_rate["sample_rate"] = 0
    bad_rate_project = tmp_path / "rate.json"
    bad_rate_project.write_text(json.dumps(bad_rate))
    assert cli.main(["verify", "--project", str(bad_rate_project)]) == 4
    monkeypatch.setattr(cli, "compare_paths", lambda graph, directory: (1e-4, 100))
    assert cli.main(["verify", "--project", str(project), "--tol", "1e-6"]) == 5


def test_invalid_source_channels_is_asset_error(tmp_path):
    source = tmp_path / "three.wav"
    sf.write(str(source), np.zeros((100, 3)), 48000)
    project = tmp_path / "project.json"
    project.write_text(json.dumps({
        "sample_rate": 48000,
        "tracks": [{"name": "bad", "source": "three.wav", "sends": [{"bus": "master"}]}],
        "buses": [],
        "master": {},
    }))
    assert cli.main(["verify", "--project", str(project)]) == 3
