import csv
import json

import numpy as np
import pytest

from audioscan import cli, ledger
from audioscan.ledger import LedgerBusyError
from audioscan.synth import _pack_wav, generate_fixture


def _last_json(capsys):
    return json.loads(capsys.readouterr().out.strip().splitlines()[-1])


def test_full_acceptance_flow(tmp_path, capsys):
    fixtures = tmp_path / "fixtures"
    database = tmp_path / "ledger.db"
    reports = tmp_path / "reports"
    cuts = tmp_path / "cuts"
    assert cli.main(["synth", "--out", str(fixtures), "--files", "12", "--seed", "7"]) == 0
    capsys.readouterr()

    assert cli.main(["analyze", str(fixtures / "fx_03.wav"), "--json"]) == 0
    analyzed = _last_json(capsys)
    assert 3 <= analyzed["segment_count"] <= 8
    assert analyzed["sample_rate"] == 96000
    assert analyzed["total_samples"] > 0
    assert analyzed["silence_total_samples"] > 0
    for segment in analyzed["segments"]:
        assert 0 <= segment["fade_in_ms"] <= 600
        assert segment["start_sample"] < segment["end_sample"]

    assert cli.main(["batch", "enqueue", str(fixtures), "--db", str(database)]) == 0
    assert cli.main([
        "batch", "run", "--db", str(database), "--workers", "2", "--report-out", str(reports)
    ]) == 0
    run_summary = _last_json(capsys)
    assert run_summary["ok"] == 12 and run_summary["failed"] == 0

    assert cli.main(["export", "--out", str(cuts), "--format", "csv", "--db", str(database)]) == 0
    capsys.readouterr()
    with (cuts / "cuts.csv").open(newline="", encoding="utf-8") as handle:
        reader = csv.DictReader(handle)
        assert reader.fieldnames == ["file", "index", "start_sample", "end_sample", "start_ms", "end_ms"]
        rows = list(reader)
    assert len(rows) == 60

    assert cli.main(["report", "--summary", "--json", "--db", str(database), "--out", str(reports)]) == 0
    summary = _last_json(capsys)
    assert summary["files"] == 12 and summary["ok"] == 12 and summary["failed"] == 0
    assert 50 <= summary["segments"] <= 70
    assert 10000 <= summary["silence_total_ms"] <= 99999

    assert cli.main(["selfcheck", "--seed", "7"]) == 0
    assert capsys.readouterr().out.strip() == "matched=12/12"


def test_decode_failure_prints_json_and_exit_3(tmp_path, capsys):
    bad = tmp_path / "broken.wav"
    bad.write_bytes(b"not a wav")
    assert cli.main(["analyze", str(bad), "--json"]) == 3
    assert _last_json(capsys) == {"error": "riff_invalid"}


def test_shorter_than_one_frame_exit_3(tmp_path, capsys):
    short = tmp_path / "short.wav"
    short.write_bytes(_pack_wav(np.zeros((10, 1), np.float32), 48000, "float32"))
    assert cli.main(["analyze", str(short), "--json"]) == 3
    assert _last_json(capsys) == {"error": "riff_invalid"}


def test_partial_batch_failure_exit_5_and_report_written(tmp_path, capsys):
    fixtures = tmp_path / "fixtures"
    fixtures.mkdir()
    good = generate_fixture(fixtures / "good.wav", 7, 1)
    (fixtures / "bad.wav").write_bytes(b"RIFF broken")
    database = tmp_path / "ledger.db"
    reports = tmp_path / "reports"
    assert cli.main(["batch", "enqueue", str(fixtures), "--db", str(database)]) == 0
    capsys.readouterr()
    assert cli.main([
        "batch", "run", "--db", str(database), "--workers", "1", "--report-out", str(reports)
    ]) == 5
    result = _last_json(capsys)
    assert result["ok"] == 1 and result["failed"] == 1
    assert (reports / "summary.json").exists()
    on_disk = json.loads((reports / "summary.json").read_text(encoding="utf-8"))
    assert on_disk["failed"] == 1
    assert good.segment_count > 0


def test_argument_error_exit_2(tmp_path):
    with pytest.raises(SystemExit) as exc:
        cli.main(["synth", "--out", str(tmp_path), "--files", "0"])
    assert exc.value.code == 2


def test_ledger_busy_exit_4(tmp_path, capsys, monkeypatch):
    fixtures = tmp_path / "fixtures"
    fixtures.mkdir()
    (fixtures / "x.wav").write_bytes(b"x")

    def busy(*args, **kwargs):
        raise LedgerBusyError("locked")

    monkeypatch.setattr(ledger, "write_retry", busy)
    assert cli.main(["batch", "enqueue", str(fixtures), "--db", str(tmp_path / "db")]) == 4
    assert _last_json(capsys) == {"error": "ledger_busy"}


def test_selfcheck_mismatch_exit_6(monkeypatch, capsys):
    monkeypatch.setattr(
        cli, "_selfcheck_one", lambda fixture, options: (False, {"file": "x", "matched": False})
    )
    assert cli.main(["selfcheck", "--seed", "7", "--files", "1"]) == 6
    assert capsys.readouterr().out.splitlines()[0] == "matched=0/1"
