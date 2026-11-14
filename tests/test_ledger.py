import csv
import sqlite3

import numpy as np
import pytest

from audioscan import ledger
from audioscan.ledger import (
    LedgerBusyError,
    complete_job,
    connect,
    enqueue,
    init_db,
    mark_running,
    pending_jobs,
    reset_running_jobs,
)
from audioscan.report import export_cuts, summary, write_reports
from audioscan.synth import _pack_wav


def _fixture_dir(tmp_path):
    source = tmp_path / "fixtures"
    source.mkdir()
    tone = np.full((2000, 1), 0.2, dtype=np.float32)
    (source / "a.wav").write_bytes(_pack_wav(tone, 48000, "float32"))
    (source / "b.wav").write_bytes(_pack_wav(-tone, 48000, "pcm16"))
    (source / "a-copy.wav").write_bytes((source / "a.wav").read_bytes())
    return source


def test_enqueue_is_idempotent_and_deduplicates_content(tmp_path):
    conn = connect(tmp_path / "ledger.db")
    init_db(conn)
    source = _fixture_dir(tmp_path)
    first = enqueue(conn, source)
    second = enqueue(conn, source)
    assert (first.scanned, first.queued, first.skipped) == (3, 2, 1)
    assert (second.scanned, second.queued, second.skipped) == (3, 0, 3)
    jobs = pending_jobs(conn)
    assert len(jobs) == 2
    mark_running(conn, jobs[0].job_id)
    assert reset_running_jobs(conn) == 1
    assert len(pending_jobs(conn)) == 2
    conn.close()


def test_complete_job_report_and_export(tmp_path):
    conn = connect(tmp_path / "ledger.db")
    init_db(conn)
    source = _fixture_dir(tmp_path)
    enqueue(conn, source)
    job = pending_jobs(conn)[0]
    complete_job(conn, job.job_id, 48000, 1, 2000, "float32", "mono", [(100, 900, 20, 30)])
    data = summary(conn)
    assert data["files"] == 2
    assert data["ok"] == 1
    assert data["segments"] == 1
    assert data["silence_total_samples"] == 1200
    mode = conn.execute("SELECT encoding, channel_mode FROM files WHERE id=?", (job.file_id,)).fetchone()
    assert tuple(mode) == ("float32", "mono")
    reports = write_reports(conn, tmp_path / "reports")
    assert reports["ok"] == 1
    cut_path = export_cuts(conn, tmp_path / "cuts", "csv")
    with cut_path.open(newline="", encoding="utf-8") as handle:
        reader = csv.DictReader(handle)
        assert reader.fieldnames == ["file", "index", "start_sample", "end_sample", "start_ms", "end_ms"]
        rows = list(reader)
    assert len(rows) == 1
    assert rows[0]["start_sample"] == "100"
    conn.close()


def test_write_retry_retries_five_times_then_succeeds(tmp_path, monkeypatch):
    conn = connect(tmp_path / "ledger.db")
    init_db(conn)
    monkeypatch.setattr(ledger.time, "sleep", lambda _: None)
    calls = {"count": 0}

    def operation():
        calls["count"] += 1
        if calls["count"] < 3:
            raise sqlite3.OperationalError("database is locked")
        return "ok"

    assert ledger.write_retry(conn, operation) == "ok"
    assert calls["count"] == 3
    conn.close()


def test_write_retry_exhaustion_raises_busy(tmp_path, monkeypatch):
    conn = connect(tmp_path / "ledger.db")
    init_db(conn)
    monkeypatch.setattr(ledger.time, "sleep", lambda _: None)
    calls = {"count": 0}

    def operation():
        calls["count"] += 1
        raise sqlite3.OperationalError("database is busy")

    with pytest.raises(LedgerBusyError):
        ledger.write_retry(conn, operation)
    assert calls["count"] == 5
    conn.close()
