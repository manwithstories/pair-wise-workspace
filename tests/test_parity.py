"""Offline and streamed paths must agree for arbitrary block boundaries."""

from __future__ import annotations

import numpy as np
import pytest
import soundfile as sf

from fixtures.gen import generate
from mixgraph.offline import analyze_project, render_offline
from mixgraph.project import load_project
from mixgraph.stream import render_stream


@pytest.fixture(scope="session")
def fixture_project(tmp_path_factory):
    root = tmp_path_factory.mktemp("fixtures")
    manifest, _ = generate(root, seed=7)
    return load_project(manifest)


def _load(path) -> np.ndarray:
    data, _ = sf.read(str(path), dtype="float64", always_2d=True)
    return data


def test_offline_stream_parity_across_block_sizes(fixture_project, tmp_path):
    paths = {}
    for block in (64, 97, 1024):
        path = tmp_path / f"offline_{block}.wav"
        render_offline(fixture_project, path, block)
        paths[f"offline_{block}"] = path
    stream_path = tmp_path / "stream_97.wav"
    render_stream(fixture_project, stream_path, 97)
    paths["stream_97"] = stream_path

    reference = _load(paths["offline_64"])
    reference_bytes = paths["offline_64"].read_bytes()
    for label, path in paths.items():
        candidate = _load(path)
        assert candidate.shape == reference.shape
        difference = float(np.max(np.abs(candidate - reference)))
        assert difference <= 1e-9, f"{label} differs by {difference}"
        assert path.read_bytes() == reference_bytes, f"{label} WAV bytes differ"


def test_report_contract(fixture_project):
    report = analyze_project(fixture_project, 2048)
    assert {track["id"] for track in report["tracks"]} == {
        "dialogue",
        "music",
        "ambience",
    }
    for track in report["tracks"]:
        assert -20.0 <= track["peak_dbfs"] <= -6.0
        assert track["rms_dbfs"] < track["peak_dbfs"]
        assert 40 <= len(track["automation_samples"]) <= 200
        ratio = track["resample_ratio"]
        assert isinstance(ratio["numerator"], int)
        assert isinstance(ratio["denominator"], int)
        assert np.gcd(ratio["numerator"], ratio["denominator"]) == 1
    assert report["clip_total"] == 0
    assert report["master_peak_dbfs"] <= 0.0
    assert report["buses"]


def test_stream_pull_blocks_are_fixed_except_tail(fixture_project):
    from mixgraph.stream import StreamRenderer

    renderer = StreamRenderer(fixture_project, 97)
    try:
        blocks = []
        while renderer.frames_remaining:
            blocks.append(renderer.pull())
        assert all(block.shape[0] == 97 for block in blocks[:-1])
        assert 0 < blocks[-1].shape[0] <= 97
    finally:
        renderer.close()

