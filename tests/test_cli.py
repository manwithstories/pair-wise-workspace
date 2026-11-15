"""End-to-end tests for the gen/verify CLI."""
import json

from isobmff.cli import main
from isobmff.fixtures import FRAME_DURATION


def gen(tmp_path, name, **kw):
    args = ["gen", "--out", name]
    for key, value in kw.items():
        if value is not None:
            args += [f"--{key}", str(value)]
    assert main(args) == 0
    return tmp_path / name


def test_ok_fixture_passes(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    gen(tmp_path, "ok.mp4", samples=240, fragments=6, seed=7)
    assert main(["verify", "ok.mp4", "--json", "ok.json"]) == 0
    data = json.loads((tmp_path / "ok.json").read_text())
    assert data["sample_count"] == 240
    assert data["sidx_entries"] == 6
    assert data["offset_check"]["status"] == "pass"
    assert data["offset_check"]["in_range"] == 240
    assert data["offset_check"]["total"] == 240
    assert len(data["rebuild_boundaries"]) == 6
    assert all(b.startswith("0x") for b in data["rebuild_boundaries"])
    assert data["duration_delta_total"] == 0
    assert data["verdict"] == "PASS"


def test_corrupt_duration_fails_validation(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    gen(tmp_path, "bad.mp4", samples=240, fragments=6,
        corrupt="duration", seed=7)
    assert main(["verify", "bad.mp4", "--json", "bad.json"]) == 4
    data = json.loads((tmp_path / "bad.json").read_text())
    assert data["duration_delta_total"] == FRAME_DURATION
    assert data["verdict"] == "FAIL"
    assert data["first_mismatch"]["index"] == 5


def test_corrupt_offset_fails_validation(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    gen(tmp_path, "bad.mp4", corrupt="offset")
    assert main(["verify", "bad.mp4"]) == 4


def test_corrupt_stsc_is_structural(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    gen(tmp_path, "bad.mp4", corrupt="stsc")
    assert main(["verify", "bad.mp4"]) == 3


def test_corrupt_truncate_is_structural(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    gen(tmp_path, "bad.mp4", corrupt="truncate")
    assert main(["verify", "bad.mp4"]) == 3


def test_same_seed_is_byte_identical(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    first = gen(tmp_path, "a.mp4", seed=42)
    second = gen(tmp_path, "b.mp4", seed=42)
    assert first.read_bytes() == second.read_bytes()
    third = gen(tmp_path, "c.mp4", seed=43)
    assert first.read_bytes() != third.read_bytes()


def test_batch_directory_mode(tmp_path, monkeypatch, capsys):
    monkeypatch.chdir(tmp_path)
    gen(tmp_path, "ok.mp4", seed=7)
    gen(tmp_path, "bad.mp4", corrupt="duration", seed=7)
    code = main(["verify", ".", "--json", "reports"])
    assert code == 4
    out = capsys.readouterr().out
    assert "1/2 files PASS" in out
    assert (tmp_path / "reports" / "ok.json").is_file()
    assert (tmp_path / "reports" / "bad.json").is_file()
