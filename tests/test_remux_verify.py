from __future__ import annotations

import json
from pathlib import Path

import pytest

from make_fixtures import generate
from mkvmp4.cli import main
from mkvmp4.remux import remux_file
from mkvmp4.verify import verify_file


@pytest.fixture(scope="session")
def sample_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("media")
    generate(root, 4)
    return root


def test_full_remux_manifest_and_verify(sample_dir: Path, tmp_path: Path) -> None:
    output = tmp_path / "case_b.mp4"
    manifest_path = tmp_path / "remux.json"
    manifest = remux_file(str(sample_dir / "case_b_restart.mkv"), str(output),
                          manifest_path=str(manifest_path), repair="clamp")
    tracks = manifest["tracks"]
    assert [(track["track_ID"], track["codec"], track["samples"],
             track["first_pts"], track["last_pts"]) for track in tracks] == [
        (1, "avc1", 300, 0.0, 9.96),
        (2, "avc1", 300, 0.0, 9.96),
        (3, "mp4a", 431, 0.0, 9.984),
        (4, "tx3g", 18, 0.0, 8.5),
    ]
    assert manifest["repairs"]["duplicate"] == 2
    assert manifest["repairs"]["backward"] == 1
    assert manifest["repairs"]["gap"] == 3
    assert manifest["cues_rebuilt"] == 5
    assert manifest["mdat_bytes"] == 24_000_000
    assert verify_file(str(output), str(manifest_path))["ok"] is True
    stored = json.loads(manifest_path.read_text())
    assert stored["moov_before_mdat"] is True


def test_crc_and_all_laces_round_trip(sample_dir: Path, tmp_path: Path) -> None:
    output = tmp_path / "case_c.mp4"
    manifest = tmp_path / "case_c.json"
    remux_file(str(sample_dir / "case_c_lace.mkv"), str(output),
               manifest_path=str(manifest))
    report = verify_file(str(output), str(manifest))
    assert report["ok"] is True
    assert report["tracks"][0]["samples"] == 50


def test_cli_exit_codes(sample_dir: Path, tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    assert main(["remux", str(sample_dir / "case_d_unsupported.mkv"),
                 "-o", str(tmp_path / "bad.mp4")]) == 3
    assert main(["remux", str(sample_dir / "case_b_restart.mkv"),
                 "-o", str(tmp_path / "fail.mp4"), "--repair", "fail"]) == 4
    assert main(["remux", str(sample_dir / "case_b_restart.mkv"),
                 "-o", str(tmp_path / "ratio.mp4"), "--max-repair-ratio", "0.001"]) == 4
    captured = capsys.readouterr()
    assert "unsupported codec" in captured.err
    assert "repair limit" in captured.err

