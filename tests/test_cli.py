"""CLI contract: exit codes, compare verdicts and inspect reports."""

from __future__ import annotations

import json
import shutil
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from src.mixer import cli


@pytest.fixture()
def rendered(fixture_dir: Path, tmp_path: Path):
    offline = tmp_path / "offline.wav"
    stream = tmp_path / "stream.wav"
    assert cli.main(["render", "--input", str(fixture_dir / "project.json"),
                     "--output", str(offline), "--block", "1024"]) == 0
    assert cli.main(["stream", "--input", str(fixture_dir / "project.json"),
                     "--output", str(stream), "--block", "256"]) == 0
    return offline, stream, fixture_dir / "project.json"


def test_render_reports_acceptance_fields(fixture_dir, tmp_path, capsys):
    output = tmp_path / "out.wav"
    code = cli.main(["render", "--input", str(fixture_dir / "project.json"),
                     "--output", str(output)])
    captured = capsys.readouterr()
    payload = json.loads(captured.out)
    assert code == 0
    assert payload["tracks"] == 4
    assert payload["target_rate"] == 48000
    assert payload["frames"] > 0
    assert 8 <= payload["automation"]["total"] <= 32
    assert payload["limiter"]["peak_out_dbfs"] <= 0.0
    assert payload["limiter"]["peak_out_dbfs"] >= -1.0
    assert payload["output"]["subtype"] in {"FLOAT", "DOUBLE"}
    assert output.exists()
    assert "tracks=4 target_rate=48000" in captured.err


def test_stream_reports_block_count_and_tail(fixture_dir, tmp_path, capsys):
    output = tmp_path / "out.wav"
    code = cli.main(["stream", "--input", str(fixture_dir / "project.json"),
                     "--output", str(output), "--block", "256"])
    payload = json.loads(capsys.readouterr().out)
    assert code == 0
    assert payload["block_size"] == 256
    assert payload["blocks"] > 1
    assert 0 <= payload["tail_block"] <= 256


def test_compare_matches_and_reports_error(rendered, capsys):
    offline, stream, project = rendered
    code = cli.main(["compare", "--offline", str(offline), "--stream", str(stream),
                     "--automation", str(project)])
    payload = json.loads(capsys.readouterr().out)
    assert code == 0
    assert payload["verdict"] == "match"
    assert payload["max_abs_error"] <= 1e-5
    assert payload["automation"]["total"] >= 8
    assert len(payload["track_stats"]) == 4


def test_compare_mismatch_exits_4(rendered, capsys):
    offline, stream, project = rendered
    data, rate = sf.read(str(stream), always_2d=True)
    data[1000:1010] += 0.25
    sf.write(str(stream), data, rate, subtype="FLOAT", format="WAV")
    code = cli.main(["compare", "--offline", str(offline), "--stream", str(stream),
                     "--automation", str(project)])
    captured = capsys.readouterr()
    payload = json.loads(captured.out)
    assert code == 4
    assert payload["ok"] is False
    assert payload["error"]["code"] == "render_mismatch"
    assert payload["error"]["details"]["max_abs_error"] > 1e-5
    assert "mismatch" in payload["error"]["message"]


def test_compare_frame_count_mismatch_exits_4(rendered, capsys):
    offline, stream, _ = rendered
    data, rate = sf.read(str(stream), always_2d=True)
    sf.write(str(stream), data[:-100], rate, subtype="FLOAT", format="WAV")
    code = cli.main(["compare", "--offline", str(offline), "--stream", str(stream)])
    payload = json.loads(capsys.readouterr().out)
    assert code == 4
    assert payload["error"]["details"]["frame_delta"] == 100


def test_compare_rate_mismatch_exits_4(tmp_path, capsys):
    a = tmp_path / "a.wav"
    b = tmp_path / "b.wav"
    sf.write(str(a), np.zeros((100, 2)), 48000, subtype="FLOAT", format="WAV")
    sf.write(str(b), np.zeros((100, 2)), 44100, subtype="FLOAT", format="WAV")
    code = cli.main(["compare", "--offline", str(a), "--stream", str(b)])
    payload = json.loads(capsys.readouterr().out)
    assert code == 4
    assert payload["error"]["code"] == "rate_mismatch"


def test_compare_missing_file_is_format_error(tmp_path, capsys):
    a = tmp_path / "a.wav"
    sf.write(str(a), np.zeros((10, 2)), 48000, subtype="FLOAT", format="WAV")
    code = cli.main(["compare", "--offline", str(a), "--stream", str(tmp_path / "nope.wav")])
    payload = json.loads(capsys.readouterr().out)
    assert code == 3
    assert payload["error"]["code"] == "file_not_found"


def test_inspect_project_and_audio(rendered, capsys):
    _, _, project = rendered
    code = cli.main(["inspect", "--path", str(project)])
    payload = json.loads(capsys.readouterr().out)
    assert code == 0
    assert payload["kind"] == "project"
    assert [t["name"] for t in payload["tracks"]] == ["drums", "bass", "pad", "fx"]
    assert payload["tracks"][2]["layout"] == "quad"
    assert payload["tracks"][3]["clips"] == 2

    code = cli.main(["inspect", "--path", str(rendered[0])])
    payload = json.loads(capsys.readouterr().out)
    assert code == 0
    assert payload["kind"] == "audio"
    assert payload["audio"]["channels"] == 2
    assert payload["audio"]["sample_rate"] == 48000


def test_unknown_layout_in_project_exits_3(fixture_dir, tmp_path, capsys):
    raw = json.loads((fixture_dir / "project.json").read_text())
    raw["tracks"][0]["layout"] = "5.1"
    bad = fixture_dir / "bad_layout.json"
    bad.write_text(json.dumps(raw), encoding="utf-8")
    code = cli.main(["render", "--input", str(bad), "--output", str(tmp_path / "o.wav")])
    payload = json.loads(capsys.readouterr().out)
    assert code == 3
    assert payload["error"]["code"] == "unknown_channel_layout"


def test_bus_referencing_unknown_track_exits_2(fixture_dir, tmp_path, capsys):
    raw = json.loads((fixture_dir / "project.json").read_text())
    raw["buses"] = [{"name": "ghost", "tracks": ["not-a-track"]}]
    # Write beside the sources: project file paths resolve relative to the JSON.
    bad = fixture_dir / "bad_bus.json"
    bad.write_text(json.dumps(raw), encoding="utf-8")
    code = cli.main(["render", "--input", str(bad), "--output", str(tmp_path / "o.wav")])
    payload = json.loads(capsys.readouterr().out)
    assert code == 2
    assert payload["error"]["code"] == "graph_unknown_node"


def test_generate_fixtures_via_cli(tmp_path, capsys):
    out = tmp_path / "generated"
    code = cli.main(["generate", "--out", str(out), "--seed", "99"])
    payload = json.loads(capsys.readouterr().out)
    assert code == 0
    assert payload["tracks"] == ["drums", "bass", "pad", "fx"]
    assert (out / "project.json").exists()
    assert len(list(out.glob("*.wav"))) == 4
    # Regenerating with the same seed is byte-identical apart from PEAK stamps.
    first = (out / "project.json").read_text()
    other = tmp_path / "again"
    assert cli.main(["generate", "--out", str(other), "--seed", "99"]) == 0
    assert (other / "project.json").read_text() == first
