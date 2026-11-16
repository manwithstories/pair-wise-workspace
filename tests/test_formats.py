"""Format, layout and validation failures must name the offending track."""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from src.mixer.errors import EXIT_FORMAT, MixerError
from src.mixer.io import SourceFile, probe_wav
from src.mixer.model import ChannelLayout, parse_automation, parse_project
from src.mixer import cli

FIXTURES = Path(__file__).resolve().parent.parent / "fixtures"


def _write(path: Path, rate: int, channels: int, frames: int = 4800, subtype: str = "FLOAT"):
    data = np.zeros((frames, channels), dtype=np.float64)
    data[:, 0] = np.linspace(0.0, 0.5, frames)
    sf.write(str(path), data, rate, subtype=subtype, format="WAV")


def _project_for(paths: dict[str, str], layouts: dict[str, str] | None = None) -> dict:
    """Pad to the required four tracks, reusing the named file for fillers."""
    layouts = layouts or {}
    tracks = [
        {"name": name, "file": file_name, "layout": layouts.get(name, "stereo")}
        for name, file_name in paths.items()
    ]
    filler_file = next(iter(paths.values()))
    while len(tracks) < 4:
        index = len(tracks)
        tracks.append({"name": f"filler{index}", "file": filler_file, "layout": "stereo"})
    return {"schema": "mix-project/1", "target_rate": 48000, "tracks": tracks}


def test_unsupported_rate_names_track(tmp_path):
    bad = tmp_path / "bad_rate.wav"
    _write(bad, 22050, 2)
    with pytest.raises(MixerError) as excinfo:
        SourceFile(bad, "bass", ChannelLayout.parse("stereo"))
    error = excinfo.value
    assert error.code == "invalid_rate"
    assert error.exit_code == EXIT_FORMAT
    assert error.track == "bass"
    assert "22050" in str(error)


def test_truncated_file_names_track(tmp_path):
    good = tmp_path / "good.wav"
    _write(good, 48000, 2)
    truncated = tmp_path / "truncated.wav"
    truncated.write_bytes(good.read_bytes()[:-400])
    with pytest.raises(MixerError) as excinfo:
        SourceFile(truncated, "pad", ChannelLayout.parse("stereo"))
    assert excinfo.value.code == "truncated_file"
    assert excinfo.value.track == "pad"
    assert excinfo.value.exit_code == EXIT_FORMAT


def test_non_finite_samples_abort(tmp_path):
    path = tmp_path / "nan.wav"
    _write(path, 48000, 1)
    data, rate = sf.read(str(path), always_2d=True)
    data[10, 0] = np.nan
    sf.write(str(path), data, rate, subtype="FLOAT", format="WAV")
    with pytest.raises(MixerError) as excinfo:
        SourceFile(path, "drums", ChannelLayout.parse("mono"))
    assert excinfo.value.code == "non_finite_samples"
    assert excinfo.value.track == "drums"


def test_infinite_sample_abort(tmp_path):
    path = tmp_path / "inf.wav"
    _write(path, 48000, 2)
    data, rate = sf.read(str(path), always_2d=True)
    data[5, 1] = np.inf
    sf.write(str(path), data, rate, subtype="FLOAT", format="WAV")
    with pytest.raises(MixerError) as excinfo:
        SourceFile(path, "fx", ChannelLayout.parse("stereo"))
    assert excinfo.value.code == "non_finite_samples"


def test_channel_count_mismatch(tmp_path):
    path = tmp_path / "mono.wav"
    _write(path, 48000, 1)
    with pytest.raises(MixerError) as excinfo:
        SourceFile(path, "bass", ChannelLayout.parse("stereo"))
    assert excinfo.value.code == "channel_count_mismatch"
    assert excinfo.value.details["expected"] == 2
    assert excinfo.value.details["actual"] == 1


def test_unknown_layout_rejected():
    with pytest.raises(MixerError) as excinfo:
        ChannelLayout.parse("5.1")
    assert excinfo.value.code == "unknown_channel_layout"
    assert excinfo.value.exit_code == EXIT_FORMAT


def test_invalid_channel_matrix_rejected():
    layout = ChannelLayout.parse("quad")
    with pytest.raises(MixerError) as excinfo:
        layout.matrix_for([[1.0, 0.0], [0.0, 1.0]])
    assert excinfo.value.code == "invalid_channel_matrix"


def test_missing_file_is_a_format_error(tmp_path):
    with pytest.raises(MixerError) as excinfo:
        probe_wav(tmp_path / "nope.wav")
    assert excinfo.value.code == "file_not_found"


def test_automation_duplicates_keep_last_declaration():
    from src.mixer.model import AutomationPoint

    points = parse_automation(
        [
            {"time": 0.5, "gain_db": 1.0},
            {"time": 0.0, "gain_db": 0.0},
            {"time": 0.5, "gain_db": -4.0},
        ],
        "bass",
    )
    assert [(p.time, p.gain_db) for p in points] == [(0.0, 0.0), (0.5, -4.0)]
    assert points == [AutomationPoint(0.0, 0.0), AutomationPoint(0.5, -4.0)]


def test_cli_reports_format_error_with_exit_3(tmp_path, capsys):
    bad = tmp_path / "bad.wav"
    _write(bad, 22050, 2)
    project = _project_for({"bass": "bad.wav"}, {"bass": "stereo"})
    (tmp_path / "project.json").write_text(json.dumps(project), encoding="utf-8")
    code = cli.main(
        [
            "render",
            "--input",
            str(tmp_path / "project.json"),
            "--output",
            str(tmp_path / "out.wav"),
        ]
    )
    captured = capsys.readouterr()
    payload = json.loads(captured.out)
    assert code == EXIT_FORMAT
    assert payload["ok"] is False
    assert payload["error"]["code"] == "invalid_rate"
    assert payload["error"]["track"] == "bass"


def test_cli_missing_project_exits_2(tmp_path, capsys):
    code = cli.main(["render", "--input", str(tmp_path / "none.json"), "--output", "x.wav"])
    payload = json.loads(capsys.readouterr().out)
    assert code == 2
    assert payload["error"]["code"] == "project_not_found"


def test_cli_rejects_unsupported_stream_block(tmp_path, capsys):
    with pytest.raises(SystemExit) as excinfo:
        cli.main(["stream", "--input", "x.json", "--output", "y.wav", "--block", "512"])
    assert excinfo.value.code == 2  # argparse rejects the unsupported block size


def test_project_needs_four_tracks():
    project = {
        "schema": "mix-project/1",
        "target_rate": 48000,
        "tracks": [{"name": "only", "file": "a.wav", "layout": "stereo"}],
    }
    with pytest.raises(MixerError) as excinfo:
        parse_project(project, base_dir=Path("/tmp"))
    assert excinfo.value.code == "invalid_project"


def test_zero_length_source_is_an_empty_track(tmp_path):
    """A legal zero-frame WAV must not fail the whole project."""
    from src.mixer.engine import OfflineEngine
    from src.mixer.model import parse_project

    empty = tmp_path / "empty.wav"
    sf.write(str(empty), np.zeros((0, 2)), 48000, subtype="FLOAT", format="WAV")
    tone = tmp_path / "tone.wav"
    sf.write(str(tone), np.full((4800, 2), 0.2), 48000, subtype="FLOAT", format="WAV")
    raw = {
        "schema": "mix-project/1",
        "target_rate": 48000,
        "tracks": [
            {"name": "empty", "file": "empty.wav", "layout": "stereo"},
            {"name": "a", "file": "tone.wav", "layout": "stereo"},
            {"name": "b", "file": "tone.wav", "layout": "stereo"},
            {"name": "c", "file": "tone.wav", "layout": "stereo"},
        ],
    }
    result = OfflineEngine(parse_project(raw, base_dir=tmp_path)).render()
    assert result.timeline_frames == 4800


def test_header_without_data_chunk_is_rejected(tmp_path):
    good = tmp_path / "good.wav"
    _write(good, 48000, 2)
    raw = bytearray(good.read_bytes())
    index = raw.find(b"data")
    assert index > 0
    broken = tmp_path / "nodata.wav"
    broken.write_bytes(bytes(raw[:index]))
    with pytest.raises(MixerError) as excinfo:
        probe_wav(broken)
    # Cutting at the data chunk leaves the RIFF size overstated, which is a
    # truncation; a file that simply never declares data is invalid_wav.
    assert excinfo.value.code in {"truncated_file", "invalid_wav"}
