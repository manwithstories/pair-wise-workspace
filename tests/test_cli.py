import json
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))
from gen_fixtures import build_fixtures  # noqa: E402

from audiocut import wavio  # noqa: E402
from audiocut.cli import main  # noqa: E402


@pytest.fixture(scope="module")
def workspace(tmp_path_factory):
    root = tmp_path_factory.mktemp("ws")
    fixtures = root / "fixtures"
    fixtures.mkdir()
    truth = build_fixtures(fixtures, 20261114)
    (root / "profiles").mkdir()
    (root / "build").mkdir()
    assert main(["calibrate", "--input", str(fixtures / "calib_a.wav"),
                 "--out", str(root / "profiles/default.json"),
                 "--frame-ms", "20", "--hop-ms", "10"]) == 0
    return root, fixtures, truth


def test_scan_report_export(workspace, capsys):
    root, fixtures, truth = workspace
    db = root / "build/index.sqlite"
    capsys.readouterr()
    assert main(["scan", "--input", str(fixtures), "--db", str(db),
                 "--profile", str(root / "profiles/default.json")]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["ok"] == len(truth) and out["failed"] == 0

    assert main(["report", "--db", str(db), "--json"]) == 0
    report = json.loads(capsys.readouterr().out)
    assert report["files"] == len(truth)
    assert report["segments"] >= 80
    assert report["total_silence_ms"] > 60_000
    by_path = {d["path"]: d for d in report["detail"]}
    for name, t in truth.items():
        detail = by_path[str(fixtures / name)]
        exp = [(s["start_sample"], s["end_sample"]) for s in t["segments"]]
        merged = []
        for s, e in exp:
            if merged and s - merged[-1][1] < 0.18 * 48000:
                merged[-1] = (merged[-1][0], e)
            else:
                merged.append((s, e))
        got = [(s["start_sample"], s["end_sample"]) for s in detail["segments"]]
        assert len(got) == len(merged), name
        for (gs, ge), (es, ee) in zip(got, merged):
            assert abs(gs - es) <= 0.2 * 48000, name
            assert abs(ge - ee) <= 0.2 * 48000, name

    labels = root / "build/labels.jsonl"
    assert main(["export", "--db", str(db),
                 "--profile", str(root / "profiles/default.json"),
                 "--out", str(labels)]) == 0
    lines = labels.read_text().strip().split("\n")
    assert len(lines) == report["segments"]
    first = json.loads(lines[0])
    assert {"path", "segment_index", "start_sample", "end_sample",
            "start_ms", "end_ms"} <= first.keys()


def test_scan_idempotent_rescan(workspace, capsys):
    root, fixtures, _truth = workspace
    db = root / "build/index.sqlite"
    capsys.readouterr()
    assert main(["scan", "--input", str(fixtures), "--db", str(db),
                 "--profile", str(root / "profiles/default.json")]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["skipped"] == out["ok"] + out["skipped"] > 0
    assert out["failed"] == 0
    conn = sqlite3.connect(db)
    assert conn.execute("SELECT COUNT(*) FROM files").fetchone()[0] == 13
    conn.close()


def test_profile_mismatch_exit4(workspace, tmp_path, capsys):
    root, _fixtures, _truth = workspace
    wavio.write_wav(tmp_path / "m44.wav", [0.0] * 4410, 44100)
    capsys.readouterr()
    code = main(["scan", "--input", str(tmp_path), "--db",
                 str(root / "build/m.sqlite"),
                 "--profile", str(root / "profiles/default.json")])
    assert code == 4


def test_unsupported_format_exit3(workspace, tmp_path, capsys):
    root, _f, _t = workspace
    import struct
    fmt = struct.pack("<HHIIHH", 3, 1, 48000, 192000, 4, 32)
    body = b"fmt " + struct.pack("<I", 16) + fmt + b"data" + struct.pack("<I", 64) + b"\x00" * 64
    (tmp_path / "float.wav").write_bytes(
        b"RIFF" + struct.pack("<I", 4 + len(body)) + b"WAVE" + body)
    capsys.readouterr()
    code = main(["scan", "--input", str(tmp_path), "--db",
                 str(root / "build/f.sqlite"),
                 "--profile", str(root / "profiles/default.json")])
    assert code == 5  # per-file failure isolated, batch reports partial
    out = json.loads(capsys.readouterr().out)
    assert out["failed"] == 1


def test_bad_profile_exit4(workspace, tmp_path, capsys):
    root, _f, _t = workspace
    bad = tmp_path / "bad.json"
    bad.write_text('{"version": 99}')
    capsys.readouterr()
    assert main(["scan", "--input", str(root / "fixtures"), "--db",
                 str(root / "build/x.sqlite"), "--profile", str(bad)]) == 4


def test_out_dir_confinement(workspace, tmp_path, capsys):
    root, fixtures, _t = workspace
    capsys.readouterr()
    code = main(["calibrate", "--input", str(fixtures / "calib_a.wav"),
                 "--out", str(tmp_path / "escape.json"),
                 "--out-dir", str(root / "profiles")])
    assert code == 2
    link = root / "profiles" / "link.json"
    link.symlink_to(tmp_path / "target.json")
    code = main(["calibrate", "--input", str(fixtures / "calib_a.wav"),
                 "--out", str(link)])
    assert code == 2


def test_usage_error_exit2(capsys):
    with pytest.raises(SystemExit) as exc:
        main(["scan"])
    assert exc.value.code == 2
